package gitcmd

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/mvcc"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/internal/graft/policystore"
	"github.com/feder-cr/git-ai/internal/grpc/metadata"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/transaction/txinfo"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"google.golang.org/protobuf/encoding/protojson"
)

const (
	// EnvHooksPayload is the name of the environment variable used
	// to hold the hooks payload.
	EnvHooksPayload = "GITALY_HOOKS_PAYLOAD"
)

// ErrPayloadNotFound is returned by HooksPayloadFromEnv if the given
// environment variables don't have a hooks payload.
var ErrPayloadNotFound = errors.New("no hooks payload found in environment")

// Hook represents a git hook. See githooks(5) for more information about
// existing hooks.
type Hook uint

const (
	// ReferenceTransactionHook represents the reference-transaction git hook.
	ReferenceTransactionHook = Hook(1 << iota)
	// UpdateHook represents the update git hook.
	UpdateHook
	// PreReceiveHook represents the pre-receive git hook.
	PreReceiveHook
	// PostReceiveHook represents the post-receive git hook.
	PostReceiveHook
	// PackObjectsHook represents the pack-objects git hook.
	PackObjectsHook

	// AllHooks is the bitwise set of all hooks supported by Gitaly.
	AllHooks = ReferenceTransactionHook | UpdateHook | PreReceiveHook | PostReceiveHook | PackObjectsHook
	// ReceivePackHooks includes the set of hooks which shall be executed in
	// a typical "push" or an emulation thereof (e.g. `updateReferenceWithHooks()`).
	ReceivePackHooks = ReferenceTransactionHook | UpdateHook | PreReceiveHook | PostReceiveHook
)

// FeatureFlagWithValue is used as part of the HooksPayload to pass on feature flags with their
// values to gitaly-hooks.
type FeatureFlagWithValue struct {
	// Flag is the feature flag.
	Flag featureflag.FeatureFlag `json:"flag"`
	// Enabled indicates whether the flag is enabled or not.
	Enabled bool `json:"enabled"`
}

// HooksPayload holds parameters required for all hooks.
type HooksPayload struct {
	// RequestedHooks is a bitfield of requested Hooks. Hooks which
	// were not requested will not get executed.
	RequestedHooks Hook `json:"requested_hooks"`
	// FeatureFlagsWithValue contains feature flags with their values. They are set into the
	// outgoing context when calling HookService.
	FeatureFlagsWithValue []FeatureFlagWithValue `json:"feature_flags_with_value,omitempty"`

	// Repo is the repository in which the hook is running.
	Repo *gitalypb.Repository `json:"-"`
	// ObjectFormat is the object format used by the repository. Some hooks use it in order to
	// verify object IDs part of their input.
	ObjectFormat string `json:"object_format"`

	// RuntimeDir is the path to Gitaly's runtime directory.
	RuntimeDir string `json:"runtime_dir"`
	// InternalSocket is the path to Gitaly's internal socket.
	InternalSocket string `json:"internal_socket"`
	// InternalSocketToken is the token required to authenticate with
	// Gitaly's internal socket.
	InternalSocketToken string `json:"internal_socket_token"`

	// Transaction is used to identify a reference transaction. This is an optional field -- if
	// it's not set, no transactional voting will happen.
	Transaction *txinfo.Transaction `json:"transaction"`

	// UserDetails contains information required when executing
	// git-receive-pack or git-upload-pack
	UserDetails *UserDetails `json:"user_details"`

	// GitalyClientContext contains the context passed through the RPCs.
	GitalyClientContext []byte `json:"gitaly_client_context,omitempty"`

	// StoragePreparing asks gitaly-hooks to forward the "preparing" state of
	// the reference-transaction hook to the server. It is set when the Graft
	// storage core is authoritative, whose write path builds and uploads the
	// pack of a transaction before git takes the reference locks
	// (doc/graft/PLAN6.md, LOCK AND HISTORY RULE). Otherwise the state is not
	// forwarded, as upstream: Praefect votes would change, and authoritative
	// mode is not supported behind Praefect.
	StoragePreparing bool `json:"storage_preparing,omitempty"`

	// LogFields contains the log fields in the current context. They are passed
	// to the HookService so the calls can be correlated with the main RPC call.
	LogFields log.Fields

	// Changelog is the changeset log metadata of the reference updates, taken
	// from the context that built the payload. Env writes it into the environment
	// and ChangelogMetaFromEnv reads it back. HooksPayloadFromEnv deliberately
	// does not restore it, so a decoded payload only holds the upstream fields
	// and still compares equal to them.
	Changelog ChangelogMeta `json:"-"`
}

// ChangelogMeta describes the changeset log entries of the reference updates
// performed with a hooks payload. It is taken from the context of the request
// that builds the payload.
type ChangelogMeta struct {
	// Operation is the operation that performs the updates, empty when it cannot
	// be derived from the context.
	Operation string `json:"operation,omitempty"`
	// Actor is the authenticated identity that performs the updates. Empty when
	// there is none, in which case the recording hook falls back to the user ID.
	Actor string `json:"actor,omitempty"`
	// Agent is true when Actor was authenticated with a per-agent token. The
	// standalone access policy then decides on Actor instead of the
	// client-supplied user ID, so an agent cannot act as another one.
	Agent bool `json:"agent,omitempty"`
	// Rewritten is true when the updates rewrite history.
	Rewritten bool `json:"rewritten,omitempty"`
	// Invocation identifies the git process that runs the reference-transaction
	// hook with this payload, or the call of updateref.UpdaterWithHooks that drives
	// the hook in process. It is a random value set for each git command that git
	// runs the hook for and for each such call, so the recording hook can tell the
	// transactions of one process apart from those of concurrent processes, also
	// when they have the same updates. Git runs two transactions to delete a
	// packed reference, and the recording hook pairs them by this value. The owner
	// of the process declares its end with BeginInvocation, which is how the storage
	// core learns that git will report nothing more for it. It is empty when neither
	// the changeset log nor the authoritative storage core needs it, see
	// InvocationRequired.
	Invocation string `json:"invocation,omitempty"`
	// MVCCCommit is the commit handle (see mvcc.CommitHandle) of the MVCC cache of
	// the request that builds the payload. On a repository that uses the MVCC
	// reference backend, the updates only become visible once that request commits
	// its cache, and they are thrown away when the commit fails. The recording hook
	// therefore records them only once the cache has committed. It is empty when
	// the request has no MVCC cache that it commits, or when the changeset log is
	// disabled.
	MVCCCommit string `json:"mvcc_commit,omitempty"`
	// PolicyGeneration is the generation of the standalone access policy under which a land
	// batch admitted its members (policystore.WithAdmissionGeneration). The
	// reference-transaction hook refuses the batch's update when the policy in force is another
	// one (doc/graft/PLAN12.md, section 5). Only the server builds it from the context, like
	// Members.
	PolicyGeneration uint64 `json:"policy_generation,omitempty"`
	// Members lists the changes that a batched land folded into the updates, in
	// the order they were landed (see changelog.WithBatch). The list is capped at
	// maxChangelogMembersSize. Only the server builds it from the context, so a
	// client cannot set it through the environment of a git command.
	Members []changelog.Member `json:"members,omitempty"`
	// MembersTruncated is true when Members lacks information: the resolved paths
	// were dropped or the list was cut to fit maxChangelogMembersSize.
	MembersTruncated bool `json:"members_truncated,omitempty"`
	// LandReference and Lands are the identities of the changes that the updates land on
	// that branch (see changelog.WithLands and doc/graft/PLAN11.md). They are never cut:
	// WithLands bounds them by changelog.MaxLandsSize and refuses a land whose identities
	// do not fit. Only the server builds them from the context, like Members.
	// LandReference holds the branch name as bytes, which JSON encodes in base64:
	// a branch name may hold bytes that are not valid UTF-8, which a JSON string
	// would replace.
	LandReference []byte           `json:"land_reference,omitempty"`
	Lands         []changelog.Land `json:"lands,omitempty"`
}

// IsZero reports whether m holds no metadata. ChangelogMeta holds slices and
// is therefore not comparable with ==.
func (m ChangelogMeta) IsZero() bool {
	return m.Operation == "" &&
		m.Actor == "" &&
		!m.Agent &&
		!m.Rewritten &&
		m.Invocation == "" &&
		m.MVCCCommit == "" &&
		len(m.Members) == 0 &&
		!m.MembersTruncated &&
		len(m.LandReference) == 0 &&
		len(m.Lands) == 0
}

// NewChangelogInvocation returns a new random value for ChangelogMeta.Invocation.
func NewChangelogInvocation() (string, error) {
	var invocation [16]byte
	if _, err := rand.Read(invocation[:]); err != nil {
		return "", fmt.Errorf("generating changelog invocation: %w", err)
	}

	return hex.EncodeToString(invocation[:]), nil
}

// UserDetails contains all information which is required for hooks
// executed by git-receive-pack, namely the pre-receive, update or post-receive
// hook.
type UserDetails struct {
	// Username contains the name of the user who has caused the hook to be executed.
	Username string `json:"username"`
	// UserID contains the ID of the user who has caused the hook to be executed.
	UserID string `json:"userid"`
	// Protocol contains the protocol via which the hook was executed. This
	// can be one of "web", "ssh" or "smarthttp".
	Protocol string `json:"protocol"`
	// RemoteIP contains the original IP of the client who initiated the flow leading to this
	// target hook.
	RemoteIP string `json:"remote_ip"`
}

// jsonHooksPayload wraps the HooksPayload such that we can manually encode the
// repository protobuf message.
type jsonHooksPayload struct {
	HooksPayload
	Repo string `json:"repository"`
	// Changelog carries ChangelogMeta next to the upstream fields. It is not
	// restored into HooksPayload on decode, see HooksPayload.Changelog.
	Changelog *ChangelogMeta `json:"graft_changelog,omitempty"`
}

// NewHooksPayload creates a new hooks payload which can then be encoded and
// passed to Git hooks.
func NewHooksPayload(
	ctx context.Context,
	cfg config.Cfg,
	repo storage.Repository,
	objectHash git.ObjectHash,
	tx *txinfo.Transaction,
	userDetails *UserDetails,
	requestedHooks Hook,
	featureFlagsWithValue map[featureflag.FeatureFlag]bool,
) HooksPayload {
	flags := make([]FeatureFlagWithValue, 0, len(featureFlagsWithValue))
	for flag, enabled := range featureFlagsWithValue {
		flags = append(flags, FeatureFlagWithValue{
			Flag:    flag,
			Enabled: enabled,
		})
	}

	// Extract context blob from gRPC metadata
	var clientContext []byte
	if ctxValue := metadata.GetValue(ctx, metadata.ClientContextMetadataKey); ctxValue != "" {
		clientContext = []byte(ctxValue)
	}

	// The changeset log attributes the update to an operation and an actor.
	// Only explicit values are carried here: an operation that cannot be
	// derived from the context stays empty, and so does the actor without an
	// authenticated identity. The hook that records the update then falls
	// back to the request's user ID and to "unknown" (see recordChangesets).
	operation := changelog.OperationFromContext(ctx)
	if operation == changelog.UnknownOperation {
		operation = ""
	}

	id, hasIdentity := identity.FromContext(ctx)

	members, membersTruncated := capChangelogMembers(changelog.BatchFromContext(ctx))
	// The payload holds no branch for an update that lands nothing: an empty slice would not
	// survive the encoding (omitempty), and the decoded payload would differ.
	var landReference []byte
	reference, lands := changelog.LandsFromContext(ctx)
	if reference != "" {
		landReference = []byte(reference)
	}

	return HooksPayload{
		Repo: &gitalypb.Repository{
			StorageName:                   repo.GetStorageName(),
			RelativePath:                  repo.GetRelativePath(),
			GitObjectDirectory:            repo.GetGitObjectDirectory(),
			GitAlternateObjectDirectories: repo.GetGitAlternateObjectDirectories(),
			GlRepository:                  repo.GetGlRepository(),
			GlProjectPath:                 repo.GetGlProjectPath(),
		},
		ObjectFormat:          objectHash.Format,
		RuntimeDir:            cfg.RuntimeDir,
		InternalSocket:        cfg.InternalSocketPath(),
		InternalSocketToken:   cfg.Auth.GetToken(),
		Transaction:           tx,
		UserDetails:           userDetails,
		RequestedHooks:        requestedHooks,
		FeatureFlagsWithValue: flags,
		GitalyClientContext:   clientContext,
		StoragePreparing:      cfg.Graft.Storage.Mode == "authoritative",
		LogFields:             log.FromContext(ctx).Data(),
		Changelog: ChangelogMeta{
			Operation:        operation,
			Actor:            identity.Actor(ctx, ""),
			Agent:            hasIdentity && id.Source == identity.SourceAgent,
			Rewritten:        changelog.IsRewritten(ctx),
			Members:          members,
			MembersTruncated: membersTruncated,
			LandReference:    landReference,
			Lands:            lands,
			MVCCCommit:       mvccCommitHandle(ctx, cfg),
			PolicyGeneration: policystore.AdmissionGeneration(ctx),
		},
	}
}

// maxChangelogMembersSize bounds the encoded size of ChangelogMeta.Members. The
// members travel in the environment of every git process of the update, and
// the environment has a limited size.
const maxChangelogMembersSize = 32 << 10

// capChangelogMembers returns the members as they fit into the payload. It drops
// the resolved paths first, and then cuts the list to the longest prefix that
// fits. The truncated result is true when information was dropped. The input is
// not modified.
func capChangelogMembers(members []changelog.Member) ([]changelog.Member, bool) {
	if len(members) == 0 {
		return nil, false
	}

	if encodedMembersSize(members) <= maxChangelogMembersSize {
		return members, false
	}

	stripped := make([]changelog.Member, len(members))
	for i, m := range members {
		m.ResolvedPaths = nil
		stripped[i] = m
	}

	if encodedMembersSize(stripped) <= maxChangelogMembersSize {
		return stripped, true
	}

	// The encoded size grows with every member, so the first prefix that does not
	// fit marks where the list is cut.
	kept := sort.Search(len(stripped), func(i int) bool {
		return encodedMembersSize(stripped[:i+1]) > maxChangelogMembersSize
	})

	// When even the first member does not fit, which needs an unrealistically large
	// actor or object ID, no member is kept. The update then carries no members, so
	// the reference-transaction hook does not skip the policy check for it and the
	// batch falls back to checking the combined update, which can only deny it.
	if kept == 0 {
		return nil, true
	}

	// A truncated list is still safe for the policy skip: every member was checked
	// in process for its own actor, and all members of a batch update the same
	// branch, so the members that were cut from the payload were checked as well.
	return stripped[:kept], true
}

// encodedMembersSize returns the size of the JSON encoding of members. An
// encoding failure counts as too large, so that the members are dropped.
func encodedMembersSize(members []changelog.Member) int {
	encoded, err := json.Marshal(members)
	if err != nil {
		return maxChangelogMembersSize + 1
	}

	return len(encoded)
}

// mvccCommitHandle returns the handle of the MVCC cache that commits the reference updates
// performed with a hooks payload built from ctx, see ChangelogMeta.MVCCCommit.
func mvccCommitHandle(ctx context.Context, cfg config.Cfg) string {
	if cfg.Graft.Changelog.Disabled {
		return ""
	}

	cache := mvcc.CacheFromContext(ctx)
	if cache == nil {
		return ""
	}

	return mvcc.CommitHandle(cache)
}

// ChangelogMetaFromEnv returns the changeset log metadata that Env encoded into
// the environment. Without it, the zero value is returned, and the recording
// hook falls back to the request's user ID and to "unknown".
func ChangelogMetaFromEnv(envs []string) (ChangelogMeta, error) {
	encoded, ok := lookupEnv(envs, EnvHooksPayload)
	if !ok {
		return ChangelogMeta{}, ErrPayloadNotFound
	}

	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return ChangelogMeta{}, err
	}

	var jsonPayload jsonHooksPayload
	if err := json.Unmarshal(decoded, &jsonPayload); err != nil {
		return ChangelogMeta{}, err
	}

	if jsonPayload.Changelog == nil {
		return ChangelogMeta{}, nil
	}

	return *jsonPayload.Changelog, nil
}

func lookupEnv(envs []string, key string) (string, bool) {
	for _, env := range envs {
		kv := strings.SplitN(env, "=", 2)
		if len(kv) != 2 {
			continue
		}

		if kv[0] == key {
			return kv[1], true
		}
	}

	return "", false
}

// HooksPayloadFromEnv extracts the HooksPayload from the given environment
// variables. If no HooksPayload exists, it returns a ErrPayloadNotFound
// error.
func HooksPayloadFromEnv(envs []string) (HooksPayload, error) {
	encoded, ok := lookupEnv(envs, EnvHooksPayload)
	if !ok {
		return HooksPayload{}, ErrPayloadNotFound
	}

	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return HooksPayload{}, err
	}

	var jsonPayload jsonHooksPayload
	if err := json.Unmarshal(decoded, &jsonPayload); err != nil {
		return HooksPayload{}, err
	}

	var repo gitalypb.Repository
	err = protojson.Unmarshal([]byte(jsonPayload.Repo), &repo)
	if err != nil {
		return HooksPayload{}, err
	}

	payload := jsonPayload.HooksPayload
	payload.Repo = &repo

	// If no RequestedHooks are passed down to us, then we need to assume
	// that the caller of this hook isn't aware of this field and thus just
	// pretend that he wants to execute all hooks.
	if payload.RequestedHooks == 0 {
		payload.RequestedHooks = AllHooks
	}

	return payload, nil
}

// Env encodes the given HooksPayload into an environment variable.
func (p HooksPayload) Env() (string, error) {
	repo, err := protojson.Marshal(p.Repo)
	if err != nil {
		return "", err
	}

	jsonPayload := jsonHooksPayload{HooksPayload: p, Repo: string(repo)}
	if !p.Changelog.IsZero() {
		meta := p.Changelog
		jsonPayload.Changelog = &meta
	}
	marshalled, err := json.Marshal(jsonPayload)
	if err != nil {
		return "", err
	}

	encoded := base64.StdEncoding.EncodeToString(marshalled)

	return fmt.Sprintf("%s=%s", EnvHooksPayload, encoded), nil
}

// IsHookRequested returns whether the HooksPayload is requesting execution of
// the given git hook.
func (p HooksPayload) IsHookRequested(hook Hook) bool {
	return p.RequestedHooks&hook != 0
}
