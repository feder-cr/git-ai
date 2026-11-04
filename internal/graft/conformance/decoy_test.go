package conformance

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// operationRPCCoverage says, for every RPC of OperationService, where its handling of a decoy is
// tested: git's DWIM rules must not let a reference such as refs/tags/refs/heads/x answer for the
// branch x. A new RPC fails TestConformanceDecoyCoverageIsComplete until it is classified here.
var operationRPCCoverage = map[string]string{
	"UserCommitFiles":       "TestUserCommitFilesResolvesBranchesExactly, operations/commit_files_exact_test.go",
	"UserLand":              "TestUserLandResolvesBranchesExactly, operations/land_exact_test.go",
	"UserFFBranch":          "TestUserFFBranchResolvesBranchesExactly, operations/branch_exact_test.go",
	"UserMergeBranch":       "TestUserMergeBranchResolvesBranchesExactly, operations/branch_exact_test.go",
	"UserDeleteBranch":      "TestUserDeleteBranchResolvesBranchesExactly, operations/branch_exact_test.go",
	"UserCherryPick":        "TestUserCherryPickResolvesBranchesExactly, operations/branch_exact_test.go",
	"UserRevert":            "TestUserRevertResolvesBranchesExactly, operations/branch_exact_test.go",
	"UserUpdateSubmodule":   "TestUserUpdateSubmoduleResolvesBranchesExactly, operations/branch_exact_test.go",
	"UserApplyPatch":        "TestUserApplyPatchResolvesBranchesExactly, operations/branch_exact_test.go",
	"UserCreateBranch":      "decoyCalls in this file",
	"UserUpdateBranch":      "decoyCalls in this file",
	"UserRebaseConfirmable": "decoyCalls in this file",
	"UserMergeToRef":        "decoyCalls in this file",
	"UserRebaseToRef":       "decoyCalls in this file",
	"UserCreateTag":         "no branch name: tag_name is a tag, target_revision is a revision by contract",
	"UserDeleteTag":         "no branch name: tag_name is a tag",
	"UserSquash":            "no branch name: the request names commits",
}

// TestConformanceDecoyCoverageIsComplete fails on an RPC of OperationService that says nothing about
// its decoys.
func TestConformanceDecoyCoverageIsComplete(t *testing.T) {
	t.Parallel()

	var rpcs []string
	for _, method := range gitalypb.OperationService_ServiceDesc.Methods {
		rpcs = append(rpcs, method.MethodName)
	}
	for _, stream := range gitalypb.OperationService_ServiceDesc.Streams {
		rpcs = append(rpcs, stream.StreamName)
	}
	sort.Strings(rpcs)

	var classified []string
	for rpc := range operationRPCCoverage {
		classified = append(classified, rpc)
	}
	sort.Strings(classified)
	require.Equal(t, rpcs, classified)
}

// A decoyCall calls an RPC with the branch name x in one of its fields.
type decoyCall struct {
	name string
	call func(ctx context.Context, o operations, x string) error
}

// decoyCalls are the fields that take a branch name of the RPCs that no earlier test covers.
var decoyCalls = []decoyCall{
	{"UserCreateBranch/branch_name", func(ctx context.Context, o operations, x string) error {
		return o.createBranch(ctx, x, "main")
	}},
	{"UserCreateBranch/start_point", func(ctx context.Context, o operations, x string) error {
		return o.createBranch(ctx, "created", x)
	}},
	{"UserUpdateBranch/branch_name", func(ctx context.Context, o operations, x string) error {
		return o.updateBranch(ctx, x, o.f.main)
	}},
	{"UserMergeToRef/first_parent_ref", func(ctx context.Context, o operations, x string) error {
		return o.mergeToRef(ctx, "refs/merge-requests/1/merge", "refs/heads/"+x)
	}},
	{"UserRebaseToRef/first_parent_ref", func(ctx context.Context, o operations, x string) error {
		return o.rebaseToRef(ctx, "refs/merge-requests/1/rebase", "refs/heads/"+x)
	}},
	{"UserRebaseConfirmable/branch", func(ctx context.Context, o operations, x string) error {
		return o.rebaseConfirmable(ctx, x, o.f.main, "source")
	}},
	{"UserRebaseConfirmable/remote_branch", func(ctx context.Context, o operations, x string) error {
		return o.rebaseConfirmable(ctx, "source", o.f.source, x)
	}},
}

// decoyScenarios are the references that git's DWIM rules take for the branch x: the first four
// answer for a missing refs/heads/x, the last one for the bare name of a branch x that exists.
var decoyScenarios = []struct {
	decoy        string
	branchExists bool
}{
	{decoy: "refs/refs/heads/x"},
	{decoy: "refs/tags/refs/heads/x"},
	{decoy: "refs/heads/refs/heads/x"},
	{decoy: "refs/remotes/refs/heads/x"},
	{decoy: "refs/tags/x", branchExists: true},
}

// decoyState is what a request did, without what differs between two identical requests: the commits
// it wrote are compared by their trees and parents' count, because their dates differ.
func decoyState(t *testing.T, f *fixture, decoy string, call func() error) string {
	t.Helper()

	before := f.refs()
	err := call()
	after := f.refs()

	lines := []string{"outcome " + outcome(err)}
	for _, name := range changedRefs(before, after) {
		if name == decoy {
			lines = append(lines, "the decoy moved")
			continue
		}
		value, ok := after[name]
		if !ok {
			lines = append(lines, name+" deleted")
			continue
		}
		description := f.session.local(f.path, "log", "-1", "--format=%T %P", value)
		tree, parents, _ := strings.Cut(description, " ")
		lines = append(lines, name+" tree "+tree+" parents "+parents)
	}
	return strings.Join(lines, "\n")
}

// TestConformanceDecoys calls each RPC twice, on twin repositories that differ only in the decoy, at
// a commit unrelated to the history of the branches. A request has to behave as if the decoy were
// not there. The decoys are written on disk, so that it runs without a storage core: the resolution
// of a name does not depend on the mode.
func TestConformanceDecoys(t *testing.T) {
	t.Parallel()

	backend := newGitaiBackend(t, gitaiMode{name: "off-gitlab"}, agentGraft())
	client := gitalypb.NewOperationServiceClient(backend.operatorConnection(t))

	for _, call := range decoyCalls {
		for _, scenario := range decoyScenarios {
			t.Run(call.name+"/"+scenario.decoy, func(t *testing.T) {
				t.Parallel()

				run := func(withDecoy bool) string {
					f := newFixture(t, backend.newRemote(t))
					if scenario.branchExists {
						f.session.local(f.path, "update-ref", "refs/heads/x", f.main)
					}
					if withDecoy {
						unrelated := f.session.local(f.client, "commit-tree", "-m", "decoy", f.session.local(f.client, "rev-parse", "main~3^{tree}"))
						f.session.step(receivePack, f.client, false, "push", "origin", unrelated+":refs/heads/decoy")
						f.session.local(f.path, "update-ref", scenario.decoy, unrelated)
						f.session.local(f.path, "update-ref", "-d", "refs/heads/decoy")
					}
					return decoyState(t, f, scenario.decoy, func() error {
						// setupCtx: UserRebaseConfirmable reads its remote repository through the
						// gitaly-servers metadata, which only the shared token may send.
						return call.call(backend.setupCtx, operations{client, f}, "x")
					})
				}

				want, got := run(false), run(true)
				t.Logf("DECOY %s %s: %q", call.name, scenario.decoy, got)
				if _, known := knownDecoyAnswers[call.name+" "+scenario.decoy]; known {
					require.NotEqual(t, want, got, "the decoy no longer answers: remove the entry of knownDecoyAnswers")
					return
				}
				require.Equal(t, want, got, "%s answered differently with the decoy %s", call.name, scenario.decoy)
			})
		}
	}
}

// knownDecoyAnswers are the decoys that still answer for a branch: suspected bugs, listed so that the
// harness stays usable while they are open. An entry fails as soon as the decoy stops answering.
var knownDecoyAnswers = map[string]string{}
