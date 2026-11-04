package conformance

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// everyOperationRPC opens every RPC of the matrix to agents, the worst an operator can do with
// [graft] agent_rpcs: what stands between an agent and a namespace is then the server's reservation,
// not the short default list, which agent_methods_test.go covers.
var everyOperationRPC = []string{
	gitalypb.OperationService_UserUpdateBranch_FullMethodName,
	gitalypb.OperationService_UserFFBranch_FullMethodName,
	gitalypb.OperationService_UserMergeBranch_FullMethodName,
	gitalypb.OperationService_UserCherryPick_FullMethodName,
	gitalypb.OperationService_UserRevert_FullMethodName,
	gitalypb.OperationService_UserApplyPatch_FullMethodName,
	gitalypb.OperationService_UserUpdateSubmodule_FullMethodName,
	gitalypb.OperationService_UserMergeToRef_FullMethodName,
	gitalypb.OperationService_UserRebaseToRef_FullMethodName,
	gitalypb.OperationService_UserRebaseConfirmable_FullMethodName,
	gitalypb.OperationService_UserCreateTag_FullMethodName,
	gitalypb.OperationService_UserDeleteTag_FullMethodName,
}

// A namespaceRPC is an RPC of the matrix, called with a name in the field that names what it writes.
type namespaceRPC struct {
	name string
	call func(ctx context.Context, o operations, name string) error
}

var namespaceRPCs = []namespaceRPC{
	{"UserCommitFiles", func(ctx context.Context, o operations, name string) error { return o.commitFiles(ctx, name, "main") }},
	{"UserCreateBranch", func(ctx context.Context, o operations, name string) error { return o.createBranch(ctx, name, "main") }},
	{"UserUpdateBranch", func(ctx context.Context, o operations, name string) error { return o.updateBranch(ctx, name, o.f.main) }},
	{"UserDeleteBranch", func(ctx context.Context, o operations, name string) error { return o.deleteBranch(ctx, name) }},
	{"UserLand", func(ctx context.Context, o operations, name string) error { return o.land(ctx, name) }},
	{"UserFFBranch", func(ctx context.Context, o operations, name string) error { return o.ffBranch(ctx, name) }},
	{"UserMergeBranch", func(ctx context.Context, o operations, name string) error { return o.mergeBranch(ctx, name) }},
	{"UserCherryPick", func(ctx context.Context, o operations, name string) error { return o.cherryPick(ctx, name) }},
	{"UserRevert", func(ctx context.Context, o operations, name string) error { return o.revert(ctx, name) }},
	{"UserApplyPatch", func(ctx context.Context, o operations, name string) error { return o.applyPatch(ctx, name) }},
	{"UserUpdateSubmodule", func(ctx context.Context, o operations, name string) error { return o.updateSubmodule(ctx, name) }},
	{"UserRebaseConfirmable", func(ctx context.Context, o operations, name string) error {
		return o.rebaseConfirmable(ctx, name, o.f.main, "source")
	}},
	{"UserMergeToRef", func(ctx context.Context, o operations, name string) error {
		return o.mergeToRef(ctx, name, "refs/heads/main")
	}},
	{"UserRebaseToRef", func(ctx context.Context, o operations, name string) error {
		return o.rebaseToRef(ctx, name, "refs/heads/main")
	}},
	{"UserCreateTag", func(ctx context.Context, o operations, name string) error { return o.createTag(ctx, name) }},
	{"UserDeleteTag", func(ctx context.Context, o operations, name string) error { return o.deleteTag(ctx, name) }},
}

// namespaces are the namespaces of the matrix: x exists in each of them, new does not.
var namespaces = []string{"refs/graft/", "refs/keep-around/", "refs/tags/"}

// matrixModes are the modes of the matrix: the decision is taken in the agent interceptor and the
// hook, so it depends neither on the dialect of the database nor on whether the core is authoritative
// in anything but what the voter does afterwards.
func matrixModes() []gitaiMode {
	var modes []gitaiMode
	for _, mode := range gitaiModes() {
		if mode.coreMode == "" || mode.name == "authoritative-sqlite" {
			modes = append(modes, mode)
		}
	}
	return modes
}

// seedNamespaces writes refs/graft/x, refs/keep-around/x and refs/tags/x with the shared token, as
// the server's own features do, through the transaction that every mode observes.
func seedNamespaces(t *testing.T, b *gitaiBackend, f *fixture) {
	t.Helper()

	conn := b.operatorConnection(t)
	stream, err := gitalypb.NewRefServiceClient(conn).UpdateReferences(b.ctx)
	require.NoError(t, err)
	var updates []*gitalypb.UpdateReferencesRequest_Update
	for _, namespace := range namespaces {
		updates = append(updates, &gitalypb.UpdateReferencesRequest_Update{
			Reference: []byte(namespace + "x"), NewObjectId: []byte(f.main),
		})
	}
	require.NoError(t, stream.Send(&gitalypb.UpdateReferencesRequest{Repository: f.repo, Updates: updates}))
	_, err = stream.CloseAndRecv()
	require.NoError(t, err)
}

// allowedWrites returns the writes an agent may do with the name, which is a name in one of the
// namespaces. Branch names the server completes under refs/heads/, so that a branch of that odd name
// is a write of a branch and not one of the namespace.
func allowedWrite(rpc string, name string, change string) bool {
	switch rpc {
	case "UserCreateTag", "UserDeleteTag":
		return change == "refs/tags/"+name
	case "UserMergeToRef", "UserRebaseToRef":
		// target_ref names the whole reference. A tag is written as an agent may write a tag.
		return change == name && strings.HasPrefix(name, "refs/tags/")
	default:
		return change == "refs/heads/"+name
	}
}

// TestConformanceAgentNamespaces is the RPC-by-namespace matrix: an agent's RPC names a reference in
// refs/graft/, refs/keep-around/ or refs/tags/, one that exists and one that does not. Whatever the
// RPC answers, the references it may change are those of allowedWrite, and nothing else moves. A push
// to the same namespaces is in the client table.
func TestConformanceAgentNamespaces(t *testing.T) {
	t.Parallel()

	for _, mode := range matrixModes() {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()

			backend := newGitaiBackend(t, mode, agentGraft(everyOperationRPC...))
			client := gitalypb.NewOperationServiceClient(agentConnection(t, backend))

			for _, rpc := range namespaceRPCs {
				for _, namespace := range namespaces {
					for _, leaf := range []string{"x", "new"} {
						name := namespace + leaf
						t.Run(rpc.name+"/"+name, func(t *testing.T) {
							t.Parallel()

							f := newFixture(t, backend.newRemote(t))
							seedNamespaces(t, backend, f)

							before := f.refs()
							err := rpc.call(backend.ctx, operations{client, f}, name)
							changed := changedRefs(before, f.refs())
							t.Logf("MATRIX %s %s %s: %s, changed %v", mode.name, rpc.name, name, outcome(err), changed)

							for _, change := range changed {
								require.True(t, allowedWrite(rpc.name, name, change),
									"%s with %q as an agent moved %s (%v), which it may not", rpc.name, name, change, err)
							}
						})
					}
				}
			}
		})
	}
}

// symbolicAliases are symbolic branches whose targets are in the namespaces of the matrix. An agent
// cannot make one; a server feature or an operator can. A write to the branch writes the target.
var symbolicAliases = map[string]string{
	"alias-graft":      "refs/graft/symbolic",
	"alias-keeparound": "refs/keep-around/symbolic",
	"alias-tag":        "refs/tags/symbolic",
}

// aliasWrites are the ways an agent writes an existing branch: every RPC of the matrix that names a
// branch, and a push.
var aliasWrites = append(rpcsNamed(
	"UserCommitFiles", "UserUpdateBranch", "UserDeleteBranch", "UserLand", "UserFFBranch", "UserMergeBranch",
	"UserCherryPick", "UserRevert", "UserApplyPatch", "UserUpdateSubmodule",
), namespaceRPC{"push", func(_ context.Context, o operations, name string) error {
	if o.f.session.push(o.f.client, "source:refs/heads/"+name) != 0 {
		return errRefused
	}
	return nil
}})

// rpcsNamed returns the RPCs of the matrix with the given names.
func rpcsNamed(names ...string) []namespaceRPC {
	var rpcs []namespaceRPC
	for _, name := range names {
		for _, rpc := range namespaceRPCs {
			if rpc.name == name {
				rpcs = append(rpcs, rpc)
			}
		}
	}
	return rpcs
}

// TestConformanceAgentWritesThroughSymbolicBranches checks the layer below the request filter and
// receive.hideRefs, which see only the name of the request: the reference-transaction hook sees the
// target of a symbolic branch, and refs/graft/ is the server's whatever the mode. It runs without a
// storage core: the symbolic branch is written on disk, which a core would count as a divergence.
func TestConformanceAgentWritesThroughSymbolicBranches(t *testing.T) {
	t.Parallel()

	for _, mode := range matrixModes() {
		if mode.coreMode != "" {
			continue
		}
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()

			backend := newGitaiBackend(t, mode, agentGraft(everyOperationRPC...))
			client := gitalypb.NewOperationServiceClient(agentConnection(t, backend))

			for _, rpc := range aliasWrites {
				for alias, target := range symbolicAliases {
					t.Run(fmt.Sprintf("%s/%s", rpc.name, alias), func(t *testing.T) {
						t.Parallel()

						f := newFixture(t, backend.newRemote(t))
						f.session.local(f.path, "update-ref", target, f.main)
						f.session.local(f.path, "symbolic-ref", "refs/heads/"+alias, target)

						before := f.refs()
						err := rpc.call(backend.ctx, operations{client, f}, alias)
						changed := changedRefs(before, f.refs())
						t.Logf("MATRIX %s %s %s -> %s: %v, changed %v\n%s", mode.name, rpc.name, alias, target, err, changed, f.session.log.String())

						want := []string{}
						if symbolicWriteMoves(rpc.name, alias) {
							want = []string{"refs/heads/" + alias, target}
							sort.Strings(want)
						} else {
							require.Equal(t, symbolicRefusal(rpc.name), outcome(err), "%s through %s as an agent", rpc.name, alias)
						}
						require.Equal(t, want, changed, "%s through %s as an agent: %v\n%s", rpc.name, alias, err, f.session.log.String())
					})
				}
			}
		})
	}
}

// symbolicWriteMoves tells whether a write through a symbolic branch reaches its target. It does when
// only the target's name could stop it: refs/graft/ is the only reserved namespace, the harness's
// policy allows refs/**, and an agent cannot make a symbolic branch. A land does not: it updates
// branches only, and keeps what it knows by branch, so a target that is no branch is refused.
func symbolicWriteMoves(rpc, alias string) bool {
	return alias != "alias-graft" && rpc != "UserLand"
}

// symbolicRefusal is how each write through a symbolic branch is refused, as outcome prints it. A push
// is refused by git, with an exit code. UserUpdateBranch answers FailedPrecondition to every failure
// that is no custom hook's, whatever the cause.
func symbolicRefusal(rpc string) string {
	switch rpc {
	case "UserLand", "UserUpdateBranch":
		return "FailedPrecondition"
	case "push":
		return errRefused.Error()
	default:
		return "PermissionDenied"
	}
}

// TestConformanceAgentCreatesThroughDanglingSymbolicBranches checks the writes that create: a symbolic
// branch whose target does not exist is a branch that does not exist either, and a create through it
// writes the target.
func TestConformanceAgentCreatesThroughDanglingSymbolicBranches(t *testing.T) {
	t.Parallel()

	for _, mode := range matrixModes() {
		if mode.coreMode != "" {
			continue
		}
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()

			backend := newGitaiBackend(t, mode, agentGraft(everyOperationRPC...))
			client := gitalypb.NewOperationServiceClient(agentConnection(t, backend))

			for _, rpc := range append(rpcsNamed("UserCommitFiles", "UserCreateBranch", "UserLand"), aliasWrites[len(aliasWrites)-1]) {
				t.Run(rpc.name, func(t *testing.T) {
					t.Parallel()

					f := newFixture(t, backend.newRemote(t))
					f.session.local(f.path, "symbolic-ref", "refs/heads/alias", "refs/graft/absent")

					before := f.refs()
					err := rpc.call(backend.ctx, operations{client, f}, "alias")
					require.Error(t, err)
					require.Empty(t, changedRefs(before, f.refs()), "%s through a dangling symbolic branch: %v\n%s", rpc.name, err, f.session.log.String())
				})
			}
		})
	}
}

// TestConformancePolicyJudgesTheTargetOfSymbolicBranches gives the agent a branch it may write that
// is a symbolic reference to one it may not. The policy judges the branch that would be written,
// whichever way the write comes, a land in a lane included.
func TestConformancePolicyJudgesTheTargetOfSymbolicBranches(t *testing.T) {
	t.Parallel()

	for _, batching := range []bool{false, true} {
		t.Run(fmt.Sprintf("batching=%v", batching), func(t *testing.T) {
			t.Parallel()

			graft := agentGraft(everyOperationRPC...)
			graft.Policy = append([]config.GraftPolicyRule{
				{Refs: []string{"refs/heads/protected"}, Allow: false},
			}, graft.Policy...)
			graft.Land.Batch.Disabled = !batching
			backend := newGitaiBackend(t, gitaiMode{name: "off-standalone", standalone: true}, graft)
			client := gitalypb.NewOperationServiceClient(agentConnection(t, backend))

			for _, rpc := range aliasWrites {
				t.Run(rpc.name, func(t *testing.T) {
					t.Parallel()

					f := newFixture(t, backend.newRemote(t))
					f.session.local(f.path, "update-ref", "refs/heads/protected", f.main)
					f.session.local(f.path, "symbolic-ref", "refs/heads/alias", "refs/heads/protected")

					before := f.refs()
					err := rpc.call(backend.ctx, operations{client, f}, "alias")
					require.Error(t, err)
					require.Empty(t, changedRefs(before, f.refs()), "%s through a symbolic branch: %v\n%s", rpc.name, err, f.session.log.String())

					// The same write is allowed once the branch points elsewhere: what was refused
					// was the target.
					f.session.local(f.path, "update-ref", "refs/heads/allowed", f.main)
					f.session.local(f.path, "symbolic-ref", "refs/heads/alias", "refs/heads/allowed")
					require.NoError(t, rpc.call(backend.ctx, operations{client, f}, "alias"), f.session.log.String())
					require.NotEqual(t, f.main, f.refs()["refs/heads/allowed"])
					require.Equal(t, f.main, f.refs()["refs/heads/protected"])
				})
			}
		})
	}
}
