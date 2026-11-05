// Package refnamelimit refuses, with InvalidArgument, the requests of every caller that name a
// reference to write over git.MaxReferenceNameLength, before any handler, hook or git process runs.
//
// It holds the list of the request fields that name a reference the RPC writes. The layers below it
// hold the same limit for what no request names: the update hook per reference of a push, the updater
// and the reference-transaction hook for names that come from elsewhere, such as a remote. A request
// that deletes a reference is not refused, so that an existing long reference can be cleaned up.
package refnamelimit

import (
	"context"
	"strings"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"google.golang.org/grpc"
)

const (
	branchPrefix = "refs/heads/"
	tagPrefix    = "refs/tags/"
)

// UnaryInterceptor refuses a unary request that names a reference to write over the limit.
func UnaryInterceptor(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if err := validate(req); err != nil {
		return nil, err
	}

	return handler(ctx, req)
}

// StreamInterceptor refuses every message of a stream that names a reference to write over the limit.
func StreamInterceptor(srv any, stream grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	return handler(srv, &guardedStream{ServerStream: stream})
}

// guardedStream checks the messages that the handler receives.
type guardedStream struct {
	grpc.ServerStream
}

// RecvMsg receives the next message and refuses it when it names a reference over the limit.
func (s *guardedStream) RecvMsg(m any) error {
	if err := s.ServerStream.RecvMsg(m); err != nil {
		return err
	}

	return validate(m)
}

// validate refuses a request that names a reference to write over the limit.
func validate(req any) error {
	for _, name := range writtenReferences(req) {
		if err := git.ValidateReferenceNameLength(name); err != nil {
			return structerr.NewInvalidArgument("%w", err)
		}
	}

	return nil
}

// writtenReferences returns the full names of the references that a request writes, and the targets of
// the symbolic references that it writes. TestMutatingRPCFieldsAreClassified keeps the list honest.
func writtenReferences(req any) []string {
	switch r := req.(type) {
	case *gitalypb.UserCreateBranchRequest:
		return []string{branchPrefix + string(r.GetBranchName())}
	case *gitalypb.UserUpdateBranchRequest:
		return []string{branchPrefix + string(r.GetBranchName())}
	case *gitalypb.UserCreateTagRequest:
		return []string{tagPrefix + string(r.GetTagName())}
	case *gitalypb.UserMergeToRefRequest:
		return []string{string(r.GetTargetRef())}
	case *gitalypb.UserRebaseToRefRequest:
		return []string{string(r.GetTargetRef())}
	case *gitalypb.UserMergeBranchRequest:
		return []string{branchPrefix + string(r.GetBranch())}
	case *gitalypb.UserFFBranchRequest:
		return []string{branchPrefix + string(r.GetBranch())}
	case *gitalypb.UserCherryPickRequest:
		return []string{branchPrefix + string(r.GetBranchName())}
	case *gitalypb.UserRevertRequest:
		return []string{branchPrefix + string(r.GetBranchName())}
	case *gitalypb.UserCommitFilesRequest:
		return []string{branchPrefix + string(r.GetHeader().GetBranchName())}
	case *gitalypb.UserRebaseConfirmableRequest:
		return []string{branchPrefix + string(r.GetHeader().GetBranch())}
	case *gitalypb.UserApplyPatchRequest:
		return []string{branchPrefix + string(r.GetHeader().GetTargetBranch())}
	case *gitalypb.UserUpdateSubmoduleRequest:
		return []string{branchPrefix + string(r.GetBranch())}
	case *gitalypb.UserLandRequest:
		return []string{branchPrefix + string(r.GetBranch())}
	case *gitalypb.ResolveConflictsRequest:
		return []string{branchPrefix + string(r.GetHeader().GetSourceBranch())}
	case *gitalypb.CreateRepositoryRequest:
		return []string{branchPrefix + string(r.GetDefaultBranch())}
	case *gitalypb.FetchSourceBranchRequest:
		return []string{string(r.GetTargetRef())}
	case *gitalypb.WriteRefRequest:
		return writeRefReferences(r)
	case *gitalypb.UpdateReferencesRequest:
		var names []string
		for _, update := range r.GetUpdates() {
			if !isZeroOID(string(update.GetNewObjectId())) {
				names = append(names, string(update.GetReference()))
			}
		}
		return names
	default:
		return nil
	}
}

// writeRefReferences returns what a WriteRef writes: the target of HEAD, or a reference unless the
// request deletes it.
func writeRefReferences(r *gitalypb.WriteRefRequest) []string {
	if string(r.GetRef()) == "HEAD" {
		return []string{string(r.GetRevision())}
	}
	if isZeroOID(string(r.GetRevision())) {
		return nil
	}

	return []string{string(r.GetRef())}
}

// isZeroOID reports whether oid is an all-zeroes object ID, of whichever hash the repository uses: a
// request is judged before its repository is looked at.
func isZeroOID(oid string) bool {
	return oid != "" && strings.Trim(oid, "0") == ""
}
