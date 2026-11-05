package operations

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/remoterepo"
	"github.com/feder-cr/git-ai/internal/gitaly/hook/updateref"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

//nolint:revive // This is unintentionally missing documentation.
func (s *Server) UserRevert(ctx context.Context, req *gitalypb.UserRevertRequest) (*gitalypb.UserRevertResponse, error) {
	if err := validateCherryPickOrRevertRequest(ctx, s.locator, req); err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}

	quarantineDir, quarantineRepo, cleanup, err := s.quarantinedRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	defer cleanup()

	startRevision, err := s.fetchStartRevision(ctx, quarantineRepo, req)
	if err != nil {
		return nil, err
	}

	repoHadBranches, err := quarantineRepo.HasBranches(ctx)
	if err != nil {
		return nil, structerr.NewInternal("has branches: %w", err)
	}

	committerSignature, err := git.SignatureFromRequest(req)
	if err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}

	var newrev git.ObjectID

	oursCommit, err := quarantineRepo.ReadCommit(ctx, startRevision.Revision())
	if err != nil {
		if errors.Is(err, localrepo.ErrObjectNotFound) {
			return nil, structerr.NewNotFound("ours commit lookup: commit not found").
				WithMetadata("revision", startRevision)
		}

		return nil, structerr.NewInternal("read commit: %w", err)
	}
	revertCommit, err := quarantineRepo.ReadCommit(ctx, git.Revision(req.GetCommit().GetId()))
	if err != nil {
		if errors.Is(err, localrepo.ErrObjectNotFound) {
			return nil, structerr.NewNotFound("revert commit lookup: commit not found").
				WithMetadata("revision", req.GetCommit().GetId())
		}

		return nil, structerr.NewInternal("read commit: %w", err)
	}

	var theirs git.ObjectID
	if len(revertCommit.GetParentIds()) > 0 {
		// Use the first parent as `theirs` to implement mainline = 1.
		theirs = git.ObjectID(revertCommit.GetParentIds()[0])
	} else {
		// It is a root commit, use empty tree oid as `theirs`.
		hash, err := quarantineRepo.ObjectHash(ctx)
		if err != nil {
			return nil, structerr.NewInternal("detecting object hash: %w", err)
		}
		theirs = hash.EmptyTreeOID
	}

	// We "merge" in the "changes" from parent to child, which would apply the "opposite"
	// patch to "ours", thus reverting the commit.
	treeOID, err := quarantineRepo.MergeTree(
		ctx,
		oursCommit.GetId(),
		theirs.String(),
		localrepo.WithMergeBase(git.Revision(revertCommit.GetId())),
		localrepo.WithConflictingFileNamesOnly(),
	)
	if err != nil {
		var conflictErr *localrepo.MergeTreeConflictError
		if errors.As(err, &conflictErr) {
			return nil, structerr.NewFailedPrecondition("revert: there are conflicting files").WithDetail(
				&gitalypb.UserRevertError{
					Error: &gitalypb.UserRevertError_MergeConflict{
						MergeConflict: &gitalypb.MergeConflictError{
							ConflictingFiles: conflictingFileNames(conflictErr),
						},
					},
				})
		}

		return nil, structerr.NewInternal("merge-tree: %w", err)
	}

	if oursCommit.GetTreeId() == treeOID.String() {
		return nil, structerr.NewFailedPrecondition("revert: could not apply because the result was empty").WithDetail(
			&gitalypb.UserRevertError{
				Error: &gitalypb.UserRevertError_ChangesAlreadyApplied{
					ChangesAlreadyApplied: &gitalypb.ChangesAlreadyAppliedError{},
				},
			})
	}

	newrev, err = quarantineRepo.WriteCommit(
		ctx,
		localrepo.WriteCommitConfig{
			TreeID:         treeOID,
			Message:        string(req.GetMessage()),
			Parents:        []git.ObjectID{startRevision},
			AuthorName:     committerSignature.Name,
			AuthorEmail:    committerSignature.Email,
			AuthorDate:     committerSignature.When,
			CommitterName:  committerSignature.Name,
			CommitterEmail: committerSignature.Email,
			CommitterDate:  committerSignature.When,
			GitConfig:      s.gitConfig,
			Sign:           req.GetSign(),
		},
	)
	if err != nil {
		return nil, structerr.NewInternal("write commit: %w", err)
	}

	referenceName := git.NewReferenceNameFromBranchName(string(req.GetBranchName()))
	branchCreated := false
	var oldrev git.ObjectID

	objectHash, err := quarantineRepo.ObjectHash(ctx)
	if err != nil {
		return nil, structerr.NewInternal("detecting object hash: %w", err)
	}

	if expectedOldOID := req.GetExpectedOldOid(); expectedOldOID != "" {
		oldrev, err = objectHash.FromHex(expectedOldOID)
		if err != nil {
			return nil, structerr.NewInvalidArgument("invalid expected old object ID: %w", err).WithMetadata("old_object_id", expectedOldOID)
		}

		oldrev, err = quarantineRepo.ResolveRevision(
			ctx, git.Revision(fmt.Sprintf("%s^{object}", oldrev)),
		)
		if err != nil {
			return nil, structerr.NewInvalidArgument("cannot resolve expected old object ID: %w", err).
				WithMetadata("old_object_id", expectedOldOID)
		}
	} else {
		oldrev, err = s.resolveBranchCommitExactly(ctx, quarantineRepo, referenceName)
		if errors.Is(err, git.ErrReferenceNotFound) {
			branchCreated = true
			oldrev = objectHash.ZeroOID
		} else if err != nil {
			return nil, structerr.NewInvalidArgument("resolve ref: %w", err)
		}
	}

	if req.GetDryRun() {
		newrev = startRevision
	}

	if !branchCreated {
		ancestor, err := quarantineRepo.IsAncestor(ctx, oldrev.Revision(), newrev.Revision())
		if err != nil {
			return nil, structerr.NewInternal("checking for ancestry: %w", err)
		}
		if !ancestor {
			return nil, structerr.NewFailedPrecondition("revert: branch diverged").WithDetail(
				&gitalypb.UserRevertError{
					Error: &gitalypb.UserRevertError_NotAncestor{
						NotAncestor: &gitalypb.NotAncestorError{
							ParentRevision: []byte(oldrev.Revision()),
							ChildRevision:  []byte(newrev.Revision()),
						},
					},
				})
		}
	}

	if err := s.updateReferenceWithHooks(ctx, req.GetRepository(), req.GetUser(), quarantineDir, referenceName, newrev, oldrev); err != nil {
		var customHookErr updateref.CustomHookError
		if errors.As(err, &customHookErr) {
			return nil, structerr.NewPermissionDenied("revert: custom hook error").WithDetail(
				&gitalypb.UserRevertError{
					Error: &gitalypb.UserRevertError_CustomHook{
						CustomHook: customHookErr.Proto(),
					},
				})
		}

		return nil, fmt.Errorf("update reference with hooks: %w", err)
	}

	return &gitalypb.UserRevertResponse{
		BranchUpdate: &gitalypb.OperationBranchUpdate{
			CommitId:      newrev.String(),
			BranchCreated: branchCreated,
			RepoCreated:   !repoHadBranches,
		},
	}, nil
}

// resolveStartBranch resolves the start branch name of a request, a revision, to its commit, as
// git resolves it with one deviation. objectHash is the object format of the repository the start
// branch is read from; read returns the value of a reference there, read by its exact name
// (git.ErrReferenceNotFound when it does not exist, a dangling symbolic reference included), and
// resolve is git's resolution of a revision there.
//
// The revision is split into the name it starts with and its suffix (~1, ^{}, @{1}, :path). git
// takes a full object ID (in either case) for the object; a root reference such as HEAD or
// FETCH_HEAD, when it exists, from the repository's directory (git reads other upper-case files
// there too, such as AUTO_MERGE, but a push cannot write them); and any other name x by its DWIM
// rules, the first that exists of refs/x, refs/tags/x, refs/heads/x, refs/remotes/x and
// refs/remotes/x/HEAD. The one deviation: when that is a reference with the same name as the
// branch, the tag refs/tags/x or refs/x, and the branch refs/heads/x exists, the branch is taken. An
// existing branch of the exact name beats a non-branch reference of the same name, such as a tag an
// agent pushed to shadow it. A name that starts with a namespace git itself prefixes (heads/, tags/,
// remotes/) names it, and follows git, as everything else does. When no reference is named x,
// git's resolution answers, by design: an abbreviated SHA or another revision keeps working as a
// start point, as before. A full reference name (refs/...) is read exactly as given, with no
// fallback: a missing refs/heads/x does not resolve through refs/refs/heads/x or another decoy.
//
// A start point is a commit, never a path: <revision>:path is refused, and so is :/<text>, a search
// of the commit messages, by choice, as a start point that depends on which message matches is
// fragile. Through a start repository, FindCommit refuses revisions that hold whitespace or a colon,
// so main@{2024-01-01 10:00:00} or main^{/fix: x} work locally only.
//
// The chosen reference is read once, to its value, and the suffix applies to that object ID: no
// rule resolves the name again, so a reference that is dangling, or that goes away meanwhile, never
// yields a decoy. A reflog suffix (@{n}) needs the name: the reference must have a reflog of its
// own (reflogExists), since git would otherwise read the reflog of a decoy such as
// refs/refs/heads/x; it is then resolved by the full name, and the reference must still have the
// value read, else the request fails with errStartReferenceChanged, which a retry may not meet.
func resolveStartBranch(
	name string,
	objectHash git.ObjectHash,
	read func(git.ReferenceName) (git.ObjectID, error),
	reflogExists func(git.ReferenceName) (bool, error),
	resolve func(git.Revision) (git.ObjectID, error),
) (git.ObjectID, error) {
	base, suffix := splitRevisionSuffix(name)
	// A start point is a commit, and <revision>:path names a blob or a tree; git would parse a
	// ^{commit} after it as part of the path.
	if namesPath(suffix) {
		return "", errStartPointPath
	}
	// A revision that starts with its suffix (@{-1}) names no reference.
	if base == "" || isFullObjectID(base, objectHash) {
		return resolve(git.Revision(name + "^{commit}"))
	}

	reference, value, err := startReference(base, read)
	switch {
	case err != nil:
		return "", err
	case reference == "" && strings.HasPrefix(base, "refs/"):
		return "", git.ErrReferenceNotFound
	case reference == "":
		return resolve(git.Revision(name + "^{commit}"))
	case !strings.Contains(suffix, "@{"):
		return resolve(git.Revision(value.String() + suffix + "^{commit}"))
	}

	hasReflog, err := reflogExists(reference)
	if err != nil {
		return "", err
	}
	if !hasReflog {
		return "", fmt.Errorf("reference %q has no reflog: %w", reference, git.ErrReferenceNotFound)
	}
	commit, err := resolve(git.Revision(reference.String() + suffix + "^{commit}"))
	if err != nil {
		return "", err
	}
	if again, err := read(reference); err != nil || again != value {
		return "", fmt.Errorf("reference %q: %w", reference, errStartReferenceChanged)
	}
	return commit, nil
}

// resolveStartBranchLocally is resolveStartBranch in repo: the one rule by which a request names the
// commit that its work starts from, for the start points of RPCs in every field that holds one.
func resolveStartBranchLocally(ctx context.Context, repo *localrepo.Repo, name string) (git.ObjectID, error) {
	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return "", structerr.NewInternal("detecting object hash: %w", err)
	}

	return resolveStartBranch(name, objectHash,
		func(name git.ReferenceName) (git.ObjectID, error) {
			return repo.ResolveReferenceExactly(ctx, name)
		},
		func(name git.ReferenceName) (bool, error) {
			return repo.ReflogExists(ctx, name)
		},
		func(revision git.Revision) (git.ObjectID, error) {
			return repo.ResolveRevision(ctx, revision)
		},
	)
}

// errStartPointPath is the error of a start point that names a path (<revision>:path), not a commit.
var errStartPointPath = errors.New("the start point names a path, not a commit")

// namesPath reports whether the suffix of a revision names a path: a colon outside braces, where
// @{<date>} and ^{/<text>} may hold one.
func namesPath(suffix string) bool {
	depth := 0
	for _, c := range suffix {
		switch c {
		case '{':
			depth++
		case '}':
			if depth > 0 {
				depth--
			}
		case ':':
			if depth == 0 {
				return true
			}
		}
	}
	return false
}

// errStartReferenceChanged is the error of a start reference that changed while its reflog was read.
var errStartReferenceChanged = errors.New("the reference changed while its reflog was read")

// startRefError returns the error of a start branch that resolveStartBranch failed to resolve: a
// start reference that changed meanwhile is a failed precondition, which a retry may not meet, and
// anything else an invalid argument.
func startRefError(err error) error {
	if errors.Is(err, errStartReferenceChanged) {
		return structerr.NewFailedPrecondition("resolve start ref: %w", err)
	}
	return structerr.NewInvalidArgument("resolve start ref: %w", err)
}

// startReference returns the reference that git resolves the name to, with the deviation of
// resolveStartBranch, and its value; an empty name when there is none.
func startReference(name string, read func(git.ReferenceName) (git.ObjectID, error)) (git.ReferenceName, git.ObjectID, error) {
	if strings.HasPrefix(name, "refs/") {
		value, err := read(git.ReferenceName(name))
		if errors.Is(err, git.ErrReferenceNotFound) {
			return "", "", nil
		}
		return git.ReferenceName(name), value, err
	}

	branch := git.ReferenceName("refs/heads/" + name)
	other := git.ReferenceName("refs/" + name)
	tag := git.ReferenceName("refs/tags/" + name)
	candidates := []git.ReferenceName{other, tag, branch, git.ReferenceName("refs/remotes/" + name), git.ReferenceName("refs/remotes/" + name + "/HEAD")}
	if isRootReference(name) {
		candidates = append([]git.ReferenceName{git.ReferenceName(name)}, candidates...)
	}
	namesNamespace := strings.HasPrefix(name, "heads/") || strings.HasPrefix(name, "tags/") || strings.HasPrefix(name, "remotes/")

	for _, candidate := range candidates {
		value, err := read(candidate)
		if errors.Is(err, git.ErrReferenceNotFound) {
			continue
		}
		// A reference that does not point to a commit exists all the same, as it does when its
		// value is read locally: the branch may still beat it, and it fails the request only
		// when it is the reference chosen.
		if err != nil && !errors.Is(err, remoterepo.ErrNotACommit) {
			return "", "", err
		}
		if !namesNamespace && (candidate == other || candidate == tag) {
			branchValue, branchErr := read(branch)
			if !errors.Is(branchErr, git.ErrReferenceNotFound) {
				return branch, branchValue, branchErr
			}
		}
		return candidate, value, err
	}
	return "", "", nil
}

// isRootReference reports whether name has the syntax of a root reference, which git reads from
// the repository's directory before any DWIM rule when it exists: HEAD, or upper-case letters and
// underscores ending in _HEAD (FETCH_HEAD, ORIG_HEAD, MERGE_HEAD, ...).
func isRootReference(name string) bool {
	if name == "HEAD" {
		return true
	}
	if !strings.HasSuffix(name, "_HEAD") {
		return false
	}
	for _, c := range name {
		if (c < 'A' || c > 'Z') && c != '_' {
			return false
		}
	}
	return true
}

// isFullObjectID reports whether name is a full object ID of objectHash's format, in either case,
// which git takes for the object.
func isFullObjectID(name string, objectHash git.ObjectHash) bool {
	return objectHash.ValidateHex(strings.ToLower(name)) == nil
}

// splitRevisionSuffix splits a revision into the name it starts with and the suffix that applies to
// it (~<n>, ^<n>, ^{<type>}, @{<n>}, :<path>). A reference name never holds ~, ^, : or @{ (git
// check-ref-format), so the name ends at the first of them.
func splitRevisionSuffix(revision string) (string, string) {
	end := len(revision)
	for _, marker := range []string{"~", "^", ":", "@{"} {
		if i := strings.Index(revision, marker); i >= 0 && i < end {
			end = i
		}
	}
	return revision[:end], revision[end:]
}

type requestFetchingStartRevision interface {
	GetBranchName() []byte
	GetStartRepository() *gitalypb.Repository
	GetStartBranchName() []byte
}

func (s *Server) fetchStartRevision(
	ctx context.Context,
	localRepo *localrepo.Repo,
	req requestFetchingStartRevision,
) (git.ObjectID, error) {
	startBranchName := req.GetStartBranchName()
	if len(startBranchName) == 0 {
		startBranchName = req.GetBranchName()
	}

	if req.GetStartRepository() == nil {
		startRevision, err := resolveStartBranchLocally(ctx, localRepo, string(startBranchName))
		if err != nil {
			return "", startRefError(err)
		}
		return startRevision, nil
	}

	remoteRepo, err := remoterepo.New(ctx, req.GetStartRepository(), s.conns)
	if err != nil {
		return "", structerr.NewInternal("%w", err)
	}
	objectHash, err := remoteRepo.ObjectHash(ctx)
	if err != nil {
		return "", structerr.NewInternal("detecting object hash: %w", err)
	}

	startRevision, err := resolveStartBranch(string(startBranchName), objectHash,
		func(name git.ReferenceName) (git.ObjectID, error) {
			return remoteRepo.ReadReference(ctx, name)
		},
		// No RPC tells whether a reference of the start repository has a reflog of its own, so a
		// reflog entry of one is not taken there: git could read a decoy's reflog for it.
		func(git.ReferenceName) (bool, error) {
			return false, nil
		},
		func(revision git.Revision) (git.ObjectID, error) {
			return remoteRepo.ResolveRevision(ctx, revision)
		},
	)
	if err != nil {
		return "", startRefError(err)
	}

	_, err = localRepo.ResolveRevision(ctx, startRevision.Revision()+"^{commit}")
	if errors.Is(err, git.ErrReferenceNotFound) {
		if err := localRepo.FetchInternal(
			ctx,
			req.GetStartRepository(),
			[]string{startRevision.String()},
			localrepo.FetchOpts{Tags: localrepo.FetchOptsTagsNone},
		); err != nil {
			return "", structerr.NewInternal("fetch start: %w", err)
		}
	} else if err != nil {
		return "", structerr.NewInvalidArgument("resolve start: %w", err)
	}

	return startRevision, nil
}
