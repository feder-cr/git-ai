package refnamelimit

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

const zeroOID = "0000000000000000000000000000000000000000"

// name returns a name that makes a reference of exactly length bytes with the given prefix.
func name(prefix string, length int) []byte {
	return []byte(strings.Repeat("x", length-len(prefix)))
}

// fullName returns a full reference name of exactly length bytes.
func fullName(length int) []byte {
	return []byte("refs/" + strings.Repeat("x", length-len("refs/")))
}

// writeRequests returns one request per field that names a written reference, which names a reference
// of length bytes.
func writeRequests(length int) map[string]proto.Message {
	branch := name(branchPrefix, length)
	full := fullName(length)

	return map[string]proto.Message{
		"UserCreateBranch":      &gitalypb.UserCreateBranchRequest{BranchName: branch},
		"UserUpdateBranch":      &gitalypb.UserUpdateBranchRequest{BranchName: branch},
		"UserCreateTag":         &gitalypb.UserCreateTagRequest{TagName: name(tagPrefix, length)},
		"UserMergeToRef":        &gitalypb.UserMergeToRefRequest{TargetRef: full},
		"UserRebaseToRef":       &gitalypb.UserRebaseToRefRequest{TargetRef: full},
		"UserMergeBranch":       &gitalypb.UserMergeBranchRequest{Branch: branch},
		"UserFFBranch":          &gitalypb.UserFFBranchRequest{Branch: branch},
		"UserCherryPick":        &gitalypb.UserCherryPickRequest{BranchName: branch},
		"UserRevert":            &gitalypb.UserRevertRequest{BranchName: branch},
		"UserUpdateSubmodule":   &gitalypb.UserUpdateSubmoduleRequest{Branch: branch},
		"UserLand":              &gitalypb.UserLandRequest{Branch: branch},
		"CreateRepository":      &gitalypb.CreateRepositoryRequest{DefaultBranch: branch},
		"FetchSourceBranch":     &gitalypb.FetchSourceBranchRequest{TargetRef: full},
		"WriteRef":              &gitalypb.WriteRefRequest{Ref: full, Revision: []byte("main")},
		"WriteRef of HEAD":      &gitalypb.WriteRefRequest{Ref: []byte("HEAD"), Revision: full},
		"UpdateReferences":      &gitalypb.UpdateReferencesRequest{Updates: []*gitalypb.UpdateReferencesRequest_Update{{Reference: full, NewObjectId: []byte("1")}}},
		"UserCommitFiles":       &gitalypb.UserCommitFilesRequest{UserCommitFilesRequestPayload: &gitalypb.UserCommitFilesRequest_Header{Header: &gitalypb.UserCommitFilesRequestHeader{BranchName: branch}}},
		"UserRebaseConfirmable": &gitalypb.UserRebaseConfirmableRequest{UserRebaseConfirmableRequestPayload: &gitalypb.UserRebaseConfirmableRequest_Header_{Header: &gitalypb.UserRebaseConfirmableRequest_Header{Branch: branch}}},
		"UserApplyPatch":        &gitalypb.UserApplyPatchRequest{UserApplyPatchRequestPayload: &gitalypb.UserApplyPatchRequest_Header_{Header: &gitalypb.UserApplyPatchRequest_Header{TargetBranch: branch}}},
		"ResolveConflicts":      &gitalypb.ResolveConflictsRequest{ResolveConflictsRequestPayload: &gitalypb.ResolveConflictsRequest_Header{Header: &gitalypb.ResolveConflictsRequestHeader{SourceBranch: branch}}},
	}
}

func TestUnaryInterceptor(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	call := func(req proto.Message) (bool, error) {
		called := false
		_, err := UnaryInterceptor(ctx, req, &grpc.UnaryServerInfo{}, func(context.Context, any) (any, error) {
			called = true
			return nil, nil
		})
		return called, err
	}

	for desc, req := range writeRequests(1025) {
		t.Run(desc+" over the limit", func(t *testing.T) {
			t.Parallel()

			called, err := call(req)
			require.Equal(t, codes.InvalidArgument, status.Code(err))
			require.ErrorContains(t, err, "over the limit of 1024 bytes")
			require.False(t, called, "the handler must not run")
		})
	}

	for desc, req := range writeRequests(1024) {
		t.Run(desc+" at the limit", func(t *testing.T) {
			t.Parallel()

			called, err := call(req)
			require.NoError(t, err)
			require.True(t, called)
		})
	}

	long := fullName(1025)
	for desc, req := range map[string]proto.Message{
		"UserDeleteBranch":          &gitalypb.UserDeleteBranchRequest{BranchName: long},
		"UserDeleteTag":             &gitalypb.UserDeleteTagRequest{TagName: long},
		"DeleteRefs":                &gitalypb.DeleteRefsRequest{Refs: [][]byte{long}},
		"WriteRef deletion":         &gitalypb.WriteRefRequest{Ref: long, Revision: []byte(zeroOID)},
		"UpdateReferences deletion": &gitalypb.UpdateReferencesRequest{Updates: []*gitalypb.UpdateReferencesRequest_Update{{Reference: long, NewObjectId: []byte(zeroOID)}}},
		"FindBranch":                &gitalypb.FindBranchRequest{Name: long},
		"RefExists":                 &gitalypb.RefExistsRequest{Ref: long},
		"ListRefs":                  &gitalypb.ListRefsRequest{Patterns: [][]byte{long}},
	} {
		t.Run(desc+" over the limit is not a write", func(t *testing.T) {
			t.Parallel()

			called, err := call(req)
			require.NoError(t, err)
			require.True(t, called)
		})
	}
}

// fakeStream hands the handler the messages of a stream.
type fakeStream struct {
	grpc.ServerStream
	messages []*gitalypb.UpdateReferencesRequest
}

func (s *fakeStream) RecvMsg(m any) error {
	proto.Merge(m.(proto.Message), s.messages[0])
	s.messages = s.messages[1:]
	return nil
}

func TestStreamInterceptor_checksEveryMessage(t *testing.T) {
	t.Parallel()

	stream := &fakeStream{messages: []*gitalypb.UpdateReferencesRequest{
		{Updates: []*gitalypb.UpdateReferencesRequest_Update{{Reference: fullName(1024), NewObjectId: []byte("1")}}},
		{Updates: []*gitalypb.UpdateReferencesRequest_Update{{Reference: fullName(1025), NewObjectId: []byte("1")}}},
	}}

	var received int
	err := StreamInterceptor(nil, stream, &grpc.StreamServerInfo{}, func(_ any, ss grpc.ServerStream) error {
		for {
			if err := ss.RecvMsg(&gitalypb.UpdateReferencesRequest{}); err != nil {
				return err
			}
			received++
		}
	})

	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "over the limit of 1024 bytes")
	require.Equal(t, 1, received)
}

// fieldMayNameReference matches the fields that could name a reference.
var fieldMayNameReference = regexp.MustCompile(`branch|ref|tag|target|revision|head`)

// writtenFields are the request fields of the mutating RPCs that name a reference the RPC writes, which
// writtenReferences checks. An exact list, so that a new field has to be judged.
var writtenFields = map[protoreflect.FullName]struct{}{
	"gitaly.CreateRepositoryRequest.default_branch":      {},
	"gitaly.FetchSourceBranchRequest.target_ref":         {},
	"gitaly.ResolveConflictsRequestHeader.source_branch": {},
	"gitaly.UpdateReferencesRequest.Update.reference":    {},
	"gitaly.UserApplyPatchRequest.Header.target_branch":  {},
	"gitaly.UserCherryPickRequest.branch_name":           {},
	"gitaly.UserCommitFilesRequestHeader.branch_name":    {},
	"gitaly.UserCreateBranchRequest.branch_name":         {},
	"gitaly.UserCreateTagRequest.tag_name":               {},
	"gitaly.UserFFBranchRequest.branch":                  {},
	"gitaly.UserLandRequest.branch":                      {},
	"gitaly.UserMergeBranchRequest.branch":               {},
	"gitaly.UserMergeToRefRequest.target_ref":            {},
	"gitaly.UserRebaseConfirmableRequest.Header.branch":  {},
	"gitaly.UserRebaseToRefRequest.target_ref":           {},
	"gitaly.UserRevertRequest.branch_name":               {},
	"gitaly.UserUpdateBranchRequest.branch_name":         {},
	"gitaly.UserUpdateSubmoduleRequest.branch":           {},
	"gitaly.WriteRefRequest.ref":                         {},
	"gitaly.WriteRefRequest.revision":                    {},
}

// otherFields are the fields of the mutating RPCs whose names match fieldMayNameReference but that do not
// name a reference the RPC writes.
var otherFields = map[protoreflect.FullName]string{
	"gitaly.CommitLanguagesRequest.revision":                          "read",
	"gitaly.CreateForkRequest.revision":                               "selects an existing reference of the source; repoutil.Create checks what the new repository holds",
	"gitaly.CreateRepositoryFromURLRequest.http_authorization_header": "not a reference",
	"gitaly.CreateReviewRequest.head_commit_id":                       "a commit",
	"gitaly.CreateReviewRequest.target_branch":                        "stored in the review, no reference is written under it",
	"gitaly.DeleteConflictingReferenceRequest.expected_target":        "compared with the database value; nothing is written under it",
	"gitaly.DeleteConflictingReferenceRequest.reference":              "deletion",
	"gitaly.DeleteRefsRequest.except_with_prefix":                     "deletion",
	"gitaly.DeleteRefsRequest.refs":                                   "deletion",
	"gitaly.FetchSourceBranchRequest.expected_target_old_oid":         "an object ID",
	"gitaly.FetchSourceBranchRequest.source_branch":                   "read",
	"gitaly.GitCommit.referenced_by":                                  "read",
	"gitaly.Remote.http_authorization_header":                         "not a reference",
	"gitaly.Remote.mirror_refmaps":                                    "refspecs: the names they produce reach the updater",
	"gitaly.ResolveConflictsRequestHeader.target_branch":              "read",
	"gitaly.RestoreRepositoryRequest.path_prefix":                     "not a reference",
	"gitaly.SubmitReviewVerdictRequest.head_commit_id":                "a commit",
	"gitaly.UpdateReviewRequest.expected_head_commit_id":              "a commit",
	"gitaly.UpdateReviewRequest.head_commit_id":                       "a commit",
	"gitaly.UserCherryPickRequest.start_branch_name":                  "read",
	"gitaly.UserCommitFilesRequestHeader.start_branch_name":           "read",
	"gitaly.UserCreateTagRequest.target_revision":                     "read",
	"gitaly.UserDeleteBranchRequest.branch_name":                      "deletion",
	"gitaly.UserDeleteTagRequest.tag_name":                            "deletion",
	"gitaly.UserMergeToRefRequest.branch":                             "read",
	"gitaly.UserMergeToRefRequest.first_parent_ref":                   "read",
	"gitaly.UserRebaseConfirmableRequest.Header.branch_sha":           "an object ID",
	"gitaly.UserRebaseConfirmableRequest.Header.remote_branch":        "read",
	"gitaly.UserRebaseToRefRequest.first_parent_ref":                  "read",
	"gitaly.UserRevertRequest.start_branch_name":                      "read",
	"gitaly.WriteRefRequest.old_revision":                             "read",
}

// TestMutatingRPCFieldsAreClassified walks the requests of every mutating RPC, so that a field that may
// name a reference to write cannot be added without deciding whether writtenReferences has to check it.
func TestMutatingRPCFieldsAreClassified(t *testing.T) {
	t.Parallel()

	var unclassified []string
	visited := map[protoreflect.FullName]bool{}

	var walk func(protoreflect.MessageDescriptor)
	walk = func(m protoreflect.MessageDescriptor) {
		if visited[m.FullName()] {
			return
		}
		visited[m.FullName()] = true

		fields := m.Fields()
		for i := 0; i < fields.Len(); i++ {
			field := fields.Get(i)
			switch field.Kind() {
			case protoreflect.MessageKind, protoreflect.GroupKind:
				walk(field.Message())
			case protoreflect.StringKind, protoreflect.BytesKind:
				if !fieldMayNameReference.MatchString(string(field.Name())) {
					continue
				}
				_, written := writtenFields[field.FullName()]
				_, other := otherFields[field.FullName()]
				if !written && !other {
					unclassified = append(unclassified, string(field.FullName()))
				}
			}
		}
	}

	protoregistry.GlobalFiles.RangeFiles(func(file protoreflect.FileDescriptor) bool {
		if file.Package() != "gitaly" {
			return true
		}
		for i := 0; i < file.Services().Len(); i++ {
			methods := file.Services().Get(i).Methods()
			for j := 0; j < methods.Len(); j++ {
				method := methods.Get(j)
				op, ok := proto.GetExtension(method.Options(), gitalypb.E_OpType).(*gitalypb.OperationMsg)
				if ok && op.GetOp() == gitalypb.OperationMsg_MUTATOR {
					walk(method.Input())
				}
			}
		}
		return true
	})

	sort.Strings(unclassified)
	require.Empty(t, unclassified, "decide for each field whether writtenReferences checks it")
}
