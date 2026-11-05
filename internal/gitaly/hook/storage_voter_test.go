package hook

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/internal/grpc/backchannel"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// recordingVoter records the phases the hook manager hands to the storage core.
type recordingVoter struct {
	mu          sync.Mutex
	phases      []string
	invocations []string
}

func (v *recordingVoter) record(phase string, payload gitcmd.HooksPayload) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.phases = append(v.phases, phase)
	v.invocations = append(v.invocations, payload.Changelog.Invocation)
}

func (v *recordingVoter) recorded() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.phases...)
}

func (v *recordingVoter) recordedInvocations() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.invocations...)
}

func (v *recordingVoter) TransactionStarts(_ context.Context, payload gitcmd.HooksPayload) {
	v.record("starts", payload)
}

func (v *recordingVoter) Preparing(_ context.Context, payload gitcmd.HooksPayload, _ []byte) {
	v.record("preparing", payload)
}

func (v *recordingVoter) Prepared(_ context.Context, payload gitcmd.HooksPayload, _ []byte, _ changelog.Meta) error {
	v.record("prepared", payload)
	return nil
}

func (v *recordingVoter) Committed(_ context.Context, payload gitcmd.HooksPayload, _ []byte) {
	v.record("committed", payload)
}

func (v *recordingVoter) Aborted(_ context.Context, payload gitcmd.HooksPayload, _ []byte) {
	v.record("aborted", payload)
}

func (v *recordingVoter) Authoritative() bool { return true }

func (v *recordingVoter) DeferReferenceWrites(context.Context, *gitalypb.Repository) (func(), bool) {
	return nil, false
}

func (v *recordingVoter) CreatedRefConflict(context.Context, *gitalypb.Repository, string) (string, error) {
	return "", nil
}

// TestReferenceTransactionHook_storageVoterInvocation checks that every phase hands the storage
// core the invocation of the process, by which it ties the admission to that process.
func TestReferenceTransactionHook_storageVoterInvocation(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repo, _ := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})

	payload := gitcmd.NewHooksPayload(ctx, cfg, repo, gittest.DefaultObjectHash, nil, nil, gitcmd.ReferenceTransactionHook, nil)
	payload.Changelog.Invocation = "the-invocation"
	env, err := payload.Env()
	require.NoError(t, err)

	voter := &recordingVoter{}
	manager := NewManager(
		cfg,
		config.NewLocator(cfg),
		testhelper.SharedLogger(t),
		gittest.NewCommandFactory(t, cfg),
		nil,
		transaction.NewManager(cfg, testhelper.SharedLogger(t), backchannel.NewRegistry()),
		gitlab.NewMockClient(t, gitlab.MockAllowed, gitlab.MockPreReceive, gitlab.MockPostReceive),
		NewPostReceiveRegistry(),
		WithStorageVoter(voter),
	)

	changes := fmt.Sprintf("%s %s refs/heads/main\n", gittest.DefaultObjectHash.ZeroOID, strings.Repeat("b", gittest.DefaultObjectHash.EncodedLen()))
	for _, state := range []ReferenceTransactionState{
		ReferenceTransactionPreparing,
		ReferenceTransactionPrepared,
		ReferenceTransactionCommitted,
		ReferenceTransactionAborted,
	} {
		require.NoError(t, manager.ReferenceTransactionHook(ctx, state, []string{env}, strings.NewReader(changes)))
	}

	require.Equal(t, []string{"starts", "preparing", "prepared", "committed", "aborted"}, voter.recorded())
	require.Equal(t, []string{"the-invocation", "the-invocation", "the-invocation", "the-invocation", "the-invocation"}, voter.recordedInvocations())
}

// TestReferenceTransactionHook_storageVoterPhases checks that the hook manager
// hands every phase of a reference transaction to the storage core, the
// Preparing phase included (doc/graft/PLAN6.md, LOCK AND HISTORY RULE), and
// that a transaction the standalone policy denies is not prepared: nothing is
// uploaded for a transaction that the Prepared phase aborts.
func TestReferenceTransactionHook_storageVoterPhases(t *testing.T) {
	t.Parallel()

	changes := fmt.Sprintf("%s %s refs/heads/main\n", gittest.DefaultObjectHash.ZeroOID, strings.Repeat("b", gittest.DefaultObjectHash.EncodedLen()))

	for _, tc := range []struct {
		desc   string
		policy []config.GraftPolicyRule
		want   []string
		denied bool
	}{
		{
			desc: "every phase reaches the storage core",
			want: []string{"starts", "preparing", "prepared", "committed"},
		},
		{
			desc:   "a denied transaction is neither prepared nor committed, but it starts",
			policy: []config.GraftPolicyRule{{Refs: []string{"refs/heads/main"}, Allow: false}},
			want:   []string{"starts"},
			denied: true,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)
			cfg := testcfg.Build(t)
			cfg.Graft.Standalone = true
			cfg.Graft.Policy = tc.policy

			repo, _ := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})

			payloadCtx := identity.NewContext(ctx, identity.Identity{Name: "agent-a", Source: identity.SourceAgent})
			env, err := gitcmd.NewHooksPayload(payloadCtx, cfg, repo, gittest.DefaultObjectHash, nil, nil, gitcmd.ReferenceTransactionHook, nil).Env()
			require.NoError(t, err)

			voter := &recordingVoter{}
			manager := NewManager(
				cfg,
				config.NewLocator(cfg),
				testhelper.SharedLogger(t),
				gittest.NewCommandFactory(t, cfg),
				nil,
				transaction.NewManager(cfg, testhelper.SharedLogger(t), backchannel.NewRegistry()),
				gitlab.NewMockClient(t, gitlab.MockAllowed, gitlab.MockPreReceive, gitlab.MockPostReceive),
				NewPostReceiveRegistry(),
				WithStorageVoter(voter),
			)

			require.NoError(t, manager.ReferenceTransactionHook(ctx, ReferenceTransactionPreparing, []string{env}, strings.NewReader(changes)))

			err = manager.ReferenceTransactionHook(ctx, ReferenceTransactionPrepared, []string{env}, strings.NewReader(changes))
			if tc.denied {
				require.Equal(t, codes.PermissionDenied, status.Code(err), "error %v", err)
				require.Equal(t, tc.want, voter.recorded())
				return
			}
			require.NoError(t, err)

			require.NoError(t, manager.ReferenceTransactionHook(ctx, ReferenceTransactionCommitted, []string{env}, strings.NewReader(changes)))
			require.Equal(t, tc.want, voter.recorded())
		})
	}
}
