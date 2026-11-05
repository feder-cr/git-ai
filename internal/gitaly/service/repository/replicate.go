package repository

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/remoterepo"
	"github.com/feder-cr/git-ai/internal/gitaly/repoutil"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/grpc/client"
	"github.com/feder-cr/git-ai/internal/grpc/metadata"
	"github.com/feder-cr/git-ai/internal/safe"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/tempdir"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/streamio"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ErrInvalidSourceRepository is returned when attempting to replicate from an invalid source repository.
var ErrInvalidSourceRepository = status.Error(codes.NotFound, "invalid source repository")

func (s *server) getReferenceBackend(
	ctx context.Context,
	repoClient gitalypb.RepositoryServiceClient,
	source *gitalypb.Repository,
) (*git.ReferenceBackend, error) {
	resp, err := repoClient.RepositoryInfo(ctx, &gitalypb.RepositoryInfoRequest{Repository: source})
	if err != nil {
		return nil, fmt.Errorf("repository info: %w", err)
	}

	switch resp.GetReferences().GetReferenceBackend() {
	case gitalypb.ReferenceBackend_REFERENCE_BACKEND_FILES:
		return &git.ReferenceBackendFiles, nil
	case gitalypb.ReferenceBackend_REFERENCE_BACKEND_REFTABLE:
		return &git.ReferenceBackendReftables, nil
	case gitalypb.ReferenceBackend_REFERENCE_BACKEND_MVCC:
		return &git.ReferenceBackendMVCC, nil
	default:
		return nil, fmt.Errorf("unknown reference backend")
	}
}

// ReplicateRepository replicates data from a source repository to target repository. On the target
// repository, this operation ensures synchronization of the following components:
//
// - Git config
// - Git attributes
// - Custom Git hooks,
// - References and objects
func (s *server) ReplicateRepository(ctx context.Context, in *gitalypb.ReplicateRepositoryRequest) (*gitalypb.ReplicateRepositoryResponse, error) {
	if err := validateReplicateRepository(ctx, s.locator, in); err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}

	repoClient, err := s.newRepoClient(ctx, in.GetSource().GetStorageName())
	if err != nil {
		return nil, structerr.NewInternal("new client: %w", err)
	}

	// We're checking for repository existence up front such that we can give a conclusive error
	// in case it doesn't. Otherwise, the error message returned to the client would depend on
	// the order in which the sync functions were executed. Most importantly, given that
	// `syncRepository` uses FetchInternalRemote which in turn uses gitaly-ssh, this code path
	// cannot pass up NotFound errors given that there is no communication channel between
	// Gitaly and gitaly-ssh.
	request, err := repoClient.RepositoryExists(ctx, &gitalypb.RepositoryExistsRequest{
		Repository: in.GetSource(),
	})
	if err != nil {
		return nil, structerr.NewInternal("checking for repo existence: %w", err)
	}
	if !request.GetExists() {
		return nil, ErrInvalidSourceRepository
	}

	// When creating a replica, we extract the tar of the source repository into
	// the target repository. So we need to ensure that they use the same reference
	// backend.
	sourceBackend, err := s.getReferenceBackend(ctx, repoClient, in.GetSource())
	if err != nil {
		if structerr.GRPCCode(err) == codes.FailedPrecondition {
			return nil, ErrInvalidSourceRepository
		}

		return nil, structerr.NewInternal("source reference backend: %w", err)
	}

	// A repository using the MVCC reference backend must be served by the MVCC-enabled Git
	// binary. The MVCC middleware cannot select it for this RPC as the target repository does
	// not exist when the request arrives, so set it in the context.
	if sourceBackend.Name == git.ReferenceBackendMVCC.Name {
		ctx = gitcmd.ContextWithReferenceBackend(ctx, git.ReferenceBackendMVCC)
	}

	if err := s.locator.ValidateRepository(ctx, in.GetRepository()); err != nil {
		repoPath, err := s.locator.GetRepoPath(ctx, in.GetRepository(), storage.WithRepositoryVerificationSkipped())
		if err != nil {
			return nil, structerr.NewInternal("%w", err)
		}

		if err = s.create(ctx, in, sourceBackend, repoPath); err != nil {
			if errors.Is(err, ErrInvalidSourceRepository) {
				return nil, ErrInvalidSourceRepository
			}

			return nil, structerr.NewInternal("%w", err)
		}
	}

	// The partitioning hint should not be forwarded to other Gitaly nodes as the path is irrelevant for them.
	outgoingCtx := storage.ContextWithoutPartitioningHint(ctx)
	outgoingCtx = metadata.IncomingToOutgoing(outgoingCtx)

	if err := s.replicateRepository(outgoingCtx, in.GetSource(), in.GetRepository()); err != nil {
		return nil, structerr.NewInternal("replicating repository: %w", err)
	}

	return &gitalypb.ReplicateRepositoryResponse{}, nil
}

func (s *server) replicateRepository(ctx context.Context, source, target *gitalypb.Repository) error {
	if err := s.syncGitconfig(ctx, source, target, func(ctx context.Context, path string, content io.Reader) error {
		if err := s.writeFile(ctx, path, content); err != nil {
			return err
		}

		if tx := storage.ExtractTransaction(ctx); tx != nil {
			originalConfigRelativePath, err := filepath.Rel(tx.FS().Root(), path)
			if err != nil {
				return fmt.Errorf("original config relative path: %w", err)
			}

			if err := tx.FS().RecordRemoval(originalConfigRelativePath); err != nil {
				return fmt.Errorf("record old config removal: %w", err)
			}

			if err := tx.FS().RecordFile(originalConfigRelativePath); err != nil {
				return fmt.Errorf("record new config creation: %w", err)
			}
		}

		return nil
	}); err != nil {
		return fmt.Errorf("synchronizing gitconfig: %w", err)
	}

	if err := s.syncReferences(ctx, source, target); err != nil {
		return fmt.Errorf("synchronizing references: %w", err)
	}

	if err := s.syncCustomHooks(ctx, source, target); err != nil {
		return fmt.Errorf("synchronizing custom hooks: %w", err)
	}

	return nil
}

func validateReplicateRepository(ctx context.Context, locator storage.Locator, in *gitalypb.ReplicateRepositoryRequest) error {
	if err := locator.ValidateRepository(ctx, in.GetRepository(), storage.WithSkipRepositoryExistenceCheck()); err != nil {
		return err
	}

	if in.GetSource() == nil {
		return errors.New("source repository cannot be empty")
	}

	if in.GetRepository().GetStorageName() == in.GetSource().GetStorageName() {
		return errors.New("repository and source have the same storage")
	}

	return nil
}

func (s *server) create(
	ctx context.Context,
	in *gitalypb.ReplicateRepositoryRequest,
	sourceBackend *git.ReferenceBackend,
	repoPath string,
) error {
	// if the directory exists, remove it
	if _, err := os.Stat(repoPath); err == nil {
		tempDir, err := tempdir.NewWithoutContext(in.GetRepository().GetStorageName(), s.logger, s.locator)
		if err != nil {
			return err
		}

		if err = os.Rename(repoPath, filepath.Join(tempDir.Path(), filepath.Base(repoPath))); err != nil {
			return fmt.Errorf("error deleting invalid repo: %w", err)
		}

		s.logger.WithField("repo_path", repoPath).WarnContext(ctx, "removed invalid repository")
	}

	if err := s.createFromSnapshot(ctx, sourceBackend, in.GetSource(), in.GetRepository()); err != nil {
		return fmt.Errorf("could not create repository from snapshot: %w", err)
	}

	return nil
}

func (s *server) createFromSnapshot(
	ctx context.Context,
	sourceBackend *git.ReferenceBackend,
	source, target *gitalypb.Repository,
) error {
	if err := repoutil.Create(ctx, s.logger, s.locator, s.gitCmdFactory, s.catfileCache, s.txManager, s.repositoryCounter, target, func(repo *gitalypb.Repository) error {
		if err := s.extractSnapshot(ctx, source, repo); err != nil {
			return fmt.Errorf("extracting snapshot: %w", err)
		}

		// Sync the full config from the source, overwriting the partial config included in the
		// snapshot (which only contains repositoryformatversion and extensions.*). We write it
		// directly to disk without voting as the voting is handled by the voting round of the
		// repository creation. We also don't want to record the config creating operation
		// separately as it is already recorded by `repoutil.Create` when it records the entire repository.
		//
		// We only run this with transactions as the file update is not atomic without transactions.
		if tx := storage.ExtractTransaction(ctx); tx != nil {
			if err := s.syncGitconfig(ctx, source, repo, func(ctx context.Context, path string, content io.Reader) (returnedErr error) {
				file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.File)
				if err != nil {
					return fmt.Errorf("open: %w", err)
				}

				defer func() {
					if err := file.Close(); err != nil {
						returnedErr = errors.Join(returnedErr, fmt.Errorf("close: %w", err))
					}
				}()

				if _, err := io.Copy(file, content); err != nil {
					return fmt.Errorf("copy: %w", err)
				}

				return nil
			}); err != nil {
				return fmt.Errorf("sync gitconfig: %w", err)
			}
		}

		return nil
	}, repoutil.WithReferenceBackend(*sourceBackend)); err != nil {
		return fmt.Errorf("creating repository: %w", err)
	}

	if tx := storage.ExtractTransaction(ctx); tx != nil {
		if err := s.migrationStateManager.RecordKeyCreation(
			tx,
			tx.OriginalRepository(target).GetRelativePath(),
		); err != nil {
			return fmt.Errorf("recording migration key: %w", err)
		}
	}

	return nil
}

func (s *server) extractSnapshot(ctx context.Context, source, target *gitalypb.Repository) error {
	repoClient, err := s.newRepoClient(ctx, source.GetStorageName())
	if err != nil {
		return fmt.Errorf("new client: %w", err)
	}

	stream, err := repoClient.GetSnapshot(ctx, &gitalypb.GetSnapshotRequest{Repository: source})
	if err != nil {
		return fmt.Errorf("get snapshot: %w", err)
	}

	// We need to catch a possible 'invalid repository' error from GetSnapshot. On an empty read,
	// we read the first message from the stream here to get access to the possible 'invalid repository' error.
	firstBytes, err := stream.Recv()
	if err != nil {
		switch {
		case structerr.GRPCCode(err) == codes.NotFound && strings.Contains(err.Error(), "GetRepoPath: not a git repository:"):
			// The error condition exists for backwards compatibility purposes, only,
			// and can be removed in the next release.
			return ErrInvalidSourceRepository
		case structerr.GRPCCode(err) == codes.NotFound && strings.Contains(err.Error(), storage.ErrRepositoryNotFound.Error()):
			return ErrInvalidSourceRepository
		case structerr.GRPCCode(err) == codes.FailedPrecondition && strings.Contains(err.Error(), storage.ErrRepositoryNotValid.Error()):
			return ErrInvalidSourceRepository
		default:
			return fmt.Errorf("first snapshot read: %w", err)
		}
	}

	snapshotReader := io.MultiReader(
		bytes.NewReader(firstBytes.GetData()),
		streamio.NewReader(func() ([]byte, error) {
			resp, err := stream.Recv()
			return resp.GetData(), err
		}),
	)

	targetPath, err := s.locator.GetRepoPath(ctx, target, storage.WithRepositoryVerificationSkipped())
	if err != nil {
		return fmt.Errorf("target path: %w", err)
	}

	// Extract tar using Go's tar package
	if err := s.extractTarToDirectory(ctx, snapshotReader, targetPath); err != nil {
		return fmt.Errorf("extract tar: %w", err)
	}

	return nil
}

// errSymlinkEscapes is returned when a symlink resolves outside of the extraction directory.
var errSymlinkEscapes = errors.New("symlink target escapes extraction directory")

// maxSymlinkChainLength caps how many symlinks resolveInRoot follows before giving up. It mirrors
// the kernel's own limit on symlink chains so that cyclic links terminate instead of looping.
const maxSymlinkChainLength = 40

// extractTarToDirectory extracts a tar archive to the specified directory using Go's tar package
func (s *server) extractTarToDirectory(ctx context.Context, reader io.Reader, targetDir string) error {
	targetDir = filepath.Clean(targetDir)

	// All filesystem operations go through the root so that the kernel refuses any path that
	// traverses a symlink leading out of targetDir. Without it an archive could first create a
	// symlink that only escapes once a later entry completes the chain, and the entries written
	// in between would land outside of the extraction directory.
	root, err := os.OpenRoot(targetDir)
	if err != nil {
		return fmt.Errorf("opening extraction root: %w", err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			s.logger.WithField("directory", targetDir).WithError(err).
				WarnContext(ctx, "failed to close extraction root")
		}
	}()

	tarReader := tar.NewReader(reader)

	// Paths of the symlinks the archive creates, revalidated against the final state of the
	// extraction directory once the loop below has finished.
	var extractedSymlinks []string

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break // End of archive
		}
		if err != nil {
			return fmt.Errorf("reading tar header: %w", err)
		}

		targetPath := filepath.Join(targetDir, header.Name)

		if !strings.HasPrefix(targetPath, targetDir+string(os.PathSeparator)) &&
			targetPath != targetDir {
			return fmt.Errorf("invalid file path in tar: %s", header.Name)
		}

		// Get the path relative to the target dir
		relPath, err := filepath.Rel(targetDir, targetPath)
		if err != nil {
			return fmt.Errorf("relative file path in tar: %w", err)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			// The root rejects modes with bits set outside 0o777, and a tar header may carry
			// setuid, setgid or sticky bits. Only keep the permission bits.
			if err := root.MkdirAll(relPath, os.FileMode(header.Mode).Perm()); err != nil {
				return fmt.Errorf("creating directory %s: %w", targetPath, err)
			}

		case tar.TypeReg:
			if err := s.extractFile(ctx, tarReader, root, relPath, header); err != nil {
				return fmt.Errorf("extracting file %s: %w", targetPath, err)
			}

		case tar.TypeSymlink:
			if filepath.IsAbs(header.Linkname) {
				return fmt.Errorf("absolute symlink not allowed: %s -> %s", header.Name, header.Linkname)
			}

			// Reject targets that already escape so that a malicious archive is caught before
			// anything further is extracted. Chains completed by later entries are caught by
			// the validation below the loop.
			if err := validateSymlinkWithinRoot(targetDir, relPath, header.Linkname); err != nil {
				return err
			}

			// Remove existing file/symlink if it exists
			if err := root.Remove(relPath); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("removing existing file for symlink %s: %w", targetPath, err)
			}

			if err := root.Symlink(header.Linkname, relPath); err != nil {
				return fmt.Errorf("creating symlink %s -> %s: %w", targetPath, header.Linkname, err)
			}

			extractedSymlinks = append(extractedSymlinks, relPath)

		case tar.TypeLink:
			linkTarget := filepath.Join(targetDir, header.Linkname)

			if !strings.HasPrefix(linkTarget, targetDir+string(os.PathSeparator)) &&
				linkTarget != targetDir {
				return fmt.Errorf("invalid hard link target: %s", header.Linkname)
			}

			relLinkTarget, err := filepath.Rel(targetDir, linkTarget)
			if err != nil {
				return fmt.Errorf("relative hard link target: %w", err)
			}

			// Remove existing file if it exists
			if err := root.Remove(relPath); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("removing existing file for hard link %s: %w", targetPath, err)
			}

			if err := root.Link(relLinkTarget, relPath); err != nil {
				return fmt.Errorf("creating hard link %s -> %s: %w", targetPath, linkTarget, err)
			}

			// Hard linking a symlink copies it into a new parent directory, from which its
			// relative target may resolve elsewhere, so the copy needs validating also.
			info, err := root.Lstat(relPath)
			if err != nil {
				return fmt.Errorf("checking hard link %s: %w", targetPath, err)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				extractedSymlinks = append(extractedSymlinks, relPath)
			}

		default:
			// Skip unsupported file types (devices, FIFOs, etc.)
			s.logger.WithField("file", header.Name).WithField("type", header.Typeflag).
				WarnContext(ctx, "skipping unsupported file type in tar archive")
		}

		if header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeDir {
			if err := root.Chmod(relPath, os.FileMode(header.Mode).Perm()); err != nil {
				return fmt.Errorf("setting permissions for %s: %w", targetPath, err)
			}
		}
	}

	// Symlinks are validated a second time now that the whole archive is on disk, because a chain
	// of symlinks can escape even when no single link in it does. Given the entries
	//
	//	escape -> a/up/..
	//	a/
	//	a/up   -> ..
	//
	// "escape" is dangling when it is extracted and resolves no further than "a", while "a/up"
	// resolves to the extraction directory itself. Both are legal on their own, yet once all three
	// exist "escape" points at the parent of the extraction directory. The escape only emerges from
	// the combination, so it cannot be detected entry by entry while the archive is still being
	// streamed.
	for _, relPath := range extractedSymlinks {
		linkname, err := root.Readlink(relPath)
		if err != nil {
			return fmt.Errorf("reading symlink %s: %w", filepath.Join(targetDir, relPath), err)
		}

		if err := validateSymlinkWithinRoot(targetDir, relPath, linkname); err != nil {
			return err
		}
	}

	return nil
}

// validateSymlinkWithinRoot verifies that the symlink at relPath, which is relative to root, does not point
// out of root. It walks the target one component at a time, following any symlink it encounters,
// and reports an error as soon as the resolution leaves root. Components that do not exist are
// treated as directories so that dangling targets are validated too.
func validateSymlinkWithinRoot(root, relPath, linkname string) (returnedErr error) {
	defer func() {
		if returnedErr != nil {
			returnedErr = fmt.Errorf("validating symlink %s -> %s: %w", relPath, linkname, returnedErr)
		}
	}()

	// start with the root path
	resolved := root

	// The target is resolved from the symlink's parent directory, so the components of both are
	// walked. They are split individually rather than joined into a single path first because
	// joining cleans the result, and cleaning collapses "a/.." even when "a" is a symlink, which
	// is precisely the escape this function exists to catch.
	remaining := slices.Concat(
		strings.Split(filepath.Dir(relPath), string(os.PathSeparator)),
		strings.Split(linkname, string(os.PathSeparator)),
	)

	// this tracks the number of symlinks in the chain
	followed := 0

	for len(remaining) > 0 {
		component := remaining[0]
		remaining = remaining[1:]

		switch component {
		case "", ".":
			continue
		case "..":
			if resolved == root {
				return errSymlinkEscapes
			}
			resolved = filepath.Dir(resolved)
			continue
		}

		next := filepath.Join(resolved, component)

		info, err := os.Lstat(next)
		switch {
		// in te case of a directory that still does not exist on disk
		case os.IsNotExist(err):
			resolved = next
		// any other kind of errors are returned
		case err != nil:
			return fmt.Errorf("resolving %s: %w", next, err)
		// either a directory or a file that exist
		case info.Mode()&os.ModeSymlink == 0:
			resolved = next
		default:
			followed++
			if followed > maxSymlinkChainLength {
				return fmt.Errorf("resolving %s: %w", next, syscall.ELOOP)
			}

			linkname, err := os.Readlink(next)
			if err != nil {
				return fmt.Errorf("reading symlink %s: %w", next, err)
			}

			// Absolute targets are rejected on extraction, so encountering one here means it
			// was already present in the extraction directory. Treat it as an escape rather
			// than resolving it against the filesystem root.
			if filepath.IsAbs(linkname) {
				return errSymlinkEscapes
			}

			remaining = slices.Concat(strings.Split(linkname, string(os.PathSeparator)), remaining)
		}
	}

	return nil
}

// extractFile extracts a regular file from the tar archive to relPath, which is relative to root.
func (s *server) extractFile(ctx context.Context, tarReader *tar.Reader, root *os.Root, relPath string, header *tar.Header) error {
	// The root rejects modes with bits set outside 0o777, which both mode.Directory (fs.ModeDir)
	// and the tar header (setuid, setgid, sticky) may set. Only keep the permission bits.
	if err := root.MkdirAll(filepath.Dir(relPath), mode.Directory.Perm()); err != nil {
		return fmt.Errorf("creating parent directory: %w", err)
	}

	file, err := root.OpenFile(relPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode).Perm())
	if err != nil {
		return fmt.Errorf("creating file: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			s.logger.WithField("file", filepath.Join(root.Name(), relPath)).WithError(closeErr).
				WarnContext(ctx, "failed to close file during tar extraction")
		}
	}()

	// Copy file content with context cancellation support
	const bufferSize = 32 * 1024 // 32KB buffer
	buffer := make([]byte, bufferSize)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		n, err := tarReader.Read(buffer)
		if n > 0 {
			if _, writeErr := file.Write(buffer[:n]); writeErr != nil {
				return fmt.Errorf("writing to file: %w", writeErr)
			}
		}

		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading from tar: %w", err)
		}
	}

	return nil
}

func (s *server) syncReferences(ctx context.Context, source, target *gitalypb.Repository) error {
	repo := s.localRepoFactory.Build(target)

	if err := fetchInternalRemote(ctx, s.txManager, s.conns, repo, source); err != nil {
		return fmt.Errorf("fetch internal remote: %w", err)
	}

	return nil
}

func fetchInternalRemote(
	ctx context.Context,
	txManager transaction.Manager,
	conns *client.Pool,
	repo *localrepo.Repo,
	remoteRepoProto *gitalypb.Repository,
) error {
	// Create a remote repository to access the remote repo
	remoteRepo, err := remoterepo.New(ctx, remoteRepoProto, conns)
	if err != nil {
		return structerr.NewInternal("%w", err)
	}

	// This call fetches the name of the default branch (HEAD) in the remote repo, but
	// does not attempt to resolve the reference.
	remoteDefaultBranch, err := remoteRepo.HeadReference(ctx)
	if err != nil {
		return structerr.NewInternal("getting remote default branch: %w", err)
	}

	// Now we resolve the `HEAD` revision of the remote repo. If the reference cannot be
	// resolved, it can mean 2 things:
	// 1. The repository is an empty repository with no commit yet
	// 2. HEAD is pointing to a non-existing branch
	remoteHeadResolved := false
	if _, err = remoteRepo.ResolveRevision(ctx, remoteDefaultBranch.Revision()); err != nil {
		if !errors.Is(err, git.ErrReferenceNotFound) {
			return structerr.NewInternal("error calling the remote repo to resolve HEAD reference: %w", err)
		}
	} else {
		remoteHeadResolved = true
	}

	// Fetch from the remote repo to sync the local repo
	var stderr bytes.Buffer
	if err := repo.FetchInternal(
		ctx,
		remoteRepoProto,
		[]string{git.MirrorRefSpec},
		localrepo.FetchOpts{
			Prune:  true,
			Stderr: &stderr,
			// By default, Git will fetch any tags that point into the fetched references. This check
			// requires time, and is ultimately a waste of compute because we already mirror all refs
			// anyway, including tags. By adding `--no-tags` we can thus ask Git to skip that and thus
			// accelerate the fetch.
			Tags: localrepo.FetchOptsTagsNone,
			CommandOptions: []gitcmd.CmdOpt{
				gitcmd.WithConfig(gitcmd.ConfigPair{Key: "fetch.negotiationAlgorithm", Value: "skipping"}),
				// Disable the consistency checks of objects fetched into the replicated repository.
				// These fetched objects come from preexisting internal sources, thus it would be
				// problematic for the fetch to fail consistency checks due to altered requirements.
				gitcmd.WithConfig(gitcmd.ConfigPair{Key: "fetch.fsckObjects", Value: "false"}),
				// The fetched objects are kept as a pack, never unpacked into loose objects: a
				// loose object written into the repository would race housekeeping's removal of
				// empty loose object directories, which fails git's write.
				gitcmd.WithConfig(gitcmd.ConfigPair{Key: "fetch.unpackLimit", Value: "1"}),
			},
		},
	); err != nil {
		if refusal := gitcmd.ReferenceNameLengthRefusal(stderr.Bytes()); refusal != nil {
			return refusal
		}
		if errors.As(err, &localrepo.FetchFailedError{}) {
			return structerr.New("%w", err).WithMetadata("stderr", stderr.String())
		}
		return fmt.Errorf("fetch: %w", err)
	}

	// If we could not resolve remote repo's HEAD prior to the fetch, it means either the remote
	// repository is empty or HEAD points to a non-existing branch.
	// If the repository is empty, there is nothing more to do. We should just wait for commits
	// to be written before syncing references. So it is safe to return.
	// If the HEAD points to a non-existing branch, here is not the place to try and fix it
	// anyway. The primary node should fix it first, so let's return.
	if !remoteHeadResolved {
		return nil
	}

	// Let's try to resolve the default branch of the remote repo
	// on the local repo, to make sure we fetched it right.
	_, err = repo.ResolveRevision(ctx, remoteDefaultBranch.Revision())
	if err != nil {
		return structerr.NewInternal("remote default branch does not resolve locally after fetch: %w", err)
	}

	// We need to synchronize the HEAD reference on the local repo.
	// First get the default branch on the local repo.
	localDefaultBranch, err := repo.HeadReference(ctx)
	if err != nil {
		return structerr.NewInternal("getting HEAD on local repo: %w", err)
	}

	// We know from above that the default branch on the remote repository resolves
	// to an OID on the local repository after fetch.
	// If the local HEAD is the same as the remote default branch, it means the local HEAD
	// also resolves to the same OID, so we are good.
	// If they don't match, we know the remote default branch resolves to a valid OID
	// so it is safe to update the current default branch here with the value of the
	// remote default branch.
	if localDefaultBranch != remoteDefaultBranch {
		if err = repo.SetDefaultBranch(ctx, txManager, remoteDefaultBranch); err != nil {
			return structerr.NewInternal("setting default branch on local repo: %w", err)
		}
	}

	return nil
}

// syncCustomHooks replicates custom hooks from a source to a target.
func (s *server) syncCustomHooks(ctx context.Context, source, target *gitalypb.Repository) error {
	repoClient, err := s.newRepoClient(ctx, source.GetStorageName())
	if err != nil {
		return fmt.Errorf("creating repo client: %w", err)
	}

	stream, err := repoClient.GetCustomHooks(ctx, &gitalypb.GetCustomHooksRequest{
		Repository: source,
	})
	if err != nil {
		return fmt.Errorf("getting custom hooks: %w", err)
	}

	reader := streamio.NewReader(func() ([]byte, error) {
		request, err := stream.Recv()
		return request.GetData(), err
	})

	if err := repoutil.SetCustomHooks(ctx, s.logger, s.locator, s.txManager, reader, target); err != nil {
		return fmt.Errorf("setting custom hooks: %w", err)
	}

	return nil
}

func (s *server) syncGitconfig(ctx context.Context, source, target *gitalypb.Repository, writeConfig func(ctx context.Context, path string, content io.Reader) error) error {
	repoClient, err := s.newRepoClient(ctx, source.GetStorageName())
	if err != nil {
		return err
	}

	repoPath, err := s.locator.GetRepoPath(ctx, target)
	if err != nil {
		return err
	}

	stream, err := repoClient.GetConfig(ctx, &gitalypb.GetConfigRequest{
		Repository: source,
	})
	if err != nil {
		return err
	}

	configPath := filepath.Join(repoPath, "config")
	return writeConfig(ctx, configPath, streamio.NewReader(func() ([]byte, error) {
		resp, err := stream.Recv()
		return resp.GetData(), err
	}))
}

func (s *server) writeFile(ctx context.Context, path string, reader io.Reader) (returnedErr error) {
	parentDir := filepath.Dir(path)
	if err := os.MkdirAll(parentDir, mode.Directory); err != nil {
		return err
	}

	lockedFile, err := safe.NewLockingFileWriter(path, safe.LockingFileWriterConfig{
		FileWriterConfig: safe.FileWriterConfig{
			FileMode: mode.File,
		},
	})
	if err != nil {
		return fmt.Errorf("creating file writer: %w", err)
	}
	defer func() {
		if err := lockedFile.Close(); err != nil && returnedErr == nil {
			returnedErr = err
		}
	}()

	if _, err := io.Copy(lockedFile, reader); err != nil {
		return err
	}

	if err := transaction.CommitLockedFile(ctx, s.txManager, lockedFile); err != nil {
		return err
	}

	return nil
}

// newRepoClient creates a new RepositoryClient that talks to the gitaly of the source repository
func (s *server) newRepoClient(ctx context.Context, storageName string) (gitalypb.RepositoryServiceClient, error) {
	gitalyServerInfo, err := storage.ExtractGitalyServer(ctx, storageName)
	if err != nil {
		return nil, err
	}

	conn, err := s.conns.Dial(ctx, gitalyServerInfo.Address, gitalyServerInfo.Token)
	if err != nil {
		return nil, err
	}

	return gitalypb.NewRepositoryServiceClient(conn), nil
}
