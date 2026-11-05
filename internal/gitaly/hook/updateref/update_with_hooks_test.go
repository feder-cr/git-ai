package updateref_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/quarantine"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/gitaly/hook/updateref"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	hookservice "github.com/feder-cr/git-ai/internal/gitaly/service/hook"
	"github.com/feder-cr/git-ai/internal/helper/text"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

func TestUpdaterWithHooks_UpdateReference_invalidParameters(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	gitCmdFactory := gittest.NewCommandFactory(t, cfg)
	repo, _ := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})

	revA := git.ObjectID(strings.Repeat("a", gittest.DefaultObjectHash.EncodedLen()))
	revB := git.ObjectID(strings.Repeat("b", gittest.DefaultObjectHash.EncodedLen()))

	updater := updateref.NewUpdaterWithHooks(cfg, testhelper.NewLogger(t), config.NewLocator(cfg), &hook.MockManager{}, gitCmdFactory, nil)

	testCases := []struct {
		desc           string
		ref            git.ReferenceName
		newRev, oldRev git.ObjectID
		expectedErr    error
	}{
		{
			desc:        "missing reference",
			oldRev:      revA,
			newRev:      revB,
			expectedErr: fmt.Errorf("reference cannot be empty"),
		},
		{
			desc:        "missing old rev",
			ref:         "refs/heads/master",
			newRev:      revB,
			expectedErr: fmt.Errorf("validating old value: %w", git.InvalidObjectIDLengthError{OID: "", CorrectLength: gittest.DefaultObjectHash.EncodedLen(), Length: 0}),
		},
		{
			desc:        "missing new rev",
			ref:         "refs/heads/master",
			oldRev:      revB,
			expectedErr: fmt.Errorf("validating new value: %w", git.InvalidObjectIDLengthError{OID: "", CorrectLength: gittest.DefaultObjectHash.EncodedLen(), Length: 0}),
		},
		{
			desc:        "invalid old rev",
			ref:         "refs/heads/master",
			newRev:      revA,
			oldRev:      "foobar",
			expectedErr: fmt.Errorf("validating old value: %w", git.InvalidObjectIDLengthError{OID: "foobar", CorrectLength: gittest.DefaultObjectHash.EncodedLen(), Length: 6}),
		},
		{
			desc:        "invalid new rev",
			ref:         "refs/heads/master",
			newRev:      "foobar",
			oldRev:      revB,
			expectedErr: fmt.Errorf("validating new value: %w", git.InvalidObjectIDLengthError{OID: "foobar", CorrectLength: gittest.DefaultObjectHash.EncodedLen(), Length: 6}),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			err := updater.UpdateReference(ctx, repo, gittest.TestUser, nil, tc.ref, tc.newRev, tc.oldRev)
			require.Equal(t, tc.expectedErr, err)
		})
	}
}

func TestUpdaterWithHooks_UpdateReference(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)

	// We need to set up a separate "real" hook service here, as it will be used in
	// git-update-ref(1) spawned by `updateRefWithHooks()`
	testserver.RunGitalyServer(t, cfg, func(srv *grpc.Server, deps *service.Dependencies) {
		gitalypb.RegisterHookServiceServer(srv, hookservice.NewServer(deps))
	})

	repo, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})
	commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))

	requirePayload := func(t *testing.T, env []string) {
		require.Len(t, env, 1)

		expectedPayload := gitcmd.NewHooksPayload(
			ctx,
			cfg,
			repo,
			gittest.DefaultObjectHash,
			nil,
			&gitcmd.UserDetails{
				UserID:   gittest.TestUser.GetGlId(),
				Username: gittest.TestUser.GetGlUsername(),
				Protocol: "web",
			},
			gitcmd.ReceivePackHooks,
			featureflag.FromContext(ctx),
		)

		actualPayload, err := gitcmd.HooksPayloadFromEnv(env)
		require.NoError(t, err)

		// Flags aren't sorted, so we just verify they contain the same elements.
		require.ElementsMatch(t, expectedPayload.FeatureFlagsWithValue, actualPayload.FeatureFlagsWithValue)
		expectedPayload.FeatureFlagsWithValue = nil
		actualPayload.FeatureFlagsWithValue = nil

		testhelper.ProtoEqual(t, expectedPayload, actualPayload)
	}

	referenceTransactionCalls := 0
	testCases := []struct {
		desc                 string
		preReceive           func(t *testing.T, ctx context.Context, repo *gitalypb.Repository, pushOptions, env []string, stdin io.Reader, stdout, stderr io.Writer) error
		postReceive          func(t *testing.T, ctx context.Context, repo *gitalypb.Repository, pushOptions, env []string, stdin io.Reader, stdout, stderr io.Writer) error
		update               func(t *testing.T, ctx context.Context, repo *gitalypb.Repository, ref, oldValue, newValue string, env []string, stdout, stderr io.Writer) error
		referenceTransaction func(t *testing.T, ctx context.Context, state hook.ReferenceTransactionState, env []string, stdin io.Reader) error
		expectedErr          string
		expectedRefDeletion  bool
	}{
		{
			desc: "successful update",
			preReceive: func(t *testing.T, ctx context.Context, repo *gitalypb.Repository, pushOptions, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
				changes, err := io.ReadAll(stdin)
				require.NoError(t, err)
				require.Equal(t, fmt.Sprintf("%s %s refs/heads/main\n", commitID, gittest.DefaultObjectHash.ZeroOID.String()), string(changes))
				require.Empty(t, pushOptions)
				requirePayload(t, env)
				return nil
			},
			update: func(t *testing.T, ctx context.Context, repo *gitalypb.Repository, ref, oldValue, newValue string, env []string, stdout, stderr io.Writer) error {
				require.Equal(t, "refs/heads/main", ref)
				require.Equal(t, commitID.String(), oldValue)
				require.Equal(t, newValue, gittest.DefaultObjectHash.ZeroOID.String())
				requirePayload(t, env)
				return nil
			},
			postReceive: func(t *testing.T, ctx context.Context, repo *gitalypb.Repository, pushOptions, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
				changes, err := io.ReadAll(stdin)
				require.NoError(t, err)
				require.Equal(t, fmt.Sprintf("%s %s refs/heads/main\n", commitID.String(), gittest.DefaultObjectHash.ZeroOID.String()), string(changes))
				requirePayload(t, env)
				require.Empty(t, pushOptions)
				return nil
			},
			referenceTransaction: func(t *testing.T, ctx context.Context, state hook.ReferenceTransactionState, env []string, stdin io.Reader) error {
				changes, err := io.ReadAll(stdin)
				require.NoError(t, err)
				require.Equal(t, fmt.Sprintf("%s %s refs/heads/main\n", commitID.String(), gittest.DefaultObjectHash.ZeroOID.String()), string(changes))

				require.Less(t, referenceTransactionCalls, 3)
				switch referenceTransactionCalls {
				case 0:
					require.Equal(t, state, hook.ReferenceTransactionPreparing)
				case 1:
					require.Equal(t, state, hook.ReferenceTransactionPrepared)
				case 2:
					require.Equal(t, state, hook.ReferenceTransactionCommitted)
				}
				referenceTransactionCalls++

				requirePayload(t, env)
				return nil
			},
			expectedRefDeletion: true,
		},
		{
			desc: "prereceive error",
			preReceive: func(t *testing.T, ctx context.Context, repo *gitalypb.Repository, pushOptions, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
				_, err := io.Copy(stderr, strings.NewReader("prereceive failure"))
				require.NoError(t, err)
				return errors.New("ignored")
			},
			expectedErr: "prereceive failure",
		},
		{
			desc: "prereceive error from GitLab API response",
			preReceive: func(t *testing.T, ctx context.Context, repo *gitalypb.Repository, pushOptions, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
				return hook.NotAllowedError{Message: "GitLab: file is locked"}
			},
			expectedErr: "GitLab: file is locked",
		},
		{
			desc: "update error",
			preReceive: func(t *testing.T, ctx context.Context, repo *gitalypb.Repository, pushOptions, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
				return nil
			},
			update: func(t *testing.T, ctx context.Context, repo *gitalypb.Repository, ref, oldValue, newValue string, env []string, stdout, stderr io.Writer) error {
				_, err := io.Copy(stderr, strings.NewReader("update failure"))
				require.NoError(t, err)
				return errors.New("ignored")
			},
			expectedErr: "update failure",
		},
		{
			desc: "reference-transaction error",
			preReceive: func(t *testing.T, ctx context.Context, repo *gitalypb.Repository, pushOptions, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
				return nil
			},
			update: func(t *testing.T, ctx context.Context, repo *gitalypb.Repository, ref, oldValue, newValue string, env []string, stdout, stderr io.Writer) error {
				return nil
			},
			referenceTransaction: func(t *testing.T, ctx context.Context, state hook.ReferenceTransactionState, env []string, stdin io.Reader) error {
				// The reference-transaction hook doesn't execute any custom hooks,
				// which is why it currently doesn't have any stdout/stderr.
				// Instead, errors are directly returned.
				return errors.New("reference-transaction failure")
			},
			expectedErr: "reference-transaction failure",
		},
		{
			desc: "post-receive custom hooks error is ignored",
			preReceive: func(t *testing.T, ctx context.Context, repo *gitalypb.Repository, pushOptions, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
				return nil
			},
			update: func(t *testing.T, ctx context.Context, repo *gitalypb.Repository, ref, oldValue, newValue string, env []string, stdout, stderr io.Writer) error {
				return nil
			},
			referenceTransaction: func(t *testing.T, ctx context.Context, state hook.ReferenceTransactionState, env []string, stdin io.Reader) error {
				return nil
			},
			postReceive: func(t *testing.T, ctx context.Context, repo *gitalypb.Repository, pushOptions, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
				_, err := io.Copy(stderr, strings.NewReader("post-receive failure"))
				require.NoError(t, err)
				return hook.NewCustomHookError(errors.New("ignored"))
			},
			expectedRefDeletion: true,
		},
		{
			desc: "post-receive non-custom hooks error returned",
			preReceive: func(t *testing.T, ctx context.Context, repo *gitalypb.Repository, pushOptions, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
				return nil
			},
			update: func(t *testing.T, ctx context.Context, repo *gitalypb.Repository, ref, oldValue, newValue string, env []string, stdout, stderr io.Writer) error {
				return nil
			},
			referenceTransaction: func(t *testing.T, ctx context.Context, state hook.ReferenceTransactionState, env []string, stdin io.Reader) error {
				return nil
			},
			postReceive: func(t *testing.T, ctx context.Context, repo *gitalypb.Repository, pushOptions, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
				_, err := io.Copy(stderr, strings.NewReader("post-receive failure"))
				require.NoError(t, err)
				return errors.New("uh oh")
			},
			expectedRefDeletion: true,
			expectedErr:         "running post-receive hooks: uh oh",
		},
	}

	for _, tc := range testCases {
		referenceTransactionCalls = 0
		t.Run(tc.desc, func(t *testing.T) {
			hookManager := hook.NewMockManager(t, tc.preReceive, tc.postReceive, tc.update, tc.referenceTransaction)

			gitCmdFactory := gittest.NewCommandFactory(t, cfg)
			updater := updateref.NewUpdaterWithHooks(cfg, testhelper.NewLogger(t), config.NewLocator(cfg), hookManager, gitCmdFactory, nil)

			err := updater.UpdateReference(ctx, repo, gittest.TestUser, nil, git.ReferenceName("refs/heads/main"), gittest.DefaultObjectHash.ZeroOID, commitID)
			if tc.expectedErr == "" {
				require.NoError(t, err)
			} else {
				require.Contains(t, err.Error(), tc.expectedErr)
			}

			if tc.expectedRefDeletion {
				contained, err := localrepo.NewTestRepo(t, cfg, repo).HasRevision(ctx, git.Revision("refs/heads/main"))
				require.NoError(t, err)
				require.False(t, contained, "branch should have been deleted")
				gittest.Exec(t, cfg, "-C", repoPath, "branch", "main", commitID.String())
			} else {
				ref, err := localrepo.NewTestRepo(t, cfg, repo).GetReference(ctx, "refs/heads/main")
				require.NoError(t, err)
				require.Equal(t, ref.Target, commitID.String())
			}
		})
	}
}

// TestUpdaterWithHooks_UpdateReference_abortsTransaction verifies that a transaction which fails
// before it is committed is reported to the reference-transaction hook as aborted, so that the
// hook can release what it set up in the earlier phases.
func TestUpdaterWithHooks_UpdateReference_abortsTransaction(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)

	testCases := []struct {
		desc string
		// setup runs before the update and may make the transaction fail in Prepare.
		setup func(t *testing.T, repoPath string)
		// failPrepared makes the prepared-phase reference-transaction hook return an error.
		failPrepared  bool
		expectedErr   string
		expectedState []hook.ReferenceTransactionState
	}{
		{
			desc:          "prepared hook error aborts the transaction",
			failPrepared:  true,
			expectedErr:   "executing preparatory reference-transaction hook: prepared failure",
			expectedState: []hook.ReferenceTransactionState{hook.ReferenceTransactionPreparing, hook.ReferenceTransactionPrepared, hook.ReferenceTransactionAborted},
		},
		{
			desc: "prepare failure aborts the transaction",
			setup: func(t *testing.T, repoPath string) {
				testhelper.SkipWithReftable(t, "the reference lock is a table lock with reftables")

				// A held lock file makes git-update-ref(1) fail to prepare the transaction.
				require.NoError(t, os.WriteFile(filepath.Join(repoPath, "refs", "heads", "main.lock"), nil, 0o644))
			},
			expectedErr:   "reference update",
			expectedState: []hook.ReferenceTransactionState{hook.ReferenceTransactionPreparing, hook.ReferenceTransactionAborted},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			repo, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
				SkipCreationViaService: true,
			})
			commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))

			if tc.setup != nil {
				tc.setup(t, repoPath)
			}

			var states []hook.ReferenceTransactionState
			hookManager := hook.NewMockManager(t, hook.NopPreReceive, hook.NopPostReceive, hook.NopUpdate,
				func(t *testing.T, ctx context.Context, state hook.ReferenceTransactionState, env []string, stdin io.Reader) error {
					states = append(states, state)
					if tc.failPrepared && state == hook.ReferenceTransactionPrepared {
						return errors.New("prepared failure")
					}
					return nil
				},
			)

			gitCmdFactory := gittest.NewCommandFactory(t, cfg)
			updater := updateref.NewUpdaterWithHooks(cfg, testhelper.NewLogger(t), config.NewLocator(cfg), hookManager, gitCmdFactory, nil)

			err := updater.UpdateReference(ctx, repo, gittest.TestUser, nil, git.ReferenceName("refs/heads/main"), gittest.DefaultObjectHash.ZeroOID, commitID)
			require.ErrorContains(t, err, tc.expectedErr)
			require.Equal(t, tc.expectedState, states)

			// The branch must be left untouched: the transaction was never committed.
			ref, err := localrepo.NewTestRepo(t, cfg, repo).GetReference(ctx, "refs/heads/main")
			require.NoError(t, err)
			require.Equal(t, commitID.String(), ref.Target)
		})
	}
}

// recordingHooks is a hook manager that records the reference each hook was told about, and runs
// duringPreReceive inside the first hook, when the update has been decided on and not yet written.
type recordingHooks struct {
	seen map[string]string
}

func newRecordingHooks(t *testing.T, duringPreReceive func()) (*recordingHooks, hook.Manager) {
	t.Helper()

	recording := &recordingHooks{seen: map[string]string{}}
	record := func(t *testing.T, phase string, stdin io.Reader) {
		changes, err := io.ReadAll(stdin)
		require.NoError(t, err)
		fields := strings.Fields(string(changes))
		require.Len(t, fields, 3, "one change: %q", changes)
		recording.seen[phase] = fields[2]
	}

	return recording, hook.NewMockManager(t,
		func(t *testing.T, ctx context.Context, repo *gitalypb.Repository, pushOptions, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
			record(t, "pre-receive", stdin)
			if duringPreReceive != nil {
				duringPreReceive()
			}
			return nil
		},
		func(t *testing.T, ctx context.Context, repo *gitalypb.Repository, pushOptions, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
			record(t, "post-receive", stdin)
			return nil
		},
		func(t *testing.T, ctx context.Context, repo *gitalypb.Repository, ref, oldValue, newValue string, env []string, stdout, stderr io.Writer) error {
			recording.seen["update"] = ref
			return nil
		},
		func(t *testing.T, ctx context.Context, state hook.ReferenceTransactionState, env []string, stdin io.Reader) error {
			record(t, fmt.Sprintf("reference-transaction %d", state), stdin)
			return nil
		},
	)
}

// everyHookSaw is what recordingHooks holds after an update of reference that committed.
func everyHookSaw(reference string) map[string]string {
	return map[string]string{
		"pre-receive":  reference,
		"update":       reference,
		"post-receive": reference,
		fmt.Sprintf("reference-transaction %d", hook.ReferenceTransactionPreparing): reference,
		fmt.Sprintf("reference-transaction %d", hook.ReferenceTransactionPrepared):  reference,
		fmt.Sprintf("reference-transaction %d", hook.ReferenceTransactionCommitted): reference,
	}
}

// TestUpdaterWithHooks_UpdateReference_symbolicReference checks that the hooks, which decide on the
// update, are told about the reference that is written: for a symbolic reference, its target.
func TestUpdaterWithHooks_UpdateReference_symbolicReference(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)

	setup := func(t *testing.T) (*gitalypb.Repository, string, git.ObjectID, git.ObjectID) {
		repo, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
		oldCommit := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
		newCommit := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(oldCommit))
		gittest.WriteRef(t, cfg, repoPath, "refs/heads/target", oldCommit)
		gittest.WriteRef(t, cfg, repoPath, "refs/heads/elsewhere", oldCommit)
		gittest.Exec(t, cfg, "-C", repoPath, "symbolic-ref", "refs/heads/alias", "refs/heads/target")
		gittest.Exec(t, cfg, "-C", repoPath, "symbolic-ref", "refs/heads/chain", "refs/heads/alias")
		return repo, repoPath, oldCommit, newCommit
	}
	update := func(t *testing.T, repo *gitalypb.Repository, hookManager hook.Manager, reference git.ReferenceName, newCommit, oldCommit git.ObjectID) error {
		updater := updateref.NewUpdaterWithHooks(cfg, testhelper.NewLogger(t), config.NewLocator(cfg), hookManager, gittest.NewCommandFactory(t, cfg), nil)
		return updater.UpdateReference(ctx, repo, gittest.TestUser, nil, reference, newCommit, oldCommit)
	}
	value := func(t *testing.T, repoPath string, reference string) string {
		return text.ChompBytes(gittest.Exec(t, cfg, "-C", repoPath, "rev-parse", "--verify", reference))
	}

	for _, tc := range []struct {
		desc      string
		reference git.ReferenceName
		written   string
	}{
		{desc: "a branch", reference: "refs/heads/target", written: "refs/heads/target"},
		{desc: "a symbolic branch", reference: "refs/heads/alias", written: "refs/heads/target"},
		{desc: "a chain of symbolic branches", reference: "refs/heads/chain", written: "refs/heads/target"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			repo, repoPath, oldCommit, newCommit := setup(t)
			recording, hookManager := newRecordingHooks(t, nil)

			require.NoError(t, update(t, repo, hookManager, tc.reference, newCommit, oldCommit))
			require.Equal(t, everyHookSaw(tc.written), recording.seen)
			require.Equal(t, newCommit.String(), value(t, repoPath, tc.written))
			require.Equal(t, "refs/heads/target", text.ChompBytes(gittest.Exec(t, cfg, "-C", repoPath, "symbolic-ref", "refs/heads/alias")), "the symbolic branch stays symbolic")
		})
	}

	t.Run("a dangling symbolic branch is written at its target", func(t *testing.T) {
		t.Parallel()

		repo, repoPath, _, newCommit := setup(t)
		gittest.Exec(t, cfg, "-C", repoPath, "symbolic-ref", "refs/heads/dangling", "refs/keep/absent")
		recording, hookManager := newRecordingHooks(t, nil)

		require.NoError(t, update(t, repo, hookManager, "refs/heads/dangling", newCommit, gittest.DefaultObjectHash.ZeroOID))
		require.Equal(t, everyHookSaw("refs/keep/absent"), recording.seen)
		require.Equal(t, newCommit.String(), value(t, repoPath, "refs/keep/absent"))
	})

	t.Run("a symbolic branch retargeted after the hooks decided writes what they decided on", func(t *testing.T) {
		t.Parallel()

		repo, repoPath, oldCommit, newCommit := setup(t)
		recording, hookManager := newRecordingHooks(t, func() {
			gittest.Exec(t, cfg, "-C", repoPath, "symbolic-ref", "refs/heads/alias", "refs/heads/elsewhere")
		})

		require.NoError(t, update(t, repo, hookManager, "refs/heads/alias", newCommit, oldCommit))
		require.Equal(t, everyHookSaw("refs/heads/target"), recording.seen)
		require.Equal(t, newCommit.String(), value(t, repoPath, "refs/heads/target"))
		require.Equal(t, oldCommit.String(), value(t, repoPath, "refs/heads/elsewhere"))
	})

	t.Run("a target that turned symbolic after the hooks decided is not followed", func(t *testing.T) {
		t.Parallel()

		repo, repoPath, oldCommit, newCommit := setup(t)
		_, hookManager := newRecordingHooks(t, func() {
			gittest.Exec(t, cfg, "-C", repoPath, "symbolic-ref", "refs/heads/target", "refs/heads/elsewhere")
		})

		require.NoError(t, update(t, repo, hookManager, "refs/heads/alias", newCommit, oldCommit))
		require.Equal(t, oldCommit.String(), value(t, repoPath, "refs/heads/elsewhere"), "nothing the hooks did not see moved")
		require.Equal(t, newCommit.String(), value(t, repoPath, "refs/heads/target"))
	})

	t.Run("an error names the reference asked for", func(t *testing.T) {
		t.Parallel()

		repo, _, _, newCommit := setup(t)
		_, hookManager := newRecordingHooks(t, nil)

		// The old value does not match.
		err := update(t, repo, hookManager, "refs/heads/alias", newCommit, newCommit)
		var updateErr updateref.Error
		require.ErrorAs(t, err, &updateErr)
		require.Equal(t, git.ReferenceName("refs/heads/alias"), updateErr.Reference)
	})
}

func TestUpdaterWithHooks_quarantine(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	gitCmdFactory := gittest.NewCommandFactory(t, cfg)
	locator := config.NewLocator(cfg)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})
	commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))

	unquarantinedRepo := localrepo.NewTestRepo(t, cfg, repoProto)

	quarantine, cleanup, err := quarantine.New(ctx, repoProto, testhelper.NewLogger(t), locator)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	quarantinedRepo := localrepo.NewTestRepo(t, cfg, quarantine.QuarantinedRepo())
	blobID, err := quarantinedRepo.WriteBlob(ctx, strings.NewReader("1834298812398123"), localrepo.WriteBlobConfig{})
	require.NoError(t, err)

	expectQuarantined := func(t *testing.T, env []string, quarantined bool) {
		t.Helper()

		if env != nil {
			payload, err := gitcmd.HooksPayloadFromEnv(env)
			require.NoError(t, err)
			if quarantined {
				testhelper.ProtoEqual(t, quarantine.QuarantinedRepo(), payload.Repo)
			} else {
				testhelper.ProtoEqual(t, repoProto, payload.Repo)
			}
		}

		exists, err := quarantinedRepo.HasRevision(ctx, blobID.Revision()+"^{blob}")
		require.NoError(t, err)
		require.Equal(t, quarantined, exists)

		exists, err = unquarantinedRepo.HasRevision(ctx, blobID.Revision()+"^{blob}")
		require.NoError(t, err)
		require.Equal(t, !quarantined, exists)
	}

	hookExecutions := make(map[string]int)
	hookManager := hook.NewMockManager(t,
		// The pre-receive hook is not expected to have the object in the normal repo.
		func(t *testing.T, ctx context.Context, repo *gitalypb.Repository, pushOptions, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
			expectQuarantined(t, env, true)
			testhelper.ProtoEqual(t, quarantine.QuarantinedRepo(), repo)
			hookExecutions["prereceive"]++
			return nil
		},
		// But the post-receive hook shall get the unquarantined repository as input, with
		// objects already having been migrated into the target repo.
		func(t *testing.T, ctx context.Context, repo *gitalypb.Repository, pushOptions, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
			expectQuarantined(t, env, false)
			testhelper.ProtoEqual(t, repoProto, repo)
			hookExecutions["postreceive"]++
			return nil
		},
		// The update hook gets executed after the pre-receive hook and will be executed for
		// each reference that we're updating. As it is called immediately before the ref
		// gets queued for update, objects must have already been migrated or otherwise
		// updating the refs will fail due to missing objects.
		func(t *testing.T, ctx context.Context, repo *gitalypb.Repository, ref, oldValue, newValue string, env []string, stdout, stderr io.Writer) error {
			expectQuarantined(t, env, false)
			testhelper.ProtoEqual(t, quarantine.QuarantinedRepo(), repo)
			hookExecutions["update"]++
			return nil
		},
		// The reference-transaction hook is called as we're queueing refs for update, so
		// the objects must be part of the main object database or otherwise the update will
		// fail due to missing objects.
		func(t *testing.T, ctx context.Context, state hook.ReferenceTransactionState, env []string, stdin io.Reader) error {
			expectQuarantined(t, env, false)
			switch state {
			case hook.ReferenceTransactionPrepared:
				hookExecutions["prepare"]++
			case hook.ReferenceTransactionCommitted:
				hookExecutions["commit"]++
			}
			return nil
		},
	)

	require.NoError(t, updateref.NewUpdaterWithHooks(cfg, testhelper.NewLogger(t), locator, hookManager, gitCmdFactory, nil).UpdateReference(
		ctx,
		repoProto,
		&gitalypb.User{
			GlId:       "1234",
			GlUsername: "Username",
			Name:       []byte("Name"),
			Email:      []byte("mail@example.com"),
		},
		quarantine,
		git.ReferenceName("refs/heads/main"),
		gittest.DefaultObjectHash.ZeroOID,
		commitID,
	))

	require.Equal(t, map[string]int{
		"prereceive":  1,
		"postreceive": 1,
		"update":      1,
		"prepare":     1,
		"commit":      1,
	}, hookExecutions)

	contained, err := unquarantinedRepo.HasRevision(ctx, git.Revision("refs/heads/main"))
	require.NoError(t, err)
	require.False(t, contained, "branch should have been deleted")
}
