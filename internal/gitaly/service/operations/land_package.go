package operations

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/graft/merge3"
	"github.com/feder-cr/git-ai/internal/graft/resolver"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// This file builds the conflict package of a land that the server did not resolve (LandConflict,
// PLAN10 3.3): every conflicting file with its sides, its conflicting hunks and what the server did
// with it, so that the agent can resolve the conflict and land again with a LandResolution.

const (
	// landPackageMessageBytes bounds each commit message of the package.
	landPackageMessageBytes = 4 << 10
	// landPackageReasonBytes bounds every reason and merge message of the package.
	landPackageReasonBytes = 512
	// landMaxPackageFiles bounds the files of a package. A merge with more conflicting files gets
	// a package without files whose reason says so: it cannot be resolved through a land.
	landMaxPackageFiles = 1000
	// landMaxPackageMessages bounds the merge messages of a package.
	landMaxPackageMessages = 1000
	// landMaxPackageBytes bounds the encoded package, which travels as a gRPC error detail. A
	// package over it loses its hunks; one still over it loses its files, and its reason says so.
	landMaxPackageBytes = 4 << 20
	// landMaxHunksPerFile bounds the hunks of one file.
	landMaxHunksPerFile = 256
	// landMaxPackageReadBytes bounds the bytes of the sides that a package reads for its hunks,
	// beyond those the resolver's classification read already.
	landMaxPackageReadBytes = 64 << 20
	// landMaxPackageCells bounds the line-diff work of the hunks of a package (merge3.Cost).
	landMaxPackageCells = 64 << 20
)

// landModeGitlink is the mode of a submodule entry.
const landModeGitlink = "160000"

// Reasons of hunks_omitted and of an unavailable side.
const (
	landHunksTooLarge    = "a side is larger than max_file_bytes, so no diff is computed"
	landHunksBinary      = "a side is binary, which has no line hunks"
	landHunksDiffCost    = "the diff of the sides is over the cost bound of one file"
	landHunksBudget      = "the package's bound on the sides it reads and diffs is used up"
	landHunksPackage     = "the package would be over 4 MiB with its hunks"
	landHunksUnavailable = "a side is unavailable, so the hunks would name lines the agent cannot read"
	landSideGitlink      = "the side is a submodule (a gitlink), which names a commit, not a blob"
	landSideUnavailable  = "the merge wrote this blob for the conflict (such as the content merge of a rename), and the repository does not keep it after the land fails"
)

// Reasons of the files the server does not send to the resolver, besides the content rules of
// resolver.SendRules.
const (
	landNotSentTrailerUnsafe = "the path holds a line break or another non-printable character, which a commit trailer cannot carry"
	landNotSentInvalidUTF8   = "the path is not valid UTF-8, which the resolver protocol cannot carry"
	landNotSentDenied        = "the path may not be sent to the resolver (paths_allow, paths_deny)"
	landNotSentNotAFile      = "a side is not a file (a submodule or a directory)"
	landNotSentNoSide        = "the path is on neither side of the merge (a file/directory conflict)"
	landNotSentNoBaseOneSide = "the path has no base and a missing side, which no merge produces without a rename"
)

// landFileState is a conflicting file of a land and what the server did with it.
type landFileState struct {
	conflict landFileConflict
	kind     string
	// sides are the contents read for the resolver and the hunks (base, ours, theirs); nil for an
	// absent side or one that was not read.
	sides   [3][]byte
	read    bool
	outcome gitalypb.LandConflictFile_Outcome
	reason  string
	// syntaxUnchecked is set when a check accepted the file without a syntax verdict.
	syntaxUnchecked bool
	// hunks are the file's hunks in the package, and hunksOmitted why they are absent or cut.
	hunks        []*gitalypb.LandConflictHunk
	hunksOmitted string
	// unavailable is set when a present side's blob is not kept in the repository.
	unavailable bool
	// proposal is the resolver's resolution of the file in a land that did not happen, kept under
	// the proposal reference of the package (landWriteProposals).
	proposal *gitalypb.LandConflictProposal
}

// sent reports whether the file is sent to the resolver.
func (f *landFileState) sent() bool {
	return f.outcome == gitalypb.LandConflictFile_OUTCOME_UNSPECIFIED
}

// textRead reports whether the sides were read and are text, so that they have line hunks.
func (f *landFileState) textRead() bool {
	if !f.read {
		return false
	}
	for _, s := range f.sides {
		if bytes.IndexByte(s, 0) >= 0 {
			return false
		}
	}
	return true
}

// notSent marks the file as not sent, with its reason.
func (f *landFileState) notSent(reason string) {
	f.outcome = gitalypb.LandConflictFile_OUTCOME_NOT_SENT
	f.reason = reason
}

// landUnresolvedError is a land whose conflicts the server did not resolve. It carries what the
// conflict package is built from; the package itself, which reads sides for its hunks, is built
// only when the caller attaches it to the merge conflict error (landWithPackage).
type landUnresolvedError struct {
	listing landListing
	// reason is the package's reason: set when the server did not try to resolve at all.
	reason string
	cause  string
	// err is the error of the resolver or of the check, when one caused the outcome, so that
	// errors.Is and errors.As still see it.
	err error
}

func (e *landUnresolvedError) Error() string {
	return "conflicts not resolved: " + e.cause
}

// Unwrap returns the error of the resolver or of the check, if any.
func (e *landUnresolvedError) Unwrap() error {
	return e.err
}

// landWithPackage adds the conflict package of resolveErr, when it carries one, to the merge
// conflict error of the land as a second detail, after UserLandError. Clients that know only
// UserLandError see the error unchanged. A package that cannot be built (a side that cannot be
// read) is an internal error: the merge conflict alone would hide it. An agent the policy does not
// let update the branch gets the policy's refusal, not a package.
func landWithPackage(ctx context.Context, mergeConflict, resolveErr error) error {
	var denied *landPolicyDeniedError
	if errors.As(resolveErr, &denied) {
		return denied.err
	}
	var unresolved *landUnresolvedError
	var se structerr.Error
	if !errors.As(resolveErr, &unresolved) || !errors.As(mergeConflict, &se) {
		return mergeConflict
	}
	pkg, err := unresolved.listing.conflictPackage(ctx, unresolved.reason)
	if err != nil {
		return structerr.NewInternal("building the conflict package: %w", err)
	}
	return se.WithDetail(pkg)
}

// landConflictWithoutResolution adds a package to an ordinary land's conflict, without calling a
// resolver. It runs after the lane has answered, in a quarantine of its own, against the exact tip
// and commit the failed attempt reported: the branch may already have moved again.
func (s *Server) landConflictWithoutResolution(ctx context.Context, in *gitalypb.UserLandRequest, mergeConflict error) error {
	var commits []string
	for _, detail := range status.Convert(mergeConflict).Details() {
		if landErr, ok := detail.(*gitalypb.UserLandError); ok {
			commits = landErr.GetMergeConflict().GetConflictingCommitIds()
			if len(commits) == 2 {
				break
			}
		}
	}
	if len(commits) != 2 {
		return mergeConflict
	}
	tip, commitID := git.ObjectID(commits[0]), git.ObjectID(commits[1])
	reference := git.NewReferenceNameFromBranchName(string(in.GetBranch()))
	if err := hook.CheckAgentPolicy(ctx, s.graftInForce(ctx), fmt.Sprintf("%s %s %s\n", tip, commitID, reference)); err != nil {
		return err
	}
	_, repo, cleanup, err := s.quarantinedRepo(ctx, in.GetRepository())
	if err != nil {
		return err
	}
	defer cleanup()

	l, err := landListConflicts(ctx, repo, tip, commitID, s.resolveCfg.MaxFileBytesOrDefault())
	if errors.Is(err, errLandNoConflicts) {
		// An intermediate commit of a rebase can conflict although the merge of the two ends is
		// clean. There is no conflict package for that merge, as with resolve_conflicts.
		return mergeConflict
	}
	if err != nil {
		return structerr.NewInternal("listing the conflicts of the land: %w", err)
	}
	reason := "conflict resolution was not requested"
	if len(l.files) == 0 {
		reason = landNoConflictingFiles
	}
	return landWithPackage(ctx, mergeConflict, l.notAttempted(reason, nil))
}

// landKindOf classifies a conflicting file from the presence of its sides.
func landKindOf(c landFileConflict) string {
	switch {
	case c.ours == nil || c.theirs == nil:
		return resolver.KindModifyDelete
	case c.base == nil:
		return resolver.KindAddAdd
	case c.ours.oid == c.theirs.oid:
		return resolver.KindMode
	default:
		return resolver.KindContent
	}
}

func landKindProto(kind string) gitalypb.LandConflictFile_Kind {
	switch kind {
	case resolver.KindContent:
		return gitalypb.LandConflictFile_KIND_CONTENT
	case resolver.KindModifyDelete:
		return gitalypb.LandConflictFile_KIND_MODIFY_DELETE
	case resolver.KindAddAdd:
		return gitalypb.LandConflictFile_KIND_ADD_ADD
	case resolver.KindMode:
		return gitalypb.LandConflictFile_KIND_MODE
	}
	return gitalypb.LandConflictFile_KIND_UNSPECIFIED
}

// conflictPackage returns the conflict package of the listing: every conflicting file with its
// sides, hunks and outcome, and the merge's messages, with reason set when the request was not
// attempted at all. It reads the sides the hunks need (prepareHunks) and looks up whether each
// side's blob is kept (sideKept). Its bounds: landMaxPackageFiles files, landMaxPackageMessages
// messages, landMaxPackageBytes encoded; past them the package says what it left out.
func (l landListing) conflictPackage(ctx context.Context, reason string) (*gitalypb.LandConflict, error) {
	tm, tipCut := landCut([]byte(l.tipMessage), landPackageMessageBytes)
	cm, commitCut := landCut([]byte(l.commitMessage), landPackageMessageBytes)
	pkg := &gitalypb.LandConflict{
		Tip:               l.tip.String(),
		Commit:            l.commitID.String(),
		MergeBase:         l.base.String(),
		TipMessage:        tm,
		CommitMessage:     cm,
		MessagesTruncated: tipCut || commitCut,
		Reason:            landCutString(reason, landPackageReasonBytes),
		ProposalRef:       l.proposalRef.String(),
	}
	for i, m := range l.messages {
		if i == landMaxPackageMessages {
			break
		}
		pm := &gitalypb.LandConflictMessage{Type: landCutString(m.kind, landPackageReasonBytes), Message: landCutString(m.message, landPackageReasonBytes)}
		for _, p := range m.paths {
			pm.Paths = append(pm.Paths, []byte(p))
		}
		pkg.Messages = append(pkg.Messages, pm)
	}
	if len(l.files) > landMaxPackageFiles {
		pkg.Reason = landCutString(fmt.Sprintf("the merge has %d conflicting files, more than the %d that a conflict package lists: it cannot be resolved through a land", len(l.files), landMaxPackageFiles), landPackageReasonBytes)
		return pkg, nil
	}

	// The sides first: a file with a side the agent cannot read gets no hunks.
	listings := map[git.ObjectID]map[git.ObjectID][]string{}
	files := make([]*gitalypb.LandConflictFile, len(l.files))
	for i, f := range l.files {
		pf := &gitalypb.LandConflictFile{
			Path:            []byte(f.conflict.path),
			Kind:            landKindProto(f.kind),
			Outcome:         f.outcome,
			Reason:          landCutString(f.reason, landPackageReasonBytes),
			SyntaxUnchecked: f.syntaxUnchecked,
			Proposal:        f.proposal,
		}
		f.unavailable = false
		for _, s := range []struct {
			blob *landBlob
			rev  git.ObjectID
			dst  **gitalypb.LandConflictSide
		}{{f.conflict.base, l.base, &pf.Base}, {f.conflict.ours, l.tip, &pf.Ours}, {f.conflict.theirs, l.commitID, &pf.Theirs}} {
			if s.blob == nil {
				continue
			}
			side := &gitalypb.LandConflictSide{Mode: s.blob.mode}
			switch {
			case s.blob.mode == landModeGitlink:
				side.Unavailable = landSideGitlink
			default:
				kept, err := l.sideKept(ctx, listings, s.blob.oid, f.conflict.path, s.rev)
				if err != nil {
					return nil, err
				}
				if kept {
					side.Oid = s.blob.oid.String()
				} else {
					side.Unavailable = landSideUnavailable
				}
			}
			if side.Unavailable != "" {
				f.unavailable = true
			}
			*s.dst = side
		}
		files[i] = pf
	}
	if err := l.prepareHunks(ctx); err != nil {
		return nil, err
	}
	for i, f := range l.files {
		files[i].Hunks, files[i].HunksOmitted = f.hunks, f.hunksOmitted
	}
	pkg.Files = files

	if proto.Size(pkg) > landMaxPackageBytes {
		for _, pf := range pkg.GetFiles() {
			if len(pf.GetHunks()) > 0 {
				pf.Hunks, pf.HunksOmitted = nil, landHunksPackage
			}
		}
	}
	if proto.Size(pkg) > landMaxPackageBytes {
		pkg.Files = nil
		pkg.Reason = landCutString(fmt.Sprintf("the conflict package of the %d conflicting files would be over %d bytes even without hunks: it cannot be resolved through a land", len(l.files), landMaxPackageBytes), landPackageReasonBytes)
	}
	return pkg, nil
}

// prepareHunks computes the hunks of the content and add/add conflicts, reading the sides that
// are not read yet, within the bounds of the package: a side over max_file_bytes is not read, the
// sides read and the diff work of all files are bounded (landMaxPackageReadBytes,
// landMaxPackageCells), and a file lists at most landMaxHunksPerFile hunks. Every file left without
// all its hunks gets the reason in hunksOmitted.
func (l landListing) prepareHunks(ctx context.Context) error {
	readLeft, cellsLeft := int64(landMaxPackageReadBytes), landMaxPackageCells
	for _, f := range l.files {
		f.hunks, f.hunksOmitted = nil, ""
		if f.kind != resolver.KindContent && f.kind != resolver.KindAddAdd {
			continue
		}
		if f.unavailable {
			f.hunksOmitted = landHunksUnavailable
			continue
		}
		if !f.read {
			reason, err := l.readSides(ctx, f, &readLeft)
			if err != nil {
				return err
			}
			if reason != "" {
				f.hunksOmitted = reason
				continue
			}
		}
		if !f.textRead() {
			f.hunksOmitted = landHunksBinary
			continue
		}
		cost, fits := merge3.Cost(f.sides[0], f.sides[1], f.sides[2])
		if !fits {
			f.hunksOmitted = landHunksDiffCost
			continue
		}
		if cost > cellsLeft {
			f.hunksOmitted = landHunksBudget
			continue
		}
		cellsLeft -= cost
		hunks, ok := landConflictHunks(f.sides[0], f.sides[1], f.sides[2])
		if !ok {
			// Cost said both diffs fit, which is what Merge checks.
			return fmt.Errorf("merge3 refused the sides of %q that fit its bound", f.conflict.path)
		}
		if len(hunks) > landMaxHunksPerFile {
			f.hunksOmitted = fmt.Sprintf("the file has %d conflicting regions; the first %d are listed", len(hunks), landMaxHunksPerFile)
			hunks = hunks[:landMaxHunksPerFile]
		}
		f.hunks = hunks
	}
	return nil
}

// readSides reads the sides of f for its hunks, and returns why it did not: a side over
// max_file_bytes, or sides that would take the package past its reading bound (*readLeft, which it
// lowers by what it reads).
func (l landListing) readSides(ctx context.Context, f *landFileState, readLeft *int64) (string, error) {
	blobs := []*landBlob{f.conflict.base, f.conflict.ours, f.conflict.theirs}
	var size int64
	for _, b := range blobs {
		if b == nil {
			continue
		}
		info, err := l.repo.ReadObjectInfo(ctx, b.oid.Revision())
		if err != nil {
			return "", fmt.Errorf("reading size of %q: %w", f.conflict.path, err)
		}
		if info.Size > l.maxFileBytes {
			return landHunksTooLarge, nil
		}
		size += info.Size
	}
	if size > *readLeft {
		return landHunksBudget, nil
	}
	*readLeft -= size
	var sides [3][]byte
	for i, b := range blobs {
		if b == nil {
			continue
		}
		content, err := l.repo.ReadObject(ctx, b.oid)
		if err != nil {
			return "", fmt.Errorf("reading blob of %q: %w", f.conflict.path, err)
		}
		sides[i] = content
	}
	f.sides, f.read = sides, true
	return "", nil
}

// sideKept reports whether blob is in the tree of the merge base, the tip or the landed commit,
// which the repository keeps whatever becomes of the land. merge-tree also writes blobs of its own
// for a conflict, such as the content merge of a renamed file, into the quarantine of the request:
// those are discarded when the land fails, so their object IDs must not reach the agent. own is
// the commit the side comes from, looked up first at the conflict's path; the full listings of the
// three trees are read only when that misses (a rename, a blob of the merge), once each.
func (l landListing) sideKept(ctx context.Context, listings map[git.ObjectID]map[git.ObjectID][]string, blob git.ObjectID, p string, own git.ObjectID) (bool, error) {
	if own != "" {
		info, err := l.repo.ReadObjectInfo(ctx, git.Revision(own.String()+":"+p))
		var notFound localrepo.InvalidObjectError
		switch {
		case err == nil && info.Oid == blob && info.Type == "blob":
			return true, nil
		case err != nil && !errors.As(err, &notFound):
			return false, fmt.Errorf("looking up %q in %s: %w", p, own, err)
		}
	}
	// Every merge base is a kept tree: with a criss-cross history the base side may come from any.
	revs := append([]git.ObjectID{l.tip, l.commitID}, l.mergeBases...)
	for _, rev := range revs {
		listing, ok := listings[rev]
		if !ok {
			var err error
			if listing, err = landListTree(ctx, l.repo, rev); err != nil {
				return false, err
			}
			listings[rev] = listing
		}
		if len(listing[blob]) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// landCut cuts b to at most n bytes on a UTF-8 boundary and reports whether it was cut.
func landCut(b []byte, n int) ([]byte, bool) {
	if len(b) <= n {
		return b, false
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(b[cut]) {
		cut--
	}
	return b[:cut], true
}

// landCutString cuts s to at most n bytes, valid UTF-8.
func landCutString(s string, n int) string {
	b, _ := landCut([]byte(s), n)
	return strings.ToValidUTF8(string(b), "")
}

// landGeneratedPaths returns the paths that the attributes of rev mark linguist-generated, with one
// git check-attr for all of them.
func landGeneratedPaths(ctx context.Context, repo *localrepo.Repo, rev git.ObjectID, paths []string) (map[string]bool, error) {
	generated := map[string]bool{}
	if len(paths) == 0 {
		return generated, nil
	}
	var stdin bytes.Buffer
	for _, p := range paths {
		stdin.WriteString(p)
		stdin.WriteByte(0)
	}
	var stdout, stderr bytes.Buffer
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name: "check-attr",
			Flags: []gitcmd.Option{
				gitcmd.Flag{Name: "-z"},
				gitcmd.Flag{Name: "--stdin"},
				gitcmd.ValueFlag{Name: "--source", Value: rev.String()},
			},
			Args: []string{"linguist-generated"},
		},
		gitcmd.WithStdin(&stdin),
		gitcmd.WithStdout(&stdout),
		gitcmd.WithStderr(&stderr),
	); err != nil {
		return nil, fmt.Errorf("check-attr: %w, stderr: %q", err, stderr.String())
	}
	// The output is NUL-separated triples: path, attribute, value.
	fields := strings.Split(strings.TrimSuffix(stdout.String(), "\x00"), "\x00")
	if len(fields)%3 != 0 {
		return nil, fmt.Errorf("check-attr: unexpected output of %d fields", len(fields))
	}
	for i := 0; i+2 < len(fields); i += 3 {
		switch fields[i+2] {
		case "set", "true":
			generated[fields[i]] = true
		}
	}
	return generated, nil
}
