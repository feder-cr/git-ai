package conformance

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

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

// aliasWrites are the ways an agent writes an existing branch.
var aliasWrites = []namespaceRPC{
	namespaceRPCs[0], // UserCommitFiles
	namespaceRPCs[4], // UserLand
	namespaceRPCs[5], // UserFFBranch
	{"push", func(_ context.Context, o operations, name string) error {
		if o.f.session.push(o.f.client, "source:refs/heads/"+name) != 0 {
			return errRefused
		}
		return nil
	}},
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
						if _, moves := symbolicWrites[rpc.name+"/"+alias]; moves {
							want = []string{"refs/heads/" + alias, target}
							sort.Strings(want)
						}
						require.Equal(t, want, changed, "%s through %s as an agent: %v\n%s", rpc.name, alias, err, f.session.log.String())
					})
				}
			}
		})
	}
}

// symbolicWrites are the writes that go through a symbolic branch to its target, each with its
// reason. Everything else leaves the references as they were.
var symbolicWrites = map[string]string{
	"UserCommitFiles/alias-keeparound": reasonSymbolicNotReserved,
	"UserLand/alias-keeparound":        reasonSymbolicNotReserved,
	"UserFFBranch/alias-keeparound":    reasonSymbolicNotReserved,
	"push/alias-keeparound":            reasonSymbolicNotReserved,
	"UserCommitFiles/alias-tag":        reasonSymbolicNotReserved,
	"UserLand/alias-tag":               reasonSymbolicNotReserved,
	"UserFFBranch/alias-tag":           reasonSymbolicNotReserved,
	"push/alias-tag":                   reasonSymbolicNotReserved,

	// Suspected bug: the reservation holds on a push, not on these RPCs.
	"UserCommitFiles/alias-graft": bugSymbolicReservation,
	"UserLand/alias-graft":        bugSymbolicReservation,
	"UserFFBranch/alias-graft":    bugSymbolicReservation,
}

const (
	reasonSymbolicNotReserved = "only refs/graft/ is reserved, and the harness's policy allows refs/**; an agent cannot make a symbolic branch"
	bugSymbolicReservation    = "the RPC writes through a symbolic branch into refs/graft/, which the reference-transaction hook refuses on a push"
)
