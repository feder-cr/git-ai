package conformance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/testhelper"
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

// TestConformanceRefusalsDoNotTellTheTarget writes as an agent through a symbolic branch into a
// reference it does not see: the refusal, with its details, does not name the reference. A policy that
// denies the namespace reports the changes it was asked about, so it takes part.
func TestConformanceRefusalsDoNotTellTheTarget(t *testing.T) {
	t.Parallel()

	graft := agentGraft(everyOperationRPC...)
	graft.Policy = append([]config.GraftPolicyRule{{Refs: []string{"refs/graft/**"}, Allow: false}}, graft.Policy...)
	backend := newGitaiBackend(t, gitaiMode{name: "off-standalone", standalone: true}, graft)
	client := gitalypb.NewOperationServiceClient(agentConnection(t, backend))

	for _, rpc := range aliasWrites {
		if rpc.name == "push" {
			continue
		}
		t.Run(rpc.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t, backend.newRemote(t))
			f.session.local(f.path, "update-ref", "refs/graft/hidden/secret", f.main)
			f.session.local(f.path, "symbolic-ref", "refs/heads/alias", "refs/graft/hidden/secret")

			err := rpc.call(backend.ctx, operations{client, f}, "alias")
			require.Error(t, err)
			response := fmt.Sprint(status.Convert(err).Proto())
			require.NotContains(t, response, "secret")
			require.NotContains(t, response, "hidden")
		})
	}
}

// TestConformanceNameTooLongForTheFileSystem names a branch git cannot store as a file: git refuses
// it, not the server's read of whether it is symbolic, whose failure would tell where the repository
// is. A limit of this test: the details of the answer hold git-update-ref(1)'s stderr, with the path
// that git failed to lock, as any failure of that command does (updateref.closeWithError). So only
// the message is compared.
func TestConformanceNameTooLongForTheFileSystem(t *testing.T) {
	t.Parallel()

	backend := newGitaiBackend(t, gitaiMode{name: "off-standalone", standalone: true}, agentGraft(everyOperationRPC...))
	client := gitalypb.NewOperationServiceClient(agentConnection(t, backend))

	for _, rpc := range rpcsNamed("UserCommitFiles", "UserCreateBranch", "UserUpdateBranch", "UserDeleteBranch", "UserLand") {
		t.Run(rpc.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t, backend.newRemote(t))
			before := f.refs()
			err := rpc.call(backend.ctx, operations{client, f}, strings.Repeat("x", 300))
			if testhelper.IsReftableEnabled() && err == nil {
				// A reftable stores the name.
				return
			}
			require.Error(t, err)
			require.NotContains(t, status.Convert(err).Message(), backend.cfg.Storages[0].Path)
			require.NotContains(t, status.Convert(err).Message(), "loose reference")
			require.Empty(t, changedRefs(before, f.refs()))
		})
	}
}

// TestConformanceChainsLongerThanGitFollows writes through a chain of symbolic branches: up to what
// git follows the write reaches the end of the chain; past it, it is refused, and no branch of the
// chain stops being symbolic.
func TestConformanceChainsLongerThanGitFollows(t *testing.T) {
	t.Parallel()

	backend := newGitaiBackend(t, gitaiMode{name: "off-standalone", standalone: true}, agentGraft(everyOperationRPC...))
	client := gitalypb.NewOperationServiceClient(agentConnection(t, backend))

	// a5 is 1 hop from real, a0 is 6.
	links := []string{"a5", "a4", "a3", "a2", "a1", "a0"}
	for hops, top := range links {
		hops++
		t.Run(top, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t, backend.newRemote(t))
			f.session.local(f.path, "update-ref", "refs/heads/real", f.main)
			previous := "refs/heads/real"
			for _, link := range links {
				f.session.local(f.path, "symbolic-ref", "refs/heads/"+link, previous)
				previous = "refs/heads/" + link
			}

			err := operations{client, f}.updateBranch(backend.ctx, top, f.main)
			if hops <= 4 {
				require.NoError(t, err)
				require.Equal(t, f.source, f.session.local(f.path, "rev-parse", "refs/heads/real"))
			} else {
				require.Equal(t, "FailedPrecondition", outcome(err))
				require.Equal(t, f.main, f.session.local(f.path, "rev-parse", "refs/heads/real"))
			}
			for i, link := range links {
				want := "refs/heads/real"
				if i > 0 {
					want = "refs/heads/" + links[i-1]
				}
				require.Equal(t, want, f.session.local(f.path, "symbolic-ref", "--no-recurse", "refs/heads/"+link), link)
			}
		})
	}
}

// TestConformanceSymbolicLinkBranch writes through a branch that is a symbolic link, which git reads as
// a symbolic reference: the target moves and the link stays.
func TestConformanceSymbolicLinkBranch(t *testing.T) {
	t.Parallel()
	testhelper.SkipWithReftable(t, "the references are not files")

	backend := newGitaiBackend(t, gitaiMode{name: "off-standalone", standalone: true}, agentGraft(everyOperationRPC...))
	client := gitalypb.NewOperationServiceClient(agentConnection(t, backend))

	f := newFixture(t, backend.newRemote(t))
	f.session.local(f.path, "update-ref", "refs/heads/real", f.main)
	link := filepath.Join(f.path, "refs/heads/link")
	require.NoError(t, os.Symlink("refs/heads/real", link))

	require.NoError(t, operations{client, f}.updateBranch(backend.ctx, "link", f.main))

	require.Equal(t, f.source, f.session.local(f.path, "rev-parse", "refs/heads/real"))
	info, err := os.Lstat(link)
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&os.ModeSymlink, "the link stays a link")
}

// TestConformanceToRefRefusesSymbolicTargets writes the merge and the rebase of the *ToRef RPCs to a
// target_ref that is symbolic: its target is outside of the namespace the RPCs validate. Whether the
// target exists, and whether the request expects an old value, changes nothing.
func TestConformanceToRefRefusesSymbolicTargets(t *testing.T) {
	t.Parallel()

	backend := newGitaiBackend(t, gitaiMode{name: "off-standalone", standalone: true}, agentGraft(everyOperationRPC...))
	client := gitalypb.NewOperationServiceClient(agentConnection(t, backend))
	const targetRef = "refs/merge-requests/1/merge"

	for _, kind := range []string{"UserMergeToRef", "UserRebaseToRef"} {
		for _, target := range []string{"refs/graft/symbolic", "refs/heads/allowed", "refs/graft/absent", "refs/heads/absent"} {
			for _, expectOld := range []bool{false, true} {
				if expectOld && strings.HasSuffix(target, "absent") {
					continue
				}
				t.Run(fmt.Sprintf("%s/%s/expectOld=%v", kind, target, expectOld), func(t *testing.T) {
					t.Parallel()

					f := newFixture(t, backend.newRemote(t))
					old := ""
					if !strings.HasSuffix(target, "absent") {
						f.session.local(f.path, "update-ref", target, f.main)
					}
					if expectOld {
						old = f.main
					}
					f.session.local(f.path, "symbolic-ref", targetRef, target)
					before := f.refs()

					var err error
					if kind == "UserMergeToRef" {
						_, err = client.UserMergeToRef(backend.ctx, &gitalypb.UserMergeToRefRequest{
							Repository: f.repo, User: gittest.TestUser, SourceSha: f.source, TargetRef: []byte(targetRef),
							FirstParentRef: []byte("refs/heads/main"), Message: []byte("merge"), ExpectedOldOid: old,
						})
					} else {
						_, err = client.UserRebaseToRef(backend.ctx, &gitalypb.UserRebaseToRefRequest{
							Repository: f.repo, User: gittest.TestUser, SourceSha: f.source, TargetRef: []byte(targetRef),
							FirstParentRef: []byte("refs/heads/main"), ExpectedOldOid: old,
						})
					}
					require.Equal(t, "FailedPrecondition", outcome(err), "%v", err)
					require.ErrorContains(t, err, "target reference is symbolic")
					require.Empty(t, changedRefs(before, f.refs()))
				})
			}
		}
	}
}

// decoyNamespaces are the namespaces whose references git's DWIM rules take for a full name.
var decoyNamespaces = []string{"refs/refs/", "refs/tags/refs/", "refs/heads/refs/", "refs/remotes/refs/"}

// TestConformanceAgentCannotPlantDecoys pushes into each decoy namespace as an agent, in every mode: it
// may not create or update a reference there, it may delete an old one, and the shared token may
// still create one. UserCreateBranch with a full name is the RPC's way into refs/heads/refs/.
func TestConformanceAgentCannotPlantDecoys(t *testing.T) {
	t.Parallel()

	for _, mode := range matrixModes() {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()

			backend := newGitaiBackend(t, mode, agentGraft(everyOperationRPC...))
			client := gitalypb.NewOperationServiceClient(agentConnection(t, backend))

			for _, namespace := range decoyNamespaces {
				t.Run(namespace, func(t *testing.T) {
					t.Parallel()

					f := newFixture(t, backend.newRemote(t))
					name := namespace + "graft/green/main"

					before := f.refs()
					require.NotZero(t, f.session.push(f.client, "source:"+name), "creating %s", name)
					require.Empty(t, changedRefs(before, f.refs()))

					// The shared token: a feature of the server or an operator. The storage core has to
					// see it, so it does not go on the disk behind its back.
					stream, err := gitalypb.NewRefServiceClient(backend.operatorConnection(t)).UpdateReferences(backend.ctx)
					require.NoError(t, err)
					require.NoError(t, stream.Send(&gitalypb.UpdateReferencesRequest{Repository: f.repo, Updates: []*gitalypb.UpdateReferencesRequest_Update{
						{Reference: []byte(name), NewObjectId: []byte(f.main)},
					}}))
					_, err = stream.CloseAndRecv()
					require.NoError(t, err)

					before = f.refs()
					require.Contains(t, before, name)
					require.NotZero(t, f.session.push(f.client, "--force", "source:"+name), "updating %s", name)
					require.Empty(t, changedRefs(before, f.refs()))

					// refs/remotes/ is hidden on a push whoever pushes, so that a delete is refused
					// before the reservation is asked (git's receive.hideRefs).
					if namespace == "refs/remotes/refs/" {
						return
					}
					require.Zero(t, f.session.push(f.client, ":"+name), "deleting %s\n%s", name, f.session.log.String())
					require.Equal(t, []string{name}, changedRefs(before, f.refs()))
				})
			}

			t.Run("UserCreateBranch", func(t *testing.T) {
				t.Parallel()

				f := newFixture(t, backend.newRemote(t))
				before := f.refs()
				err := operations{client, f}.createBranch(backend.ctx, "refs/graft/green/main", "main")
				require.Equal(t, "PermissionDenied", outcome(err), "%v", err)
				require.Empty(t, changedRefs(before, f.refs()))
			})
		})
	}
}

// TestConformanceSymbolicBranchInTheStorageCore writes through a symbolic branch that the storage core
// knows of, in the modes with a core: the database keeps the branch symbolic, moves its target as the
// disk does, and agrees with the disk.
func TestConformanceSymbolicBranchInTheStorageCore(t *testing.T) {
	t.Parallel()

	for _, mode := range gitaiModes() {
		if mode.coreMode == "" {
			continue
		}
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()

			backend := newGitaiBackend(t, mode, agentGraft(everyOperationRPC...))
			client := gitalypb.NewOperationServiceClient(agentConnection(t, backend))

			for _, rpc := range aliasWrites {
				t.Run(rpc.name, func(t *testing.T) {
					t.Parallel()

					ctx := context.Background()
					f := newFixture(t, backend.newRemote(t))
					repository, err := backend.core.Meta.GetRepository(ctx, corestore.RepoKey{
						Storage: f.repo.GetStorageName(), RelativePath: f.repo.GetRelativePath(),
					})
					require.NoError(t, err)
					_, err = backend.core.Meta.CommitChangeset(ctx, corestore.ChangesetInput{
						RepoID: repository.ID, Operation: "conformance", Actor: "conformance", NodeID: backend.core.NodeID,
						Updates: []corestore.RefUpdate{{Name: "refs/heads/alias", Expect: corestore.ExpectValue, New: corestore.SymbolicTarget("refs/heads/main")}},
					})
					require.NoError(t, err)
					// The local copy holds it too, as after a sync.
					f.session.local(f.path, "symbolic-ref", "refs/heads/alias", "refs/heads/main")

					callErr := rpc.call(backend.ctx, operations{client, f}, "alias")

					database, _, err := backend.core.Meta.Refs(ctx, repository.ID)
					require.NoError(t, err)
					require.Equal(t, "ref:refs/heads/main", string(database["refs/heads/alias"]), "%s: %v", rpc.name, callErr)
					require.Equal(t, "refs/heads/main", f.session.local(f.path, "symbolic-ref", "--quiet", "refs/heads/alias"))
					require.Equal(t, f.refs()["refs/heads/main"], string(database["refs/heads/main"]), "%s: %v", rpc.name, callErr)
					if callErr == nil {
						require.NotEqual(t, f.main, string(database["refs/heads/main"]), "%s moved the target", rpc.name)
					}
				})
			}
		})
	}
}
