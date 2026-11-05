# Graft limitations and roadmap

This page lists what Graft does not do, what it does not try to do, and where
it is going. It is written for people deciding whether to run Graft, so the
limitations come first. The end-to-end test in `internal/graft/e2e` covers the
behaviour described in the sections that are not marked as limits, on one
machine.

## What Graft is not

- **Not a hosting product.** Graft has no web UI, no issues, no merge request
  review workflow and no CI. It is a Git-compatible server with extra RPCs and
  a command-line client for agent workspaces.
- **Not a replacement for GitLab.** Standalone mode removes the dependency on
  GitLab for access checks and hooks. It does not provide GitLab's features.
- **Not a Scion implementation.** Graft adopts ideas from Anthropic's article
  (see [ARCHITECTURE.md](ARCHITECTURE.md)). Internal details of Scion are not
  public and are not reproduced.
- **Not a security boundary by itself.** The access policy is local
  configuration. Whoever can edit the server configuration can change who may
  write where. Protect the configuration and the token files accordingly.

## Reference name length

Reference names, and symbolic reference targets, over 1024 bytes are refused.
An existing reference over the limit can still be read and deleted. An object
pool's names under `refs/remotes/origin/` may be 15 bytes longer, which is what
the pool's refspec adds to a name of its source.

A repository that holds such a reference fails, with a permanent error, whatever
copies its references until the reference is deleted: forking it, creating or
replicating a replica, creating or fetching into an object pool, restoring a
backup, `FetchBundle`, `CreateRepositoryFromBundle`, `RewriteHistory` and
importing it into the storage core. One such reference on a remote fails the
whole of a mirror's `FetchRemote`.

To find such references (the second command is for an unborn `HEAD`, which the
first does not list), and delete one:

    git -C <repository> for-each-ref --format='%(refname) %(symref)' | LC_ALL=C awk 'length($1) > 1024 || length($2) > 1024'
    git -C <repository> symbolic-ref -q HEAD | LC_ALL=C awk 'length($0) > 1024'
    git -C <repository> update-ref --no-deref -d <reference>

`--no-deref` matters: without it, deleting a symbolic reference deletes its target.
In an object pool the first command also lists the allowed names of 1025 to 1039
bytes under `refs/remotes/origin/`. The `DeleteRefs` RPC and a push that deletes
the reference work too, where the repository's writes are allowed.

An incomplete registration left by an earlier failed import is refused the same
way. In authoritative mode the server may refuse writes to such a repository, so
run the commands above on the node, with writers stopped, and import again: the
import resumes.

The allowance of object pools goes by the path of the repository, so a repository
at a pool-shaped path has it too, up to 1039 bytes under `refs/remotes/origin/`.
A symbolic reference that points at nothing, other than `HEAD`, is not checked
when a repository is created from a copy.

## Security limitations

### Who the policy decides on

For a request authenticated with an agent token, the standalone access policy
decides on the agent name of that token. The acting user ID that the client
sends (`gitaly.User.gl_id`) does not change the decision: in standalone mode the
request defaults replace it with the agent name before the request is handled.
The user's name and email, which become the committer of new commits, are kept
as sent. The `builder` token with `User.ID` set to `planner` is denied on
`main`. `TestPolicyUsesTokenIdentity` in `internal/graft/e2e` pins this.

For a request authenticated with the shared `auth.token`, or with no
authentication, the policy decides on the acting user ID that the request
carries. The request defaults fill it with `shared` (or `anonymous`) only when
the client left it empty; the `shared` identity exists only when agents are
configured, so without agents a caller with the shared token is `anonymous`.
The shared token is an administrative credential: a holder of it can act as any
user, as with upstream Gitaly and GitLab.

The changeset log records the identity of the token as the actor: the agent
name, or `shared` for the shared token when agents are configured. Without
agents, it falls back to the client's user ID.
`TestChangesetActorUsesTokenIdentity` in `internal/graft/e2e` pins this.

### Which updates the policy covers

In standalone mode, for a request made with an agent token, the policy is
enforced on every reference update that Gitaly performs, not only on pushes and
OperationService RPCs. The pre-receive hook checks pushes and OperationService
RPCs such as `UserLand`. The reference-transaction hook checks the policy again
while the references are locked, before they change, so RPCs that do not run
pre-receive are covered too; the members of a land batch were checked one by one
before the batch, and the hook does not check them again. A denied update is
aborted as a whole and is not recorded in the changeset log. The server log
names the rule that denied the update. `TestPolicyHoldsForEveryReferenceUpdate`
in `internal/graft/e2e` covers the denials end to end, and the tests of
`internal/gitaly/hook/referencetransaction_policy_test.go` the check in the
reference-transaction hook.

Agents never create, update or delete a reference under `refs/graft/`, whatever
the policy says and in every mode, standalone or not: that namespace is the
server's (land proposals, graft-verify's references). The request filter
refuses a request that names it, `git-receive-pack` hides it from agents on a
push, `UserMergeToRef` and `UserRebaseToRef` refuse it before they write, and
the reference-transaction hook refuses it as the last layer, also through a
symbolic reference. See [AGENT-SECURITY](AGENT-SECURITY.md).

The policy governs the references under `refs/` and the `HEAD` symbolic
reference, so an agent cannot repoint `HEAD` to get around a protected branch.
Requests made with the shared token, or without authentication, are checked
only by the pre-receive hook, as administrative callers.

Agents may call only the RPCs of the agent allowlist (see
[AGENT-SECURITY](AGENT-SECURITY.md)). The RPCs that update references without
pre-receive and are reachable by agents, `UserMergeToRef` and `UserRebaseToRef`
when an operator enables them with `agent_rpcs`, check the policy first and
return `PermissionDenied` with the policy message. Another such RPC enabled
with `agent_rpcs` reports a denial only as a failed reference update, with the
code that RPC gives such a failure (`Internal` or `FailedPrecondition`); the
update is still refused.

Some RPCs replace, remove or rewrite a whole repository, create one from data
or a URL the caller chooses, write references outside the policy or install
server-side hooks. Agents can never call them, not even through `agent_rpcs`;
they are rejected with `PermissionDenied` and need the shared token:

- `RepositoryService`: `CreateRepository`, `CreateFork`,
  `CreateRepositoryFromURL`, `CreateRepositoryFromBundle`,
  `CreateRepositoryFromSnapshot`, `BackupRepository`, `RestoreRepository`,
  `RemoveRepository`, `ReplicateRepository`, `SetCustomHooks`,
  `RestoreCustomHooks`, `WriteRef`, `FetchRemote`, `FetchSourceBranch`,
  `FetchBundle`, `OptimizeRepository`, `PruneUnreachableObjects`;
- `RefService`: `UpdateReferences`, `DeleteRefs`;
- `RemoteService`: `UpdateRemoteMirror`, `FindRemoteRepository`,
  `FindRemoteRootRef` (they push to or query a URL the caller chooses);
- `SSHService`: `SSHUploadArchive` (it reads any revision its input names);
- every RPC of `HookService`, `ObjectPoolService`, `CleanupService`,
  `PartitionService`, `InternalGitaly` and `StorageCoreService`.

`CreateRepositoryFromSnapshot` downloads a tar archive from a URL that the
client chooses and extracts it into the new repository without filtering it.
The archive can carry `custom_hooks`, a `config` or symbolic links, so an agent
could install hooks that later run as the Gitaly user on every write to that
repository. `TestAgentCannotCreateRepositoryFromSnapshot` in
`internal/graft/e2e` pins this. As a consequence, agents cannot create
repositories: an operator creates them with the shared token.

Remaining trust assumption: the hook callbacks (`HookService`) carry their own
payload, as in upstream Gitaly. Agent tokens cannot call `HookService`, but a
client holding the shared token can call it directly and forge that payload.
Expose only the services that clients need, and keep the internal socket that
Git hooks use private.

## Known limitations

### Standalone mode

- The access policy is reloaded from the configuration file on SIGUSR1
  (CONFIGURATION.md, "Reloading the policy"); every other setting, the agents
  and their tokens included, needs a restart. There is no management API for
  policies, and a reload is per node: nodes that share a storage core run
  different policies between their reloads.
- A caller that is not an agent (the shared token, or no token) is checked by
  the policy only in pre-receive, before the references are locked. A reload
  between its pre-receive and its commit can let one of its updates through
  under the previous policy. An agent's update is decided again by the
  reference-transaction hook with the policy in force.
- Policy globs use `path.Match` semantics with a trailing `**`: `prefix/**`
  matches every reference below `prefix/` at any depth (not `prefix` itself),
  and a bare `**` matches every reference. Invalid globs and empty `refs`
  entries are rejected when the configuration is loaded. Globs are not a
  permission language: a rule cannot say "allow only if the change touches files
  under `docs/`".
- A reference that no rule matches is denied while the policy is not empty. A
  policy that does not end with a catch-all rule therefore also denies tags.
- Agent tokens are read at startup. Rotating a token needs a restart.
- The request defaults, which set the identity fields of an agent's request to
  the agent name, run only when `graft.standalone = true`. With agents
  configured but standalone mode off, the `gl_id` and `gl_username` that an
  agent sends are passed to GitLab's `/allowed` check unchanged, so an agent
  can act as any GitLab user. The `HookService`, administrative RPC and
  `git_config_options` denials apply in both modes.
- With agents configured, every request to the external address must carry
  credentials (health checks excepted), including Gitaly-to-Gitaly calls (for
  example `ReplicateRepository` or `CreateFork` across nodes). Those calls use
  the token that the caller passes for the other node in the `gitaly-servers`
  metadata, and the other node accepts it only if it is its shared token, so set
  `auth.token` whenever agents are configured and servers talk to each other.
  Without it they fail with `Unauthenticated`.
- The identity of a token is the agent name, or `shared` for the shared token
  (when agents are configured).
  The policy checks the agent of the token, or the client-sent acting user ID
  for the shared token (see Security limitations above). There are no users, groups or
  per-repository roles.
- Denial messages from the standalone policy name the actor, the reference
  and the rule, for example
  `policy denies "builder" to update "refs/heads/main" (rule 2)` (or `no
  matching rule`). They do not start with `GitLab:`. OperationService RPCs
  return them as `PermissionDenied`, some behind their own prefix (for example
  `creation denied by access checks: ...`); a push shows them on git's
  standard error.

### Repositories

- Creating a repository (`graft repo create`, `Repo.Create` in the SDK) needs
  the shared token: agents may not create repositories. In shadow mode the new
  repository is not tracked until `graft storage import`.
- `graft clone` needs the branch to exist. An empty repository cannot be cloned:
  `graft repo create --initial-commit` gives it a first commit.
- Durability of written objects assumes a local journaling filesystem, ext4 (in
  any `data=` mode) or xfs, not NFS. The objects of a write are made durable by
  their writer, and the migration of the write's quarantine into the repository
  makes the new names durable with one fsync per directory: that relies on a
  hard link being one journaled operation and on the fsync of a directory making
  every earlier change of the journal durable. On other filesystems an object
  written just before a crash may be missing although the write succeeded.
- Custom hooks get the server's `core.fsync` configuration, so the objects that
  their git commands write into a quarantine are durable. When a custom hook
  runs with a quarantine of the server's own write RPCs (land, commit, merge and
  the other operations), the migration of that quarantine also fsyncs its files,
  because a hook may write objects without git; that costs about one fsync per
  file of the write. Pushes over SSH or HTTP are different: git's receive-pack
  migrates its own quarantine, so objects that a custom hook writes there
  without git are only as durable as the hook makes them.
- A push of fewer than 100 objects (most pushes) can fail, rarely, if
  housekeeping's `git prune-packed` runs on the same repository at the same
  instant: git receive-pack unpacks such a push into loose objects in its
  quarantine, and the migration of the quarantine creates a loose-object
  directory (`objects/xx`) and links into it; prune-packed removes empty such
  directories, so a removal between the two fails the link. Retry the push;
  nothing is corrupted and no reference changes. The objects migrated before
  the failure stay in the repository unreferenced, as in the next item. A
  larger push keeps a pack, which is not affected. The server's own write RPCs
  are not affected either: their migration creates a removed directory again.
- The objects of an update that fails after the quarantine migration remain in
  the repository unreferenced until housekeeping prunes them. They have passed
  the pre-receive hook. Until pruned, they can be read by object ID by anyone
  with read access to the repository. This happens when a land loses its
  compare-and-swap (a batch attempt leaves one pack, an unbatched land loose
  objects), when the expected old revision of a write is stale, when the update
  hook or the policy check of the reference-transaction hook refuses the update,
  and after a crash between the migration and the reference update.

### Landing (`UserLand`)

- The rebase strategy rejects ranges that contain merge commits with
  `InvalidArgument`. Use `STRATEGY_MERGE` for those.
- Conflicts are reported. The server resolves them only when the client sets
  `resolve_conflicts` and an operator has configured a resolver for that actor
  and path (see [RESOLVE](RESOLVE.md)). Otherwise the client must rebase its
  change and land again.
- Retries are bounded by `max_attempts` (default 20). Under very heavy
  contention on one branch, a land can fail with `reference_update`, and the
  client decides whether to retry later. In standalone mode, lands on an
  existing branch are batched (see [LAND-BATCHING](LAND-BATCHING.md)): a batch
  retries the whole batch at most 10 times, and `max_attempts` applies to lands
  that run unbatched and to the fallback after a rejected batch.
- On repositories that use the MVCC reference backend, `max_attempts` does not
  apply. A land works on a snapshot pinned for the whole request, and a
  concurrent writer is only detected when the snapshot is published. The land
  then fails after one attempt with `reference_update`, and the client has to
  land again. The changeset log only lists the lands that were published.
  Landing again is safe: when the concurrent writer landed the same change, the
  new land answers `already_landed`. The SDK does not retry `reference_update`.
- A change that landed is not landed again from the same `commit_id` on the
  same branch while the commit it landed as is on the branch, also when a
  later commit reverted it: the land answers `already_landed` and names the
  revert when its message says "This reverts commit <id>" and it is among the
  1000 commits closest to the tip (the land identity of [PLAN11](PLAN11.md)). Landing it again needs a new commit: a revert of
  the revert, or the change committed again. A branch reset past the land
  lands the change again.
- The land identity is recorded in the changeset log. With the file changelog
  (storage core off or in shadow mode) and `[graft.changelog] disabled = true`,
  nothing is recorded, and a retried rebase land replays the change: a change
  reverted in between lands again. In authoritative mode the land identity is
  looked up in the storage core's database whatever that setting is.
- In the file changelog, every land update writes and fsyncs its intents
  (`graft/changelog.intents`) in the prepared phase, under the references'
  locks, and a land whose intents cannot be written fails. The intents of an
  update that git never committed stay in the file: one line of about 200
  bytes per land of that update (a batch update has one per member).
- A land that diverges reads the land identities before it rebases or
  merges, and fails when it cannot: an error reading the changeset log or the
  identity index, or a timeout on their lock files, fails the land rather than
  risking a second land of the same change. This fail-closed behaviour is
  deliberate; the error is `Internal` in the file changelog and `Unavailable`
  when the storage core's database cannot be read.
- In the file changelog, the land identity index (`graft/changelog.lands`)
  holds every land of the repository. The idle indexes of a process are kept
  in memory within 64 repositories and 256 MiB, least recently used dropped
  first; a dropped index is read from its file again on the next land of its
  repository that diverges.
- Hooks run on every attempt, so a slow custom hook adds latency under
  contention.
- `graft land --strategy merge` and `ff_only` are available in the CLI. The
  CLI's default strategy is rebase.

### ObjectService and caching

- `GetObjects` takes at most `max_objects` object IDs per request. The SDK
  splits larger reads automatically, but raw gRPC clients must batch. The SDK
  sends 500 IDs per request at first. When the server refuses a request as too
  large, the SDK lowers its batch size to the server's limit and keeps it for
  later calls. `sdk.WithObjectBatchSize` sets the size from the start.
- Gitaly reads each object through the catfile cache and streams it in chunks
  as it reads it. The cache tier, however, holds all the objects of a request
  in memory before it sends the first message (within `--max-request-bytes`,
  below), so very large blobs and very large batches are not a good fit there.
- The cache tier serves `GetObjects`, and `GetTree` only when a `tree_oid` is
  given, because revisions move. Every other RPC is forwarded uncached.
- A `GetObjects` request through the cache tier may hold at most
  `--max-request-bytes` of objects that are not in the in-memory cache, whether
  they come from the disk store or the upstream. A larger request is streamed
  through the upstream and not cached. A `GetTree` listing larger than the
  smaller of `--max-bytes` and `--max-request-bytes` is streamed through and not
  cached either, so every request for it reaches the upstream.
- The memory tier is per process and is not shared between cache instances.
  A restart empties it. Cache tiers do not coordinate invalidation. None is
  needed for immutable objects.
- The request-level cache checks use the caller's credentials. The cache tier
  is one trust boundary: clients must reach it only through the boundary they
  use for Gitaly.
- A tier started with `--token` serves any client that sends no authorization
  header, using that token upstream. Such a client gets the access of the token,
  not its own. Only start a tier with a token when everybody who can reach its
  socket or address may use that token. See ARCHITECTURE.md, Trust boundaries.

### Changeset log and derived data

- With the storage core off or in shadow mode, the changeset log is one JSON
  Lines file per repository. Its offset index (one point every 256 sequences)
  lives only in the memory of the process: the first read of a long log after a
  restart, or after the process dropped the log's cached state, reads the file
  from the start. In authoritative mode the log is a database table indexed by
  sequence. Neither is ever trimmed.
- Only reference updates that pass through Gitaly's transaction path are
  recorded. Updates made by other tools that write to the repository directly
  are not.
- With the storage core off or in shadow mode, the file log is written by the
  reference-transaction hook after git committed the update, so a recording
  error cannot fail the update: the committed updates are then missing from the
  log, and its consumers do not see them. Each loss is counted
  (`graft_changelog_records_lost_total`) and logged with the references. The
  sequences stay dense, because an entry that failed to be written was never
  visible and the next entry takes its sequence. In authoritative mode the
  storage core writes the changelog in the database transaction of the update,
  so this cannot happen.
- Entries become visible to `ListChangesets`, `WatchChangesets` and the storage
  core only once they are fsynced, and an RPC returns only after its entries
  are, so a reader never sees an entry that a crash could take back. The entries
  of the transactions that commit together are written with one fsync, in the
  order in which the transactions locked their references. A transaction that
  reports its outcome more than 5 s after it locked its references is recorded
  when it does, out of that order, and counted
  (`graft_changelog_unordered_records_total`).
- When an append fails and cutting off the bytes it wrote fails too (two I/O
  errors in a row), only the process that wrote them remembers that they are not
  entries, and it forgets this when it drops its cache of the log. Another Gitaly
  process, during a graceful restart, can then read those bytes as entries or cut
  entries after them. A later version persists that marker next to the log.
- The log is stored per Gitaly node and is not replicated. Behind Praefect,
  every replica records the updates it applies itself, so sequence numbers and
  metadata differ between replicas. Praefect therefore always routes
  `ListChangesets` and `WatchChangesets` to the primary. After a failover, the new
  primary's log becomes authoritative, and clients must not reuse a cursor from
  the old primary's log.
- Temporary branches of `graft commit` appear in the log (`commit_files`,
  `delete_branch`). Filter by reference or operation (`land`, `land_batch`) to
  follow only lands.
- With `[graft.derived] eager = false` (the default), derived data is computed on
  first read, so the first `Blame` on a long history walks that history. With
  `eager = true` only the `graph` and `changed-paths` kinds are computed in the
  background; `lastmod` and blame are still computed on first read.

### Clients

- The Go SDK (`sdk/`) is the only supported client library. Other languages can
  use the generated gRPC stubs, but no bindings are provided.
- The server runs on Linux and macOS only, and CI tests it on Linux only. The
  `graft` CLI and the SDK also run
  on Windows (`go build ./cmd/graft`); CI builds them there, starts the CLI and
  runs the workspace tests.
- `graft` is a sparse, server-centric workspace for agents, not a replacement
  for `git`. Commands that need the full object graph locally, such as `git log`
  inside the workspace, need a normal Git clone.
- `graft` does not speak the Git protocol for `git push` and `git pull`. Use the
  server's normal Git protocol for that.
- In authoritative storage mode, a push without `--atomic` that git rejects one
  update of because of its old value (a branch another client just created, or a
  reference whose value the serving node does not have) is refused by the storage
  core, where a plain git server would land the other references. git still hands
  the rejected update to the reference-transaction hook, and the storage core
  cannot tell the other rejected updates of the push apart. git runs such a push
  in two transactions, the deletions first, then the other updates:
  - a rejected deletion fails the first transaction, and nothing of the push
    lands;
  - a rejected create or update fails the second one. The deletions of the push
    that git accepted have landed by then (in the database and on the node),
    without a status line for them and without the post-receive hook.
  Either way the client sees `fatal: in 'prepared' phase, update aborted by the
  reference-transaction hook` (over SSH) and the connection ends, with no status
  per reference. Check which references moved, and push the others again. The
  same holds for a push that the database refuses for any other reason (a
  compare-and-swap lost to another node). See [STORAGE](STORAGE.md#failures).
- `graft` does not materialize repository paths that are reserved, such as
  `.git` or `.graft` directories at any depth, or that are not safe file names
  on the client's system, such as names with `:` or `\` on Windows. It lists
  them as warnings after `clone`, `update`, `sparse`, `commit` and a land.
  Such paths stay unchanged on the server, and commits from the workspace do
  not touch them.

### Operations and scale

- The end-to-end tests run on one machine. Several nodes that share one
  database and one bucket are tested there (`internal/graft/e2e`, for example
  `storage_multinode_test.go`); nodes on separate machines are not.
- Praefect is not part of standalone mode. Praefect-based deployments keep
  upstream behaviour. `UserLand` is registered with Praefect as a
  transactional mutator, but Graft's tests cover only the routing of the
  changeset RPCs to the primary. Most upstream Praefect tests need PostgreSQL.

## Roadmap

Ordered roughly by the order we intend to work on them. None of this is a
commitment.

1. Eager derived data for `lastmod` and blame when a change lands, with metrics
   for hit rates. Phase 3 computes `graph` and `changed-paths` eagerly.
2. Changeset log retention, and an offset index of the file log that survives
   a restart.
3. Client-side help for every conflict: `graft land --resolve --conflicts-dir
   DIR` writes the conflicts of a land (the merged files with conflict markers,
   the three sides and the resolver's proposals) into DIR, and
   `graft land --resolved DIR` lands the result. A land without `--resolve`
   that fails with `merge_conflict` writes nothing yet, and the files are not
   written into the workspace itself (see [RESOLVE](RESOLVE.md)).
4. Multi-node cache tiers with explicit invalidation for mutable keys.
5. Signed changesets and a verifiable export of the log.

## Phase 4b limitations

1. The phase 4a acceptance is met by two runs on one machine and one workload
   (seed 1, conflict rate 0.2; see [BENCH](BENCH.md#batched-landing-phase-4b)).
   The runs vary a lot, the benchmark saturates the machine, and the stretch goal
   passed by only 3.7% in one of the two runs.
2. Batches still run one at a time per lane, and each batch spends most of its time
   publishing (packing its objects and the update with its hooks and the
   changeset log). Pipelined batches, which would overlap the publish of one batch
   with the work of the next, are not built: a profile showed that the inherent part
   of the publish (about 65-70 ms) is smaller than the member work of a batch, and
   that its growth under load comes from two causes outside the lane, which are
   planned next: the quarantine migration fsyncs again every file that is already
   durable, and every reference update of a repository waits for the changeset-log
   order of the updates before it (shared with `UserCommitFiles`). See PLAN8,
   section 9.
3. Each published batch adds one small pack to the repository, as a push does.
   Housekeeping repacks them like the packs of pushes, so busy branches make
   repacks more frequent. A batch attempt that loses its compare-and-swap (to
   another node or an external writer) also leaves its pack in the repository,
   unreferenced, until housekeeping prunes it: at most one per lost batch attempt.
   Unbatched lands publish loose objects and leave no pack.
4. The objects of a batch are written without fsync and made durable by the pack
   written before the update. An operator who sets `core.fsync` in the `[git]`
   configuration overrides this for the helper processes too (the configuration
   of the administrator is applied last), which keeps the old per-object fsyncs.
5. An unbatched land decides and rebases like a batch attempt (one range listing and
   the rebase engine) and publishes its objects loose, but it is still bounded by its
   own compare-and-swap retries under contention (`max_attempts`).

## Phase 4a limitations

(Items 1 and 2 are resolved by phase 4b, see above. The text describes phase 4a.)

1. Batches still run one at a time per lane. Pipelined batches (phase 4b) are not
   implemented, so a lane's throughput is bounded by its batch duration. The
   latency gain is meant to come from the streaming rebase engine and early
   completion. The acceptance criteria are checked by
   `examples/graft/demo/acceptance-phase4.sh`, for one machine and one workload
   (seed 1, conflict rate 0.2). **They are not met by the final phase 4a tree**:
   one attempt per landed change and failures limited to planned conflicts hold
   at every size, but p99 with batching on is above off at 50 and 100 agents in
   both runs of the final tree (an earlier run passed by 76 ms at 50 agents), and
   the stretch goal is missed. The lane is busy for almost the whole run, and
   landing the members one after another (about 100 ms each) is the largest share
   of a batch, ahead of the reference update with its hooks; see
   [BENCH](BENCH.md#measured-phase-4a-results).
2. The rebase engine is used only by the batch path. The single-land path
   (`land.go`) still rebases with `localrepo.Rebase`, so an unbatched land does not
   gain from it.
3. `[graft.land] graph_ancestry` is off by default. Turning it on answers
   ancestry from the `graph` index of the repository. If the index cannot be
   built, or the repository has shallow commits, grafts or replace refs, the
   server falls back to `git` and logs it. Both answers are the same, but the
   speed-up is only measured where the flag is on.
4. Signed commits are not written by the streaming commit writer
   (`ErrCommitWriterUnsupported`). Batches that commit with signing fall back to
   the ordinary writer for those commits, and gain less from the engine.
5. A batch of one is recorded as an ordinary `land` with the requester as actor,
   not as a `land_batch` changeset. It keeps its single member (original commit
   to landed commit). Consumers that look only for `land_batch` entries (for
   example, a superfan rule that counts batches) must also read `land` changesets
   with one member, or they see such lands as plain lands.
6. Adaptive sizing reacts to the duration of the last batches of one lane, in
   memory. It does not know why a batch was slow (a hook, the disk or a large
   rebase), and its state is lost when the process restarts or when the key
   (storage, repository and branch) has had no land for 5 minutes.
7. An early answer (see [LAND-BATCHING](LAND-BATCHING.md#early-completion)) is
   final and is not revised when the CAS later fails. Policy denials and invalid
   commit IDs are answered early wherever the request is in the batch. A conflict,
   an already landed answer or a tip-dependent `InvalidArgument` is answered early
   only while no earlier request of the batch has changed the running tip.
8. Commit-graph writes are off by default since phase 4a. A server that relied on
   the phase 3 default must set `[graft.derived] commit_graph = true`. See
   [DERIVED](DERIVED.md#commit-graph-writes).

## Phase 3 limitations

1. Land batching is per Gitaly process and in memory. Lanes of two nodes do not
   coordinate, and requests queued when the process stops are not landed. See
   [LAND-BATCHING](LAND-BATCHING.md#limits).
2. A land that arrives alone waits up to `[graft.land.batch] window` (10 ms by
   default) before its batch runs.
3. Eager computation is best effort. Its listings of new commits and its warmers
   each run on `[graft.derived] workers` workers with a queue of `queue_size`
   transactions; a burst beyond that is dropped and counted
   (`graft_eager_dropped_listings_total`, `graft_derived_warm_dropped_total`),
   and the dropped commits are computed on the first read.
4. `[graft.derived] queue_size` counts batches, one per committed transaction,
   not commits. See [DERIVED](DERIVED.md#eager-computation).
5. (Changed in phase 4a: commit-graph writes are opt-in through
   `[graft.derived] commit_graph`. The text below describes phase 3.) Commit-graph
   writes after updates ran by default, whether or not `eager` is set, unless
   `[graft.derived] disabled` is true. They are debounced, but they
   do use disk and CPU on busy repositories. They also run on servers that are
   not standalone, so a plain Gitaly with no `[graft]` section writes commit
   graphs after updates unless it sets `disabled`. See
   [DERIVED](DERIVED.md#commit-graph-writes).
6. `graph` and `changed-paths` are computed eagerly. (Changed in phase 4a: lands
   read the `graph` index when `[graft.land] graph_ancestry` is on; the text below
   describes phase 3.) No other RPC uses the `graph` index. `PathHistory` uses stored `changed-paths` summaries only as
   a pre-filter of its existing walk and never computes them, so they speed up a
   cold query only when `eager` is on.
7. The combined update of a batch runs as the synthetic user `graft-batch`. A
   custom server hook (for example a custom pre-receive hook) that rejects that
   update makes every request of the batch land alone. Batching then gives no
   speed-up for that repository. The policy never rejects the combined update:
   its members were admitted one by one, and the pre-receive access check and
   the reference-transaction policy check are skipped for it. A
   `[[graft.policy]]` rule that matches `graft-batch` therefore has no effect,
   and there is no per-branch way to opt out of batching; only
   `[graft.land.batch] disabled` turns it off, for the whole server. See
   [LAND-BATCHING](LAND-BATCHING.md#hooks-and-identity-of-the-combined-update).
8. The changelog entry of a batch has the actor `graft-batch`. The actual
   requesters are recorded in its members, which are truncated beyond 32 KiB.
   `graft-superfan` judges each member as a land of its own and reports a
   batch whose member list was cut as `batch-incomplete`. See
   [SUPERFAN](SUPERFAN.md#batched-lands).
9. A failed `UserLand` reports its number of attempts only in the message of
   the error for a branch that kept moving ("giving up after N attempts"); the
   error details have no field for it. `graft-bench` reads the count from that
   message and counts no attempts for other failures, such as merge conflicts
   or hook rejections. See [BENCH](BENCH.md#metrics).

## Phase 2 limitations

1. Conflict resolution is only as good as the resolver. `trivial`, `union` and
   `rerere` handle mechanical conflicts. Semantic conflicts, such as two
   changes that are each correct but break a build together, still need the
   verifier or a human. See [RESOLVE](RESOLVE.md).
2. Resolution is attempted only for changes requested with `resolve_conflicts`,
   and only for actors and paths the operator allows. Other conflicts keep the
   `merge_conflict` error. Conflicts in paths that are not valid UTF-8 are
   never resolved, because the protocol carries paths as JSON strings.
3. AI resolvers and summarizers are optional, run outside the server, and their
   output is not verified beyond the answer checks and the configured checks.
   Every AI-resolved commit is marked with `Graft-Resolved-By`.
4. The verifier tests one branch at a time, bisects only first-parent history,
   and reverts with an operator credential. A flaky test can still cause a
   wrong revert when the flake check does not catch it. Caps per hour limit the
   damage. See [VERIFY](VERIFY.md).
5. The verifier does not run tests in isolation from the host. Sandboxing the
   test command is up to the operator.
6. The agent RPC allowlist is fixed in code, with an extension list in the
   configuration. Per-agent allowlists are not supported. See
   [AGENT-SECURITY](AGENT-SECURITY.md).
7. Superfan rules are deterministic and configured by globs. They do not learn
   what is normal for a repository. Summaries are only as reliable as the model
   behind the `llm` summarizer.
8. `graft-bench` measures lands on shared branches (one by default, or the
   `--branches` it is given, each shared by its agents) and a simulated merge
   queue. It does not run real tests, and its conflict rate is a stress setting,
   not a forecast. See [BENCH](BENCH.md).

## Reporting problems

Open an issue in the repository that contains this fork. Include the Graft
version (the commit), the configuration with secrets removed, and the output of
the failing command.
