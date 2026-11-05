package conformance

import (
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	gitalyauth "github.com/feder-cr/git-ai/auth"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// A fixture is a repository for the RPC tables, seeded by a stock client so that it works in every
// mode: main with a submodule at sub, source, one commit ahead of main with a file of its own, which
// is what the RPCs land, merge or pick; and the patch of that commit.
type fixture struct {
	session *session
	repo    *gitalypb.Repository
	path    string
	main    string
	source  string
	subNext string
	patch   []byte
	client  string
}

func newFixture(t *testing.T, r remote) *fixture {
	t.Helper()

	s := &session{
		t: t, ctx: testhelper.Context(t), remote: r, protocol: 0,
		root: testhelper.TempDir(t), observed: observations{},
	}
	dir := s.seeded()
	subNext := s.local(dir, "rev-parse", "main~1")
	sub := s.local(dir, "rev-parse", "main~2")
	s.local(dir, "update-index", "--add", "--cacheinfo", "160000,"+sub+",sub")
	s.tick++
	s.local(dir, "commit", "--quiet", "--message", "the submodule")
	main := s.local(dir, "rev-parse", "HEAD")
	s.local(dir, "branch", "source")
	s.local(dir, "switch", "--quiet", "source")
	source := s.commit(dir, "source.txt", "source\n")
	patch := s.local(dir, "format-patch", "--stdout", "-1", "source")
	s.push(dir, "--force", "main", "source")
	require.Equal(t, "0", s.observed[keyOfLastStep(s)], s.log.String())

	return &fixture{
		session: s, repo: r.repo, path: r.path, main: main, source: source, subNext: subNext,
		patch: []byte(patch + "\n"), client: dir,
	}
}

// keyOfLastStep returns the key of the exit code of the session's last step.
func keyOfLastStep(s *session) string {
	last := ""
	for key := range s.observed {
		if strings.HasSuffix(key, ": exit") && strings.HasPrefix(key, "step ") && key > last {
			last = key
		}
	}
	return last
}

// refs returns the references of the fixture's repository, name to value, with the target of a
// symbolic one.
func (f *fixture) refs() map[string]string {
	f.session.t.Helper()

	refs := map[string]string{}
	output := f.session.local(f.path, "for-each-ref", "--format=%(refname) %(objectname)")
	for _, line := range strings.Split(output, "\n") {
		if name, value, ok := strings.Cut(line, " "); ok {
			refs[name] = value
		}
	}
	return refs
}

// changedRefs returns the names of the references that differ between two states, sorted.
func changedRefs(before, after map[string]string) []string {
	changed := []string{}
	for name, value := range after {
		if before[name] != value {
			changed = append(changed, name)
		}
	}
	for name := range before {
		if _, ok := after[name]; !ok {
			changed = append(changed, name)
		}
	}
	sort.Strings(changed)
	return changed
}

// agentConnection connects to the backend with the agent's token.
func agentConnection(t *testing.T, b *gitaiBackend) *grpc.ClientConn {
	t.Helper()

	conn, err := grpc.NewClient(b.cfg.SocketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(gitalyauth.RPCCredentialsV2(agentToken)),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// The calls of the RPCs of the tables. Each returns the error of the RPC, or the refusal that its
// response carries as an error, because some RPCs report a hook's refusal in a response field.

type operations struct {
	client gitalypb.OperationServiceClient
	f      *fixture
}

var errRefused = errors.New("refused in the response")

func refusedIf(refusal string) error {
	if refusal != "" {
		return errRefused
	}
	return nil
}

func (o operations) commitFiles(ctx context.Context, branch, startBranch string) error {
	stream, err := o.client.UserCommitFiles(ctx)
	if err != nil {
		return err
	}
	for _, request := range []*gitalypb.UserCommitFilesRequest{
		{UserCommitFilesRequestPayload: &gitalypb.UserCommitFilesRequest_Header{Header: &gitalypb.UserCommitFilesRequestHeader{
			Repository: o.f.repo, User: gittest.TestUser, BranchName: []byte(branch), CommitMessage: []byte("files"),
			StartBranchName: []byte(startBranch),
		}}},
		{UserCommitFilesRequestPayload: &gitalypb.UserCommitFilesRequest_Action{Action: &gitalypb.UserCommitFilesAction{
			UserCommitFilesActionPayload: &gitalypb.UserCommitFilesAction_Header{Header: &gitalypb.UserCommitFilesActionHeader{
				Action: gitalypb.UserCommitFilesActionHeader_CREATE, FilePath: []byte("written.txt"),
			}},
		}}},
	} {
		if err := stream.Send(request); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
	}
	response, err := stream.CloseAndRecv()
	if err != nil {
		return err
	}
	return refusedIf(response.GetPreReceiveError())
}

func (o operations) createBranch(ctx context.Context, branch, startPoint string) error {
	_, err := o.client.UserCreateBranch(ctx, &gitalypb.UserCreateBranchRequest{
		Repository: o.f.repo, User: gittest.TestUser, BranchName: []byte(branch), StartPoint: []byte(startPoint),
	})
	return err
}

func (o operations) updateBranch(ctx context.Context, branch, oldRevision string) error {
	response, err := o.client.UserUpdateBranch(ctx, &gitalypb.UserUpdateBranchRequest{
		Repository: o.f.repo, User: gittest.TestUser, BranchName: []byte(branch),
		Newrev: []byte(o.f.source), Oldrev: []byte(oldRevision),
	})
	if err != nil {
		return err
	}
	return refusedIf(response.GetPreReceiveError())
}

func (o operations) deleteBranch(ctx context.Context, branch string) error {
	_, err := o.client.UserDeleteBranch(ctx, &gitalypb.UserDeleteBranchRequest{
		Repository: o.f.repo, User: gittest.TestUser, BranchName: []byte(branch),
	})
	return err
}

func (o operations) land(ctx context.Context, branch string) error {
	_, err := o.client.UserLand(ctx, &gitalypb.UserLandRequest{
		Repository: o.f.repo, User: gittest.TestUser, Branch: []byte(branch), CommitId: o.f.source, CreateBranch: true,
	})
	return err
}

func (o operations) ffBranch(ctx context.Context, branch string) error {
	response, err := o.client.UserFFBranch(ctx, &gitalypb.UserFFBranchRequest{
		Repository: o.f.repo, User: gittest.TestUser, CommitId: o.f.source, Branch: []byte(branch),
	})
	if err != nil {
		return err
	}
	return refusedIf(response.GetPreReceiveError())
}

func (o operations) mergeBranch(ctx context.Context, branch string) error {
	stream, err := o.client.UserMergeBranch(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&gitalypb.UserMergeBranchRequest{
		Repository: o.f.repo, User: gittest.TestUser, CommitId: o.f.source, Branch: []byte(branch), Message: []byte("merge"),
	}); err != nil {
		return err
	}
	if _, err := stream.Recv(); err != nil {
		return err
	}
	if err := stream.Send(&gitalypb.UserMergeBranchRequest{Apply: true}); err != nil {
		return err
	}
	if _, err := stream.Recv(); err != nil {
		return err
	}
	return stream.CloseSend()
}

func (o operations) cherryPick(ctx context.Context, branch string) error {
	_, err := o.client.UserCherryPick(ctx, &gitalypb.UserCherryPickRequest{
		Repository: o.f.repo, User: gittest.TestUser, Commit: &gitalypb.GitCommit{Id: o.f.source},
		BranchName: []byte(branch), Message: []byte("pick"), StartBranchName: []byte("main"),
	})
	return err
}

func (o operations) revert(ctx context.Context, branch string) error {
	_, err := o.client.UserRevert(ctx, &gitalypb.UserRevertRequest{
		Repository: o.f.repo, User: gittest.TestUser, Commit: &gitalypb.GitCommit{Id: o.f.source},
		BranchName: []byte(branch), Message: []byte("revert"), StartBranchName: []byte("source"),
	})
	return err
}

func (o operations) applyPatch(ctx context.Context, branch string) error {
	stream, err := o.client.UserApplyPatch(ctx)
	if err != nil {
		return err
	}
	for _, request := range []*gitalypb.UserApplyPatchRequest{
		{UserApplyPatchRequestPayload: &gitalypb.UserApplyPatchRequest_Header_{Header: &gitalypb.UserApplyPatchRequest_Header{
			Repository: o.f.repo, User: gittest.TestUser, TargetBranch: []byte(branch),
		}}},
		{UserApplyPatchRequestPayload: &gitalypb.UserApplyPatchRequest_Patches{Patches: o.f.patch}},
	} {
		if err := stream.Send(request); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
	}
	_, err = stream.CloseAndRecv()
	return err
}

func (o operations) updateSubmodule(ctx context.Context, branch string) error {
	response, err := o.client.UserUpdateSubmodule(ctx, &gitalypb.UserUpdateSubmoduleRequest{
		Repository: o.f.repo, User: gittest.TestUser, CommitSha: o.f.subNext, Branch: []byte(branch),
		Submodule: []byte("sub"), CommitMessage: []byte("submodule"),
	})
	if err != nil {
		return err
	}
	return refusedIf(response.GetPreReceiveError())
}

func (o operations) mergeToRef(ctx context.Context, targetRef, firstParentRef string) error {
	_, err := o.client.UserMergeToRef(ctx, &gitalypb.UserMergeToRefRequest{
		Repository: o.f.repo, User: gittest.TestUser, SourceSha: o.f.source, TargetRef: []byte(targetRef),
		FirstParentRef: []byte(firstParentRef), Message: []byte("merge"),
	})
	return err
}

func (o operations) rebaseToRef(ctx context.Context, targetRef, firstParentRef string) error {
	_, err := o.client.UserRebaseToRef(ctx, &gitalypb.UserRebaseToRefRequest{
		Repository: o.f.repo, User: gittest.TestUser, SourceSha: o.f.source, TargetRef: []byte(targetRef),
		FirstParentRef: []byte(firstParentRef),
	})
	return err
}

func (o operations) rebaseConfirmable(ctx context.Context, branch, branchSHA, remoteBranch string) error {
	stream, err := o.client.UserRebaseConfirmable(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&gitalypb.UserRebaseConfirmableRequest{
		UserRebaseConfirmableRequestPayload: &gitalypb.UserRebaseConfirmableRequest_Header_{Header: &gitalypb.UserRebaseConfirmableRequest_Header{
			Repository: o.f.repo, User: gittest.TestUser, RebaseId: "1", Branch: []byte(branch), BranchSha: branchSHA,
			RemoteRepository: o.f.repo, RemoteBranch: []byte(remoteBranch),
		}},
	}); err != nil {
		return err
	}
	if _, err := stream.Recv(); err != nil {
		return err
	}
	if err := stream.Send(&gitalypb.UserRebaseConfirmableRequest{
		UserRebaseConfirmableRequestPayload: &gitalypb.UserRebaseConfirmableRequest_Apply{Apply: true},
	}); err != nil {
		return err
	}
	if _, err := stream.Recv(); err != nil {
		return err
	}
	return stream.CloseSend()
}

func (o operations) createTag(ctx context.Context, tag string) error {
	_, err := o.client.UserCreateTag(ctx, &gitalypb.UserCreateTagRequest{
		Repository: o.f.repo, User: gittest.TestUser, TagName: []byte(tag), TargetRevision: []byte("main"),
	})
	return err
}

func (o operations) deleteTag(ctx context.Context, tag string) error {
	_, err := o.client.UserDeleteTag(ctx, &gitalypb.UserDeleteTagRequest{
		Repository: o.f.repo, User: gittest.TestUser, TagName: []byte(tag),
	})
	return err
}

// outcome is an error in the form the tables compare and print.
func outcome(err error) string {
	if err == nil {
		return "ok"
	}
	if errors.Is(err, errRefused) {
		return errRefused.Error()
	}
	return status.Code(err).String()
}
