# Agent security model

Agents are automated writers. They are useful only if they can land changes
without a human, so Graft gives them the least authority that still lets them
work. This document lists what an agent can and cannot do, how it is
identified, and which settings control it.

## Identities

- Each agent has its own token, configured as a `[[graft.agent]]` entry with a
  `token_file`. The token is compared in constant time. The v2 HMAC token
  format, computed with the agent token as the secret, is accepted too.
- A request with a valid agent token runs as that agent
  (`identity.SourceAgent`). The agent name is the actor recorded in the
  changelog, in policy decisions and in trailers.
- The shared token (`auth.token`) is an operator credential. It is not an
  agent. Setup calls of tools and the verifier use it.
- Health checks need no token.
- With no agents configured, the server behaves as upstream Gitaly.

Agent identity is taken from the token, never from the request. The request
defaults interceptor overwrites `gl_id` and `gl_username` fields with the
agent's name for agent callers. An agent cannot impersonate another actor by
putting a different name in the request.

## What an agent may call

Agents may call only the RPCs on the built-in allowlist, plus any RPCs listed
in `graft.agent_rpcs`. The allowlist is what the SDK, the `graft` CLI and the
cache pass-through use for agent work:

- object and tree reads (`ObjectService`);
- changesets (`ChangesetService`, read only);
- derived data (`DerivedDataService`);
- code search (`SearchService`, read only, used by `graft search`);
- reviews (`ReviewService`, used by `graft review`). Its mutators write the
  review store of the repository and the keep-around references of the
  review's head and base commits (`refs/keep-around/review/<commit>`, which
  keep those commits from garbage collection while the review is open). The
  reference name is derived from a commit that must already be in the
  repository, so a review cannot write a branch, a tag or any other name, nor
  add an object. They record the token's identity as the actor, so an agent
  cannot approve as another agent;
- landing and commits: `UserLand`, `UserCommitFiles`, `UserCreateBranch`,
  `UserDeleteBranch` (through policy);
- read accessors of the commit, ref, blob, tree and diff services
  (`Find*`, `List*`, `Get*`), plus `CommitService.TreeEntry` and
  `DiffService.DiffStats` (used by the SDK and `graft show`);
- `ServerService.ServerInfo`, `RepositoryService.RepositoryExists` (which
  `graft-cache` uses to authorize a caller for a repository) and the gRPC
  health service.

Explicitly not allowed for agents, even though upstream Gitaly offers them.
These can never be enabled, not even through `graft.agent_rpcs`:

- `RepositoryService` mutators: `CreateRepository`, `CreateFork`,
  `CreateRepositoryFrom*`, `BackupRepository`, `RestoreRepository`,
  `RemoveRepository`, `ReplicateRepository`, `WriteRef`, `FetchRemote`,
  `FetchSourceBranch`, `FetchBundle`, `SetCustomHooks`, `RestoreCustomHooks`,
  `OptimizeRepository`, `PruneUnreachableObjects`.
- `RefService` `UpdateReferences` and `DeleteRefs`.
- `RemoteService` `UpdateRemoteMirror`, `FindRemoteRepository` and
  `FindRemoteRootRef`, which push to or query a URL the caller chooses.
- `SSHService` `SSHUploadArchive`, which reads any revision named in its own
  input, `refs/graft/` included (see below).
- `CleanupService`, `ObjectPoolService`, `HookService`, `InternalGitaly`,
  `PartitionService`, and `StorageCoreService` (imports, checkpoints and
  garbage collection, which deletes packs of the bucket).

An agent does not see `refs/graft/` at all (`internal/graft/agentrefs`): the
agent guard refuses a request whose revision, reference or pattern names a
reference under it (including the short forms `graft/proposals/...` and
`graft/green/...`, which git completes to the server's namespaces; other
`graft/...` short names, such as the workspace's temporary branches, are
branches), and removes such references from every response; git upload-pack and
receive-pack hide the prefix from agents. Git's pseudo references reach every
reference without naming one, so every git command of an agent that walks them
(`rev-list`, `log`, `show`, `rev-parse`, `bundle`, `fast-export`,
`format-patch`) gets `--exclude-hidden=uploadpack` before each `--all` and
`--glob`, in its arguments and on its stdin, with `uploadpack.hideRefs` set to
`refs/graft/`: `--all` and `--glob=refs/*` in `ListCommits` or `ListBlobs`,
`FindCommits` with `all`, or any other RPC or field that asks for such a walk,
leave the namespace out (`internal/git/gitcmd/agent_walk.go`). `--alternate-refs`
walks the references of the object pool, which mirrors the member's references,
`refs/graft/` included: for an agent it walks only the pool's branches and tags
(`core.alternateRefsPrefixes`). A test classifies,
by message and field, every string, bytes, bool and enum field of every RPC
agents may call by default, so a new field cannot escape the filter; RPCs added
with `graft.agent_rpcs` are checked by field name. Other callers still see
graft-verify's green references, which are documented for consumers.

Revisions reach git one per line on its standard input, so the line is what git
reads, not what the request said. Four layers keep that from being a way past
the hiding:

1. **Input validation (every caller).** A revision with an ASCII control
   character (below space, or DEL) is refused with `InvalidArgument`, the
   pseudo-revisions `--glob=`, `--branches=` and `--tags=` and their patterns
   included, and so is a line feed or carriage return in the path of a
   path-scoped revision (`rev:path`). Before this, those patterns took any bytes,
   and `--glob=x` followed by a line feed and `refs/graft/green/main` was a
   second revision, naming a hidden reference that the request check had read as
   part of the pattern. Compatibility: this applies to other callers too. A
   control character in a revision never named a reference (`git
   check-ref-format` forbids them), and a line break in the path of
   `ListBlobs` or `ListLFSPointers` could not be read by `git rev-list --stdin`
   as a path anyway. The three RPCs whose revisions git reads NUL-delimited
   (`git cat-file -Z`: `CheckObjectsExist`, `DiffBlobs`, `CheckBlobsGenerated`) keep
   accepting a line break in a path (`AllowLineBreaksInPath`), as their tests
   require. A path with a tab, a space or other special characters is allowed
   everywhere.
2. **The sinks.** Every writer of revisions into a newline-delimited git stdin
   (`git rev-list --stdin`) refuses a revision with a line feed, a carriage
   return or a NUL, whatever its caller validated, through
   `git.ValidateStdinRevision`. Today there are three: `gitpipe.Revlist` (ListCommits,
   ListBlobs, ListLFSPointers, GetTagSignatures and the other revision walks),
   `ObjectsSize`, and `CreateBundleFromRefList`, whose patterns go to
   `git bundle create --stdin`. A test (`internal/git/stdin_writers_test.go`)
   parses the code and lists every file that gives a git command a flag that
   makes it read its stdin (`--stdin`, `--revs`, `--batch` and the flags that
   begin with them) by how it is safe: a request's revisions through the helper
   (the test finds the call in the syntax tree, not a word in a comment);
   NUL-delimited (`-z`, `-Z`); or input the server builds from object IDs (a
   request's revisions are resolved to object IDs first), packs, content or
   paths of its own. It fails on a writer it does not know, so a new one is
   classified when it is added.
3. **The filter reads lines as git does.** The `--exclude-hidden` flag is put
   before a line that git reads as `--all` or `--glob=...`: without its line
   feed and the carriage return before it, and cut at the first NUL.
4. **The request check splits at the same separators.** `RevisionHidden` also
   splits at line feed, carriage return, tab and NUL.

What the hiding covers, and what it does not: it covers the reference **names**
under `refs/graft/` and the reachability that goes through them. An agent can
neither address those references nor walk to their objects through them, so it
can neither depend on a proposal reference nor learn of one from a listing. It
does **not** make the objects secret. A proposal's commits and blobs are
ordinary objects of the repository: `ListAllBlobs` and `ListAllLFSPointers`
enumerate every blob, `GetObjects` reads any object by its ID, and an agent with
access to the repository can read their content, a proposal's message included. The goal is
that agents can neither address nor depend on `refs/graft/`, not object
confidentiality; anything that must stay secret from an agent must not be
written into a repository the agent can read.

Whatever the RPC and whatever the policy says, an agent never creates,
updates or deletes a reference under `refs/graft/`. The namespace is the
server's: the proposals of a land (`refs/graft/proposals/`, hidden from
advertisement and push, written by the server) and graft-verify's green
references (written with the shared token). The reservation is not a rule of
the policy: it holds in every mode, standalone or not, for every agent. The
layers that enforce it, in the order a request meets them:

1. The agent allowlist. `UpdateReferences`, `DeleteRefs` and `WriteRef` are never
   open to agents, and neither is `SSHUploadArchive`: git-upload-archive takes
   the revision to archive in its own input, where the request filter cannot see
   it, and ignores `uploadpack.hideRefs`.
2. The request filter of the agent interceptor (`checkAgentRefs`): a request
   whose revision, reference or pattern fields name `refs/graft/` is refused
   before the RPC runs.
3. On a push (when an operator opens `SSHReceivePack` or the HTTP push to
   agents), `git-receive-pack` runs with `receive.hideRefs=refs/graft/` for
   agents, so git refuses the update ("deny updating a hidden ref") before any
   hook. The RPCs that update references without the pre-receive hook
   (`UserMergeToRef`, `UserRebaseToRef` when opened with `agent_rpcs`) check the
   reservation before they write (`CheckAgentPolicy`) and answer
   `PermissionDenied`. The other agent RPCs write branches, the review roots
   (`refs/keep-around/review/`) and, when opened with `agent_rpcs`, tags.
4. The reference-transaction hook refuses every agent update under
   `refs/graft/` while the references are locked, whatever reached it. It is
   told the references that are written, so an update through a symbolic
   reference whose target is under `refs/graft/` is refused too. On a push git
   tells it. The RPCs that run the hooks themselves (`UpdaterWithHooks`) read
   the target first, tell every hook that name, and write exactly that reference
   without following a symbolic reference again, so a reference retargeted
   meanwhile does not change what was judged.

`TestPolicyHoldsOnAgentPushes` in `internal/graft/e2e` pins layer 3 on a real
push, `TestPolicyHookDecidesForAgentUpdates` layer 4 alone (the real hook, run
by `git update-ref` with the payload of an agent's write, a symbolic reference
included), both in standalone mode and outside it, and
`TestConformanceAgentWritesThroughSymbolicBranches` every way of writing a
branch through a symbolic one. The pre-receive hook does not check the
reservation: it judges the policy in standalone mode (GitLab's `/allowed`
outside it). On a push it sees the names that were pushed, not what git will
write: a push to `refs/heads/alias`, a symbolic reference to `refs/graft/x`,
reaches it as `refs/heads/alias` and would write `refs/graft/x`; the
reference-transaction hook, which sees `refs/graft/x`, refuses it. The combined
update of a land batch, which runs without an agent identity, is held to the
reservation by the hook as well.

A denial of the reservation does not name the reference: an agent that wrote
through a symbolic branch named something else, and the target may be hidden from
it. The server's log has the name.

Git's DWIM rules take `refs/tags/refs/heads/x` for `refs/heads/x` while that does
not exist, so such a reference can answer for another one. Agents neither create
nor update a reference under `refs/refs/`, `refs/tags/refs/`, `refs/heads/refs/` or
`refs/remotes/refs/`, in every mode, by the same two last layers. They may delete
one, so that an old one can be cleaned up. The shared token is not restricted.

Every other RPC that is not on the allowlist (for example the other
`OperationService` RPCs such as `UserMergeBranch`, or the other `SSHService`
RPCs) is denied by default and can be enabled with `graft.agent_rpcs`, except
the RPCs that can never be enabled (above), `SSHUploadArchive` among them.

A call outside the allowlist fails with `PermissionDenied`. The error does
not reveal more than that the method is not allowed for agents.

### Extending the allowlist (`agent_rpcs`)

```toml
[graft]
agent_rpcs = ["/gitaly.RepositoryService/RepositorySize"]
```

Each entry is a full gRPC method name. The server checks the list at startup
and refuses to start when an entry is not a registered method or is one of the
methods above that agents may never call. The list applies to every agent;
there are no per-agent allowlists.

`SSHService` is not on the built-in allowlist, so agents cannot fetch or push
over the Git protocol through Gitaly or through the `graft-cache`
pass-through. To allow it, list
`/gitaly.SSHService/SSHUploadPackWithSidechannel` (fetch) and
`/gitaly.SSHService/SSHReceivePack` (push). Pushes are still checked by the
branch policy, but they bypass the `UserLand` path, its retries and its
conflict resolution.

Listing the fetch RPC does not make `gitaly-ssh` work for an agent. That tool
is the server's own transport between Gitaly nodes: it adds
`transfer.hideRefs=!refs` to every fetch so as to see every reference, and an
agent's request with `git_config_options` is refused (`PermissionDenied`, see
below). Pushes through `gitaly-ssh` work. The front end of an agent's fetch has
to send `SSHUploadPackWithSidechannel` itself, without `git_config_options`.

The test `TestAgentCannotCreateRepositoryFromSnapshot` and the phase-2
allowlist test check this from the outside. An exhaustive table test checks
that every registered RPC is either allowlisted or denied for agents, so a new
RPC is denied until someone decides otherwise.

## Requests that are always refused for agents

- `gitaly.HookService` calls.
- Administrative RPCs (see `internal/gitaly/server/auth/agent_methods.go`).
- Any request that carries `git_config_options`. Such options could change how
  Git runs on the server.
- Any request that carries `gitaly-servers` metadata. It names the address
  and token of other Gitaly servers, which RPCs such as `UserCommitFiles` with
  a `start_repository` on another storage dial and fetch from. An agent could
  otherwise make the server connect to any address.

## Branch policy

Policy rules (`[[graft.policy]]`) decide which actors may update which
references. Each rule has an actor glob and reference globs. The first rule
whose actor matches and whose refs match every changed reference decides.
Changes that touch references governed by different rules are denied unless
all are allowed. `**` as a suffix matches any depth.

Policy covers the `HEAD` symbolic ref as well, so an agent cannot repoint
`HEAD` to bypass a protected branch.

A denial by policy is a `PermissionDenied` error. Standalone denial messages
do not start with `GitLab:`, so they are not mistaken for a GitLab hook
rejection.

A branch that is a symbolic reference is judged by the branch it points to,
which is what an update writes, however the update comes. A rule that allows
`refs/heads/alias` does not allow writing `refs/heads/main` through it. On a push
the pre-receive hook also judges the name that was pushed. Creating a symbolic
branch needs the rights of an operator. `UserLand` takes a symbolic branch for
the branch it points to, in all it records, and refuses one that points to
anything else. A cycle of symbolic references, or a chain longer than
`git symbolic-ref` follows (more than four hops), is refused with
`FailedPrecondition`, as it cannot be judged. `UserMergeToRef` and
`UserRebaseToRef` refuse a symbolic `target_ref`.

One known limit, accepted: a land with `resolve_conflicts` is checked against
the standalone policy before the resolver is called, so an actor the policy
denies gets nothing resolved. A shared-token caller whose GitLab user the GitLab
pre-receive check would deny is not stopped there: that check runs when the
references are updated, so the resolver is called, and the land is then refused.

## Resolution and verification are opt-in

**A model's resolution can land, and the verification is what lets it.** With a
server AI and a `verify.command`, a resolution that the model made lands if the
command passes on the merged tree (see [RESOLVE](RESOLVE.md)). That makes the
command a code execution path: it runs code a model wrote, derived from
repository content that agents control. A hostile agent can plant text for the
model to read (a prompt injection), and the model's answer is then executed by
your tests, as the gitaly user on the server. The tree the command sees is the
whole tree (no `export-ignore` hides anything from it), so a planted file is
there for it as well. Run the command inside an isolation boundary, always when
the AI lands: a container with `--network=none`, the checkout read-only, no
credentials, resource limits (the example in RESOLVE.md is the default to
start from), and a separate unprivileged user. The post-merge test command of
`graft-verify` ([VERIFY](VERIFY.md)) runs the same kind of code on the tip and
has the same advice. Without a `verify.command` nothing of a model's lands.

Three things that the container does not close:

- `--network=none` closes the network, not the output. Up to 4 KiB of what the command prints goes back
  to the agent in the reason of the conflict package, so a command that prints a secret hands it
  out. Print nothing that is not for the agent, and keep secrets out of the checkout's environment
  and file system.
- Symbolic links of the tree are written as links and can point anywhere on the host. Inside a
  container with the checkout as the only mount they point at nothing; outside one, a command that
  follows them reads what the gitaly user can read. The checkout never writes through a link.
- The tree may hold files that other tools read: `graft-resolve`'s own check commands (`[[check]]`)
  also run on a model's output, with the same advice. The checkout itself refuses a `.git`
  anywhere in the tree, and a tree over `verify.max_tree_entries` or `verify.max_tree_bytes`.

What counts as a model's answer is fail-safe on both sides. The server treats any
file that is not marked `origin: "rule"` as a model's. `graft-resolve` marks as a
rule only the strategies that say so (trivial, union, rerere, and a `command`
that is not configured with `origin = "model"`); every other strategy, including
one registered from another package, is a model. A third-party resolver, or a
strategy that claims to be a rule, is the operator's trust: a `command` that
calls an AI and is left as a rule lands without the verification. The rerere
store of v0.1.0 and v0.2.0 may hold model answers; its entries without an origin
are never replayed as rules (see [RESOLVE](RESOLVE.md)).

- Automatic conflict resolution is off by default and can be limited to
  specific actors and paths (see [RESOLVE](RESOLVE.md)).
- The verifier writes the green ref and reverts with the operator credential,
  not an agent credential (see [VERIFY](VERIFY.md)).
- No Graft server component calls a language model. AI resolvers and
  summarizers run in `tools/graft-agents` behind a vendor-neutral interface.

## Operator checklist

1. Give every agent its own token file, readable only by the agent's account.
2. Keep `auth.token` for operators and tooling, and do not give it to agents.
3. Protect default and release branches with policy rules.
4. Enable resolution only for actors and paths you trust.
5. Keep `graft-resolve` and the verifier on a separate account when possible.
6. Review changelog entries with `graft log`, which shows the actor of every
   change.
