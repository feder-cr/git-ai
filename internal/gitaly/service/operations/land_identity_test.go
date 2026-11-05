package operations

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/grpc/metadata"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/internal/transaction/txinfo"
	"github.com/feder-cr/git-ai/internal/transaction/voting"
)

// TestLandBatch_retryAfterALostResponse lands two changes by rebase in one batch, and then lands
// them again, as clients do whose responses were lost: the batch records the identity of every
// member (doc/graft/PLAN11.md), and each retried member answers already landed as the commit its
// first land put on the branch.
func TestLandBatch_retryAfterALostResponse(t *testing.T) {
	t.Parallel()

	runLandBatchTest(t, testLandBatchRetryAfterALostResponse)
}

func testLandBatchRetryAfterALostResponse(t *testing.T, ctx context.Context) {
	t.Parallel()

	ctx, cfg, client := setupAgentLandService(t, ctx, agentBatchingGraft(2, nil))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	baseEntry := gittest.TreeEntry{Mode: "100644", Path: "base", Content: "base"}
	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch), gittest.WithTreeEntries(baseEntry))
	gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch), gittest.WithParents(base),
		gittest.WithTreeEntries(baseEntry, gittest.TreeEntry{Mode: "100644", Path: "moved", Content: "moved"}))
	change := func(name string) git.ObjectID {
		return gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base),
			gittest.WithTreeEntries(baseEntry, gittest.TreeEntry{Mode: "100644", Path: name, Content: name}))
	}
	a, b := change("a"), change("b")

	first, errs := landInOrder(t, ctx, client, repoProto,
		agentLanding{agent: "agent-a", commit: a},
		agentLanding{agent: "agent-b", commit: b},
	)
	for i := range errs {
		require.NoError(t, errs[i])
		require.Equal(t, int32(2), first[i].GetBatchSize(), "both changes land in one batch")
		require.True(t, first[i].GetRewritten())
	}
	landedA, landedB := git.ObjectID(first[0].GetLandedCommitId()), git.ObjectID(first[1].GetLandedCommitId())

	// The batch lands its members in the order they reached the lane: the tip is the commit of
	// agent-b, the last of them.
	tip := gittest.ResolveRevision(t, cfg, repoPath, git.DefaultRef.String())
	require.Equal(t, landedB, tip)

	entry := landEntry(t, repoPath, tip)
	require.Equal(t, changelog.OperationLandBatch, entry.Operation)
	require.ElementsMatch(t, []changelog.Land{
		{Original: a.String(), Landed: landedA.String()},
		{Original: b.String(), Landed: landedB.String()},
	}, entry.Lands)
	require.Equal(t, tip.String(), entry.Lands[len(entry.Lands)-1].Landed, "the lands are in the order of the batch")

	second, errs := landInOrder(t, ctx, client, repoProto,
		agentLanding{agent: "agent-a", commit: a},
		agentLanding{agent: "agent-b", commit: b},
	)
	for i, landed := range []git.ObjectID{landedA, landedB} {
		require.NoError(t, errs[i])
		requireLandedEarlier(t, second[i], entry, landed, "")
		require.Equal(t, tip.String(), second[i].GetBranchUpdate().GetCommitId())
	}
	requireBranchAt(t, cfg, repoPath, git.DefaultBranch, tip)
}

// TestUserLand_intentAnswersWhatTheChangelogMisses lands a change whose update git committed and
// whose changelog entry is lost, as a crash between the two leaves it (doc/graft/PLAN11.md, 4.2):
// the intent the hook recorded before git committed answers the retry.
func TestUserLand_intentAnswersWhatTheChangelogMisses(t *testing.T) {
	t.Parallel()

	runLandBatchTest(t, testUserLandIntentAnswersWhatTheChangelogMisses)
}

func testUserLandIntentAnswersWhatTheChangelogMisses(t *testing.T, ctx context.Context) {
	t.Parallel()

	ctx, cfg, client := setupOperationsService(t, ctx)
	fx := newRetryFixture(t, ctx, cfg)

	request := landRequest(fx.repoProto, git.DefaultBranch, fx.change)
	first, err := client.UserLand(ctx, request)
	require.NoError(t, err)
	landed := git.ObjectID(first.GetLandedCommitId())

	// The changelog lost the entry of the land, its only entry.
	require.NoError(t, os.Remove(changelog.Path(fx.repoPath)))

	second, err := client.UserLand(ctx, request)
	require.NoError(t, err)
	require.True(t, second.GetAlreadyLanded())
	require.Equal(t, landed.String(), second.GetLandedCommitId())
	require.Zero(t, second.GetLandedSequence())
	require.Equal(t, fmt.Sprintf("landed as %s, which the changelog does not record", landed), second.GetAlreadyLandedDetail())
	requireBranchAt(t, cfg, fx.repoPath, git.DefaultBranch, landed)
}

// TestUserLand_intentOfAnUncommittedUpdateAnswersNothing aborts a land after the prepared phase of
// its update, which leaves its intent: the intent's landed commit exists (the objects of an update
// are migrated before it is prepared) but is not on the branch, so the next land lands the change.
func TestUserLand_intentOfAnUncommittedUpdateAnswersNothing(t *testing.T) {
	t.Parallel()

	runLandBatchTest(t, testUserLandIntentOfAnUncommittedUpdateAnswersNothing)
}

func testUserLandIntentOfAnUncommittedUpdateAnswersNothing(t *testing.T, ctx context.Context) {
	t.Parallel()

	txManager := transaction.MockManager{}
	ctx, cfg, client := setupOperationsService(t, ctx, testserver.WithTransactionManager(&txManager))

	ctx, err := txinfo.InjectTransaction(ctx, 1, "node", true)
	require.NoError(t, err)
	ctx = metadata.IncomingToOutgoing(ctx)

	// The repository is set up without the transaction failing.
	txManager.VoteFn = func(context.Context, txinfo.Transaction, voting.Vote, voting.Phase) error { return nil }
	fx := newRetryFixture(t, ctx, cfg)

	// The first land fails its vote in the prepared phase of its update, after the hook recorded
	// its intent: git aborts the update. Its objects were migrated before the update, so its
	// landed commit exists, off the branch.
	var failed atomic.Bool
	txManager.VoteFn = func(_ context.Context, _ txinfo.Transaction, _ voting.Vote, phase voting.Phase) error {
		if phase == voting.Prepared && failed.CompareAndSwap(false, true) {
			return errors.New("the transaction lost its quorum")
		}
		return nil
	}

	request := landRequest(fx.repoProto, git.DefaultBranch, fx.change)
	_, err = client.UserLand(ctx, request)
	require.Error(t, err)
	require.True(t, failed.Load())
	requireBranchAt(t, cfg, fx.repoPath, git.DefaultBranch, fx.tip)

	records, err := changelog.LookupLand(fx.repoPath, git.DefaultRef.String(), fx.change.String())
	require.NoError(t, err)
	require.Len(t, records, 1, "the aborted update left its intent")
	aborted := records[0].Landed
	require.Equal(t, "commit\n", string(gittest.Exec(t, cfg, "-C", fx.repoPath, "cat-file", "-t", aborted)), "the aborted commit exists")

	// The intent's commit is not on the branch, so the land lands the change.
	second, err := client.UserLand(ctx, request)
	require.NoError(t, err)
	require.False(t, second.GetAlreadyLanded())
	require.True(t, second.GetRewritten())
	requireBranchAt(t, cfg, fx.repoPath, git.DefaultBranch, git.ObjectID(second.GetLandedCommitId()))
}

// TestUserLand_identityOfABranchWhoseNameIsNotUTF8 lands a change by rebase on a branch whose name
// is not valid UTF-8, through the hooks: its identity is recorded with the name byte for byte, and
// the retry answers from it.
func TestUserLand_identityOfABranchWhoseNameIsNotUTF8(t *testing.T) {
	t.Parallel()

	runLandBatchTest(t, testUserLandIdentityOfABranchWhoseNameIsNotUTF8)
}

func testUserLandIdentityOfABranchWhoseNameIsNotUTF8(t *testing.T, ctx context.Context) {
	t.Parallel()

	ctx, cfg, client := setupOperationsService(t, ctx)
	fx := newRetryFixture(t, ctx, cfg)
	branch := "f\xffo"
	gittest.Exec(t, cfg, "-C", fx.repoPath, "update-ref", "refs/heads/"+branch, fx.tip.String())

	request := landRequest(fx.repoProto, branch, fx.change)
	first, err := client.UserLand(ctx, request)
	require.NoError(t, err)
	require.True(t, first.GetRewritten())
	landed := git.ObjectID(first.GetLandedCommitId())

	entries, err := changelog.List(fx.repoPath, 0, 100)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "refs/heads/"+branch, entries[0].Reference)
	require.Equal(t, []changelog.Land{{Original: fx.change.String(), Landed: landed.String()}}, entries[0].Lands)

	second, err := client.UserLand(ctx, request)
	require.NoError(t, err)
	requireLandedEarlier(t, second, entries[0], landed, "")
}

// TestUserLand_concurrentRetries lands the same change twice at once, as a retry that reaches the
// server while the first call still runs: the change lands once, and the other call answers
// already landed with the commit it landed as.
func TestUserLand_concurrentRetries(t *testing.T) {
	t.Parallel()

	runLandBatchTest(t, testUserLandConcurrentRetries)
}

func testUserLandConcurrentRetries(t *testing.T, ctx context.Context) {
	t.Parallel()

	for _, tc := range []struct {
		desc  string
		graft config.Graft
	}{
		{desc: "unbatched", graft: config.Graft{}},
		{desc: "batched", graft: agentBatchingGraft(2, nil)},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			ctx, cfg, client := setupAgentLandService(t, ctx, tc.graft)
			for round := 0; round < 5; round++ {
				fx := newRetryFixture(t, ctx, cfg)
				before := gittest.Exec(t, cfg, "-C", fx.repoPath, "rev-list", "--count", git.DefaultBranch)

				responses, errs := landInOrder(t, ctx, client, fx.repoProto,
					agentLanding{commit: fx.change},
					agentLanding{commit: fx.change},
				)
				require.NoError(t, errs[0])
				require.NoError(t, errs[1])

				landed := 0
				for _, resp := range responses {
					if !resp.GetAlreadyLanded() {
						landed++
						require.True(t, resp.GetRewritten())
					}
				}
				require.Equal(t, 1, landed, "round %d: the change lands once", round)
				require.Equal(t, responses[0].GetLandedCommitId(), responses[1].GetLandedCommitId(), "round %d", round)

				tip := gittest.ResolveRevision(t, cfg, fx.repoPath, git.DefaultRef.String())
				require.Equal(t, responses[0].GetLandedCommitId(), tip.String(), "round %d", round)
				require.Equal(t, mustAtoi(t, before)+1, mustAtoi(t, gittest.Exec(t, cfg, "-C", fx.repoPath, "rev-list", "--count", git.DefaultBranch)), "round %d", round)
			}
		})
	}
}

// mustAtoi parses the decimal number of a git output.
func mustAtoi(t *testing.T, output []byte) int {
	t.Helper()

	n, err := strconv.Atoi(strings.TrimSpace(string(output)))
	require.NoError(t, err)
	return n
}

// TestUserLand_branchNameTooLongForTheIdentity refuses, when it arrives, a land on a branch whose
// name is too long for the identity of its update to be recorded.
func TestUserLand_branchNameOverTheReferenceNameLimit(t *testing.T) {
	t.Parallel()

	ctx, cfg, client := setupOperationsService(t, testhelper.Context(t))
	fx := newRetryFixture(t, ctx, cfg)

	branch := strings.TrimSuffix(strings.Repeat(strings.Repeat("a", 200)+"/", 35), "/")
	_, err := client.UserLand(ctx, landRequest(fx.repoProto, branch, fx.change))
	testhelper.RequireGrpcCode(t, err, codes.InvalidArgument)
	// A name this long is over the limit on reference names, which the server applies first.
	// changelog.TestValidateLands covers the check of the identity's size itself.
	require.ErrorContains(t, err, "over the limit of 1024 bytes")
	require.Less(t, len(err.Error()), 512, "the error does not quote the whole name")
}

// TestLandBatch_intentAnswersWhatTheChangelogMisses lands two changes in one batch, loses the
// changelog entry of their update, and lands them again: the intents the hook recorded for the
// batch's update answer.
func TestLandBatch_intentAnswersWhatTheChangelogMisses(t *testing.T) {
	t.Parallel()

	runLandBatchTest(t, testLandBatchIntentAnswersWhatTheChangelogMisses)
}

func testLandBatchIntentAnswersWhatTheChangelogMisses(t *testing.T, ctx context.Context) {
	t.Parallel()

	ctx, cfg, client := setupAgentLandService(t, ctx, agentBatchingGraft(2, nil))
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	baseEntry := gittest.TreeEntry{Mode: "100644", Path: "base", Content: "base"}
	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch), gittest.WithTreeEntries(baseEntry))
	gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch), gittest.WithParents(base),
		gittest.WithTreeEntries(baseEntry, gittest.TreeEntry{Mode: "100644", Path: "moved", Content: "moved"}))
	change := func(name string) git.ObjectID {
		return gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base),
			gittest.WithTreeEntries(baseEntry, gittest.TreeEntry{Mode: "100644", Path: name, Content: name}))
	}
	a, b := change("a"), change("b")

	first, errs := landInOrder(t, ctx, client, repoProto,
		agentLanding{agent: "agent-a", commit: a},
		agentLanding{agent: "agent-b", commit: b},
	)
	for i := range errs {
		require.NoError(t, errs[i])
		require.Equal(t, int32(2), first[i].GetBatchSize(), "both changes land in one batch")
	}
	tip := gittest.ResolveRevision(t, cfg, repoPath, git.DefaultRef.String())

	// The changelog lost the entry of the batch's update, its only entry.
	require.NoError(t, os.Remove(changelog.Path(repoPath)))

	second, errs := landInOrder(t, ctx, client, repoProto,
		agentLanding{agent: "agent-a", commit: a},
		agentLanding{agent: "agent-b", commit: b},
	)
	for i := range errs {
		require.NoError(t, errs[i])
		require.True(t, second[i].GetAlreadyLanded())
		require.Equal(t, first[i].GetLandedCommitId(), second[i].GetLandedCommitId())
		require.Zero(t, second[i].GetLandedSequence())
	}
	requireBranchAt(t, cfg, repoPath, git.DefaultBranch, tip)
}

// TestLandBatchExecutorRetryInTheBatchOfItsFirstLand puts a request and its retry in one batch:
// the retry answers already landed with the commit of the first, and only when the batch's update
// holds.
func TestLandBatchExecutorRetryInTheBatchOfItsFirstLand(t *testing.T) {
	t.Parallel()

	t.Run("the update holds", func(t *testing.T) {
		fake := &fakeLandStepper{tip: "main0"}
		outcomes := newFakeLandExecutor(fake, 3).Execute(context.Background(), testLandRequests(false, "c1", "c1"))

		require.NoError(t, outcomes[0].Err)
		require.False(t, outcomes[0].AlreadyLanded)
		require.NoError(t, outcomes[1].Err)
		require.True(t, outcomes[1].AlreadyLanded)
		require.Equal(t, git.ObjectID("c1"), outcomes[1].LandedCommit)
		require.Len(t, fake.updates, 1)
		require.Equal(t, []string{"c1"}, fake.updates[0].members2Commits(), "the retry is not a member")
	})

	t.Run("the update fails", func(t *testing.T) {
		// The first land fails with the update. Its retry is not answered as landed: it lands
		// again, alone, onto the real tip.
		fake := &fakeLandStepper{tip: "main0", script: []scriptedUpdate{{kind: landUpdateFailed, err: errors.New("disk full")}}}
		outcomes := newFakeLandExecutor(fake, 3).Execute(context.Background(), testLandRequests(false, "c1", "c1"))

		require.ErrorContains(t, outcomes[0].Err, "disk full")
		require.NoError(t, outcomes[1].Err)
		require.False(t, outcomes[1].AlreadyLanded)
		require.Equal(t, git.ObjectID("c1"), outcomes[1].LandedCommit)
		require.Len(t, fake.updates, 2)
		require.Equal(t, []string{"c1"}, fake.updates[1].members2Commits())
	})
}

// TestLandBatch_retryOfABatchOfOne retries a change that landed alone in its lane, which publishes
// it as a land of its own (UpdateSolo): its identity is recorded, and the retry answers from it.
func TestLandBatch_retryOfABatchOfOne(t *testing.T) {
	t.Parallel()

	runLandBatchTest(t, testLandBatchRetryOfABatchOfOne)
}

func testLandBatchRetryOfABatchOfOne(t *testing.T, ctx context.Context) {
	t.Parallel()

	ctx, cfg, client := setupAgentLandService(t, ctx, batchingGraft())
	fx := newRetryFixture(t, ctx, cfg)

	request := landRequest(fx.repoProto, git.DefaultBranch, fx.change)
	first, err := client.UserLand(ctx, request)
	require.NoError(t, err)
	require.Equal(t, int32(1), first.GetBatchSize())
	require.True(t, first.GetRewritten())
	landed := git.ObjectID(first.GetLandedCommitId())

	entry := landEntry(t, fx.repoPath, landed)
	require.Equal(t, changelogOperationLand, entry.Operation)
	require.Equal(t, []changelog.Land{{Original: fx.change.String(), Landed: landed.String()}}, entry.Lands)

	second, err := client.UserLand(ctx, request)
	require.NoError(t, err)
	requireLandedEarlier(t, second, entry, landed, "")
	requireBranchAt(t, cfg, fx.repoPath, git.DefaultBranch, landed)
}
