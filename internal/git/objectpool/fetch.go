package objectpool

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/housekeeping"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/stats"
	"github.com/feder-cr/git-ai/internal/git/updateref"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/transaction/voting"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

var objectPoolRefspec = fmt.Sprintf("+refs/*:%s/*", git.ObjectPoolRefNamespace)

// LocalRepoFactory is a function that returns a new localrepo.Repo for the given repository.
type LocalRepoFactory func(repo *gitalypb.Repository) *localrepo.Repo

// FetchFromOrigin initializes the pool and fetches the objects from its origin repository
func (o *ObjectPool) FetchFromOrigin(ctx context.Context, origin *localrepo.Repo, newLocalRepo LocalRepoFactory) error {
	if !o.Exists(ctx) {
		return structerr.NewInvalidArgument("object pool does not exist")
	}

	originPath, err := origin.Path(ctx)
	if err != nil {
		return fmt.Errorf("computing origin repo's path: %w", err)
	}

	if err := o.housekeepingManager.CleanStaleData(ctx, o.Repo, housekeeping.DefaultStaleDataCleanup()); err != nil {
		return fmt.Errorf("cleaning stale data: %w", err)
	}

	if err := o.logStats(ctx, "before fetch"); err != nil {
		return fmt.Errorf("computing stats before fetch: %w", err)
	}

	// Ideally we wouldn't want to prune old references at all so that we can keep alive all
	// objects without having to create loads of dangling references. But unfortunately keeping
	// around old refs can lead to D/F conflicts between old references that have since
	// been deleted in the pool and new references that have been added in the pool member we're
	// fetching from. E.g. if we have the old reference `refs/heads/branch` and the pool member
	// has replaced that since with a new reference `refs/heads/branch/conflict` then
	// the fetch would now always fail because of that conflict.
	//
	// Due to the lack of an alternative to resolve that conflict we are thus forced to enable
	// pruning. This isn't too bad given that we know to keep alive the old objects via dangling
	// refs anyway, but I'd sleep easier if we didn't have to do this.
	//
	// Note that we need to perform the pruning separately from the fetch: if the fetch is using
	// `--atomic` and `--prune` together then it still wouldn't be able to recover from the D/F
	// conflict. So we first to a preliminary prune that only prunes refs without fetching
	// objects yet to avoid that scenario.
	if err := o.pruneReferences(ctx, origin); err != nil {
		return fmt.Errorf("pruning references: %w", err)
	}

	objectHash, err := o.Repo.ObjectHash(ctx)
	if err != nil {
		return fmt.Errorf("detecting object hash: %w", err)
	}

	var stderr bytes.Buffer
	if err := o.Repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name: "fetch",
			Flags: []gitcmd.Option{
				gitcmd.Flag{Name: "--quiet"},
				gitcmd.Flag{Name: "--atomic"},
				// We already fetch tags via our refspec, so we don't
				// want to fetch them a second time via Git's default
				// tag refspec.
				gitcmd.Flag{Name: "--no-tags"},
				// We don't need FETCH_HEAD, and it can potentially be hundreds of
				// megabytes when doing a mirror-sync of repos with huge numbers of
				// references.
				gitcmd.Flag{Name: "--no-write-fetch-head"},
				// Disable showing forced updates, which may take a considerable
				// amount of time to compute. We don't display any output anyway,
				// which makes this computation kind of moot.
				gitcmd.Flag{Name: "--no-show-forced-updates"},
			},
			Args: []string{originPath, objectPoolRefspec},
		},
		gitcmd.WithRefTxHook(objectHash, o.Repo),
		gitcmd.WithStderr(&stderr),
		gitcmd.WithConfig(gitcmd.ConfigPair{
			// Git is so kind to point out that we asked it to not show forced updates
			// by default, so we need to ask it not to do that.
			Key: "advice.fetchShowForcedUpdates", Value: "false",
		}),
	); err != nil {
		if refusal := gitcmd.ReferenceNameLengthRefusal(stderr.Bytes()); refusal != nil {
			return refusal
		}
		return fmt.Errorf("fetch into object pool: %w, stderr: %q", err,
			stderr.String())
	}

	if err := o.rescueDanglingObjects(ctx); err != nil {
		return fmt.Errorf("rescuing dangling objects: %w", err)
	}

	if err := o.logStats(ctx, "after fetch"); err != nil {
		return fmt.Errorf("computing stats after fetch: %w", err)
	}

	// This RPC fetches new objects from the origin repository into the object pool. Afterward, it
	// triggers a full set of housekeeping tasks. If WAL transaction is enabled, the housekeeping
	// manager initiates a transaction and executes all housekeeping tasks inside the transaction
	// context. Normally, the transaction life cycle is managed by a gRPC middleware. RPC handlers
	// extract the transaction from the context. Unfortunately, following that approach results in
	// two non-nested transactions. The housekeeping transaction is committed before the main
	// fetching one. The housekeeping task's effect is pushed to the next request. That's opposed to
	// the initial intention of running housekeeping after fetching. As a result, this RPC needs to
	// manage the transaction itself so that two transactions can be committed in the right order.
	if tx := storage.ExtractTransaction(ctx); tx != nil {
		commitLSN, err := tx.Commit(ctx)
		if err != nil {
			return fmt.Errorf("commit: %w", err)
		}

		storage.LogTransactionCommit(ctx, o.logger, commitLSN, "FetchFromOrigin")
	}

	// We've committed the original transaction above. OptimizeRepository internally starts
	// another transaction, and knows how to retrieve the original relative path of the repository
	// if there is a transaction in the context.
	if err := o.housekeepingManager.OptimizeRepository(ctx, o.Repo); err != nil {
		return fmt.Errorf("optimizing pool repo: %w", err)
	}

	return nil
}

// pruneReferences prunes any references that have been deleted in the origin repository.
func (o *ObjectPool) pruneReferences(ctx context.Context, origin *localrepo.Repo) (returnedErr error) {
	originPath, err := origin.Path(ctx)
	if err != nil {
		return fmt.Errorf("computing origin repo's path: %w", err)
	}

	// Ideally, we'd just use `git remote prune` directly. But unfortunately, this command does
	// not support atomic updates, but will instead use a separate reference transaction for
	// updating the packed-refs file and for updating each of the loose references. This can be
	// really expensive in case we are about to prune a lot of references given that every time,
	// the reference-transaction hook needs to vote on the deletion and reach quorum.
	//
	// Instead we ask for a dry-run, parse the output and queue up every reference into a
	// git-update-ref(1) process. While ugly, it works around the performance issues.
	prune, err := o.Repo.Exec(ctx,
		gitcmd.Command{
			Name:   "remote",
			Action: "prune",
			Args:   []string{"origin"},
			Flags: []gitcmd.Option{
				gitcmd.Flag{Name: "--dry-run"},
			},
		},
		gitcmd.WithConfig(gitcmd.ConfigPair{Key: "remote.origin.url", Value: originPath}),
		gitcmd.WithConfig(gitcmd.ConfigPair{Key: "remote.origin.fetch", Value: objectPoolRefspec}),
		// This is a dry-run, only, so we don't have to enable hooks.
		gitcmd.WithDisabledHooks(),
		gitcmd.WithSetupStdout(),
	)
	if err != nil {
		return fmt.Errorf("spawning prune: %w", err)
	}

	updater, err := updateref.New(ctx, o.Repo)
	if err != nil {
		return fmt.Errorf("spawning updater: %w", err)
	}
	defer func() {
		if err := updater.Close(); err != nil && returnedErr == nil {
			returnedErr = fmt.Errorf("cancel updater: %w", err)
		}
	}()

	if err := updater.Start(); err != nil {
		return fmt.Errorf("start reference transaction: %w", err)
	}

	objectHash, err := o.ObjectHash(ctx)
	if err != nil {
		return fmt.Errorf("detecting object hash: %w", err)
	}

	// We need to manually compute a vote because all deletions we queue up here are
	// force-deletions. We are forced to filter out force-deletions because these may also
	// happen when evicting references from the packed-refs file.
	voteHash := voting.NewVoteHash()

	scanner := bufio.NewScanner(prune)
	for scanner.Scan() {
		line := scanner.Bytes()

		// We need to skip the first two lines that represent the header of git-remote(1)'s
		// output. While we should ideally just use a state machine here, it doesn't feel
		// worth it given that the output is comparatively simple and given that the pruned
		// branches are distinguished by a special prefix.
		switch {
		case bytes.Equal(line, []byte("Pruning origin")):
			continue
		case bytes.HasPrefix(line, []byte("URL: ")):
			continue
		case bytes.HasPrefix(line, []byte(" * [would prune] ")):
			// The references announced by git-remote(1) only have the remote's name as
			// prefix, which is "origin". We thus have to reassemble the complete name
			// of every reference here.
			deletedRef := "refs/remotes/" + string(bytes.TrimPrefix(line, []byte(" * [would prune] ")))

			if _, err := io.Copy(voteHash, strings.NewReader(fmt.Sprintf("%[1]s %[1]s %s\n", objectHash.ZeroOID, deletedRef))); err != nil {
				return fmt.Errorf("hashing reference deletion: %w", err)
			}

			if err := updater.Delete(git.ReferenceName(deletedRef)); err != nil {
				return fmt.Errorf("queueing ref for deletion: %w", err)
			}
		default:
			return fmt.Errorf("unexpected line: %q", line)
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scanning deleted refs: %w", err)
	}

	if err := prune.Wait(); err != nil {
		return fmt.Errorf("waiting for prune: %w", err)
	}

	vote, err := voteHash.Vote()
	if err != nil {
		return fmt.Errorf("computing vote: %w", err)
	}

	// Cast a preparing vote to acquire a distributed lock
	if err := transaction.VoteOnContext(ctx, o.txManager, vote, voting.Preparing); err != nil {
		return fmt.Errorf("casting preparing vote: %w", err)
	}

	// Prepare references so that they're locked and cannot be written by any concurrent
	// processes. This also verifies that we can indeed delete the references.
	if err := updater.Prepare(); err != nil {
		return fmt.Errorf("preparing deletion of references: %w", err)
	}

	// Vote on the references we're about to delete.
	if err := transaction.VoteOnContext(ctx, o.txManager, vote, voting.Prepared); err != nil {
		return fmt.Errorf("preparational vote on pruned references: %w", err)
	}

	// Commit the pruned references to disk so that the change gets applied.
	if err := updater.Commit(); err != nil {
		return fmt.Errorf("deleting references: %w", err)
	}

	// And then confirm that we actually deleted the references.
	if err := transaction.VoteOnContext(ctx, o.txManager, vote, voting.Committed); err != nil {
		return fmt.Errorf("preparational vote on pruned references: %w", err)
	}

	return nil
}

const danglingObjectNamespace = "refs/dangling/"

// rescueDanglingObjects creates refs for all dangling objects if finds
// with `git fsck`, which converts those objects from "dangling" to
// "not-dangling". This guards against any object ever being deleted from
// a pool repository. This is a defense in depth against accidental use
// of `git prune`, which could remove Git objects that a pool member
// relies on. There is currently no way for us to reliably determine if
// an object is still used anywhere, so the only safe thing to do is to
// assume that every object _is_ used.
func (o *ObjectPool) rescueDanglingObjects(ctx context.Context) (returnedErr error) {
	stderr := &bytes.Buffer{}
	fsck, err := o.Repo.Exec(ctx, gitcmd.Command{
		Name:  "fsck",
		Flags: []gitcmd.Option{gitcmd.Flag{Name: "--connectivity-only"}, gitcmd.Flag{Name: "--dangling"}},
	},
		gitcmd.WithStderr(stderr),
		gitcmd.WithSetupStdout(),
	)
	if err != nil {
		return err
	}

	updater, err := updateref.New(ctx, o.Repo, updateref.WithDisabledTransactions())
	if err != nil {
		return err
	}
	defer func() {
		if err := updater.Close(); err != nil && returnedErr == nil {
			returnedErr = fmt.Errorf("cancel updater: %w", err)
		}
	}()

	if err := updater.Start(); err != nil {
		return fmt.Errorf("start reference transaction: %w", err)
	}

	objectHash, err := o.ObjectHash(ctx)
	if err != nil {
		return fmt.Errorf("detecting object hash: %w", err)
	}

	scanner := bufio.NewScanner(fsck)
	for scanner.Scan() {
		split := strings.SplitN(scanner.Text(), " ", 3)
		if len(split) != 3 {
			continue
		}

		if split[0] != "dangling" {
			continue
		}

		danglingObjectID, err := objectHash.FromHex(split[2])
		if err != nil {
			return fmt.Errorf("parsing object ID %q: %w", split[2], err)
		}

		ref := git.ReferenceName(danglingObjectNamespace + split[2])
		if err := updater.Create(ref, danglingObjectID); err != nil {
			return err
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	if err := fsck.Wait(); err != nil {
		return fmt.Errorf("git fsck: %w, stderr: %q", err, stderr.String())
	}

	return updater.Commit()
}

type referencedObjectTypes struct {
	Blobs   uint64 `json:"blobs"`
	Commits uint64 `json:"commits"`
	Tags    uint64 `json:"tags"`
	Trees   uint64 `json:"trees"`
}

func (o *ObjectPool) logStats(ctx context.Context, when string) error {
	fields := log.Fields{
		"when": when,
	}

	repoInfo, err := stats.RepositoryInfoForRepository(ctx, o.Repo)
	if err != nil {
		return fmt.Errorf("deriving repository info: %w", err)
	}
	fields["repository_info"] = repoInfo

	forEachRef, err := o.Repo.Exec(ctx, gitcmd.Command{
		Name:  "for-each-ref",
		Flags: []gitcmd.Option{gitcmd.Flag{Name: "--format=%(objecttype)%00%(refname)"}},
		Args:  []string{"refs/"},
	}, gitcmd.WithSetupStdout())
	if err != nil {
		return fmt.Errorf("spawning for-each-ref: %w", err)
	}

	var danglingTypes, normalTypes referencedObjectTypes
	scanner := bufio.NewScanner(forEachRef)
	for scanner.Scan() {
		objectType, refname, found := bytes.Cut(scanner.Bytes(), []byte{0})
		if !found {
			continue
		}

		types := &normalTypes
		if bytes.HasPrefix(refname, []byte(danglingObjectNamespace)) {
			types = &danglingTypes
		}

		switch {
		case bytes.Equal(objectType, []byte("blob")):
			types.Blobs++
		case bytes.Equal(objectType, []byte("commit")):
			types.Commits++
		case bytes.Equal(objectType, []byte("tag")):
			types.Tags++
		case bytes.Equal(objectType, []byte("tree")):
			types.Trees++
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scanning references: %w", err)
	}
	if err := forEachRef.Wait(); err != nil {
		return fmt.Errorf("waiting for for-each-ref: %w", err)
	}

	fields["references.dangling"] = danglingTypes
	fields["references.normal"] = normalTypes

	o.logger.WithFields(fields).InfoContext(ctx, "pool dangling ref stats")

	return nil
}
