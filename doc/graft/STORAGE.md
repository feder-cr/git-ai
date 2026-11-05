# Graft storage core

The storage core keeps the state of every repository in a strongly consistent
database plus object storage, so that Gitaly servers become caches that any
node can rebuild. It is configured in the `[graft.storage]` section (see
[CONFIGURATION.md](CONFIGURATION.md#graftstorage)) and is off by default.

This page covers the architecture, the modes, the configuration, the operations
(import, verify, switching modes), the benchmark and the limits. The binding
designs are [PLAN6.md](PLAN6.md) (the storage core: modes, write path,
catch-up, import) and [PLAN7.md](PLAN7.md) (milestone 3: local reference
writes, checkpoints, garbage collection, review roots, background
maintenance). Where they differ, PLAN7.md is the later decision.

## Architecture

```plaintext
            clients (SDK, graft CLI, git over SSH/HTTP)
                 |                       |
          +------v------+         +------v------+
          |   node A    |         |   node B    |      any node serves any repository
          |  gate       |         |  gate       |      (authoritative mode)
          |  voter      |         |  voter      |
          |  syncer     |         |  syncer     |
          |  local git  |         |  local git  |      local repositories are caches
          +--+-------+--+         +--+-------+--+
             |       |               |       |
   changesets|       |packs          |       |
             v       v               v       v
       +-----------+  +--------------------------+
       | database  |  | object storage (bucket)  |
       | SQLite or |  | repos/<id>/packs/        |
       | Postgres  |  |   <sha256>-<nonce>.pack  |
       +-----------+  +--------------------------+
```

- **Changesets.** Every reference change is a changeset appended to a
  per-repository write-ahead log in the database (`graft_changesets`). Its
  sequence is dense per repository and starts at 1. The current references are
  in `graft_refs`, `HEAD` included (a symbolic reference is stored as
  `ref:<target>`).
- **Packs.** The new objects of a changeset are uploaded **before** the
  changeset is committed, as one immutable pack under a key of its own,
  `repos/<repo_id>/packs/<sha256>-<nonce>.pack`: the SHA-256 of its bytes,
  checked on every write and read, and a random nonce drawn for that upload.
  A pack object belongs to exactly one changeset: two writes with byte-identical
  packs store two objects, and a write never relies on an object it did not
  upload. Objects are created only if their key is absent, atomically: on S3,
  GCS and Azure by a conditional request (untested here); on file buckets by
  link(2), because gocloud's file driver does not make its own create-only write
  atomic (it checks that the key is absent, then renames onto it, so two writers
  of one key would both succeed). A file bucket therefore needs a filesystem
  with hard links. Never remove or replace a directory of a file bucket while a
  server uses the bucket: a node remembers which directories it has made
  durable, by inode, and a directory created again could reuse an inode it
  remembers. A changeset never references a pack that is not completely
  uploaded. Changesets written by v0.1.0 keep their keys
  (`repos/<repo_id>/packs/<sha256>.pack`, shared by identical packs), which
  stay readable; the database refuses such a key in a new changeset, so every
  node must run the same version.
- **Write path (the voter).** Gitaly runs the reference-transaction hook for
  every reference update. In the Preparing phase, before git locks the
  references, the voter builds the pack of the new objects and uploads it: the
  objects that no local reference reaches yet in authoritative mode, where
  every local reference value went through the database, and the objects that
  no value recorded in the database reaches in shadow mode, where some writes
  are not recorded. In the Prepared phase, while git holds the
  reference locks, it only reads the expected old values (one `for-each-ref`)
  and commits the changeset with a per-reference compare-and-swap against
  `graft_refs`, in one database transaction. The Preparing phase never fails a
  write: when it did not prepare the pack (it failed, or the transaction did not
  run it), the Prepared phase builds and uploads the pack itself. Gitaly's own
  reference updates (`OperationService` writers, `UserLand` included) run the
  Preparing phase in both modes. For the transactions that git runs the hook
  for itself (pushes, `WriteRef`, `UpdateReferences`, `DeleteRefs`),
  `gitaly-hooks` forwards git's `preparing` state in authoritative mode, so
  their pack is built before the lock too; in shadow mode it does not forward
  it (as upstream Gitaly, which keeps Praefect's votes unchanged), so their
  pack is built in the Prepared phase, under the lock. In authoritative mode
  the database commit is the commit point: if it fails, git aborts the
  transaction. A compare-and-swap conflict is reported to the land path like a
  lost git race, so `UserLand` catches up and retries.
- **Read path (the syncer).** Before a request is served, the node catches its
  local repository up to the database head: it downloads and verifies the
  packs of the missing changesets, indexes them, and moves the changed
  references to their final values without hooks. A node without the
  repository hydrates it: a new bare reftable repository built from changeset
  1, renamed into place. A watermark file `<gitdir>/graft/storage/applied`
  (`{"repo_id":N,"seq":M}`) records how far the local copy is.
- **The gate.** In authoritative mode, gRPC interceptors on every listener
  catch the repository up before every repository-scoped RPC, register
  repositories that `CreateRepository` creates, delete the database row before
  `RemoveRepository` runs, and reject the mutators whose reference changes do
  not go through the hook (see [RPCs in authoritative mode](#rpcs-in-authoritative-mode)).
- **Changelog and reviews.** In authoritative mode the Graft changelog
  (`ChangesetService`, `graft log`, `graft watch`) is written in the same
  database transaction as the reference change, and reviews are stored in the
  database. A watcher on another node sees new entries by polling every 250 ms.
- **Review commits are kept by references.** `CreateReview` and `UpdateReview`
  write a hidden reference `refs/keep-around/review/<commit>` for the head and base of
  the review before they record it, so an open review keeps its commits alive
  after their branch is deleted: on every node (housekeeping keeps what
  references reach), in checkpoints and through garbage collection. A review of
  a commit that only the serving node has works: the reference write uploads it.
  Closing a review deletes the references that no other open review uses; a
  closed or landed review keeps nothing. The open reviews of a repository are
  bounded by `[graft.review] max_open_reviews` (default 1000, see
  [REVIEW.md](REVIEW.md)). Clients never see these references over
  the git protocols and cannot push or delete them. In authoritative mode the
  database enforces the rule: a review write requires the references, and no
  write (`DeleteRefs`, `WriteRef`, `UpdateReferences`, a push) can delete or
  move one that an open review uses. In off and shadow mode the references are
  written as well (housekeeping keeps the commits), but nothing enforces them:
  the review files are the source of truth there. Each review creation and each
  head move costs one reference transaction, so one changeset and no changelog
  entry. GitLab's own `refs/keep-around/<commit>` references are never touched.
  The daily housekeeping of every repository (on by default) also writes the
  references that open reviews lack and deletes those no open review uses,
  before it prunes objects. In authoritative mode every checkpoint build does
  the same first, so a checkpoint, and the garbage collection behind it, never
  goes without the commits of an open review (a build whose backfill fails
  records no checkpoint); watch `graft_review_roots_sweep_failures_total` and
  `graft_review_roots_missing_total`. A review written by an older version
  whose commit was lost since can still be read and closed, but `UpdateReview`
  refuses it with `FailedPrecondition` naming the review and the commit.
- **Repository IDs.** A repository is identified by its storage name and
  relative path, and gets an ID that is never reused: a repository removed and
  created again is a new repository, and the old caches are recognized as
  stale by the ID in their watermark.

Everything read from the database or the bucket is validated before it touches
a local repository: reference names with git's rules, targets with the
repository's object hash, and every pack against the SHA-256 in its key.

## Modes

`[graft.storage] mode` selects one of three modes.

| Mode | Source of truth | What the storage core does |
|---|---|---|
| `off` (default, also empty) | local git | No database or bucket is opened. What milestone 3 adds outside the storage core still applies: open reviews keep their commits by `refs/keep-around/review/<commit>` references (bounded by `[graft.review] max_open_reviews`, see [REVIEW.md](REVIEW.md)), and the daily housekeeping backfills and sweeps them. |
| `shadow` | local git | Every reference transaction of an imported repository is also committed to the database, with its pack. The database never rejects a write: a compare-and-swap mismatch is recorded as a forced changeset and counted as a divergence. Any storage core error, timeout or panic is logged and counted, and the git write proceeds. Reads, the file changelog and the file review store are unchanged. Works with or without Praefect. |
| `authoritative` | database | A reference transaction commits only if its database compare-and-swap succeeds. Every request first catches the local repository up. The changelog and the reviews are served from the database. Requires `[graft] standalone = true`. |

Shadow mode is for validating the storage core next to a running deployment:
writes are mirrored, nothing is served from the database, and `graft storage
verify --connectivity` tells whether the database and the bucket follow git:
equal references, and every object they reach in the packs of the bucket. A
git write that shadow mode did not record (a counted shadow error, or a write
that landed while the repository's import was not complete) shows up as a
divergence until a later write records the reference as a forced changeset;
its objects are uploaded with the pack of the next write that builds on them.

Shadow mode never fails or noticeably delays a git write: the storage-core work
of one reference transaction (building and uploading its pack, then recording
it in the database) runs under `shadow_timeout`. A transaction whose work takes
longer is **not recorded**: the failure is counted in
`graft_storage_shadow_errors_total` and logged at error level, and the git write
proceeds. The database misses that write until the next write of the same
reference, which is recorded as a forced changeset (counted in
`graft_storage_shadow_divergences_total`); until then `graft storage verify`
reports the reference as diverging. Recording every write is therefore
guaranteed only while the shadow work of every transaction fits the budget; on a
heavily loaded machine some writes may not fit. Switch to authoritative mode only
when those counters and `graft_storage_untracked_writes_total` are at zero and
`graft storage verify --connectivity` is consistent on every repository (see
[Switching](#switching-from-shadow-to-authoritative)).

Authoritative mode serves reftable repositories only. Repositories created
through `CreateRepository` are created with the reftable backend, and the syncer
treats a local copy that is not reftable as a stale cache and hydrates a
reftable copy instead. A request for another reference backend is rejected.

### RPCs in authoritative mode

Repository-scoped RPCs that only read, and maintenance RPCs, run after the
catch-up. Mutators run only if their reference changes go through the
reference-transaction hook; the allowlist is
`internal/graft/corestore/gate/policy.go`:

- `OperationService`: every `User*` writer, including `UserLand`.
- `ConflictsService/ResolveConflicts`.
- `RefService/UpdateReferences` and `DeleteRefs`.
- `RepositoryService/WriteRef` (including `HEAD`), `CreateRepository`,
  `RemoveRepository`.
- `SSHService/SSHReceivePack` and `SmartHTTPService/PostReceivePack` (pushes).
- `ReviewService` writers.

Every other mutator fails with `FailedPrecondition` ("... is not supported in
authoritative storage mode"). This includes the RPCs that write references
without the hook: `CreateRepositoryFromURL`, `CreateFork`,
`CreateRepositoryFromBundle`, `CleanupService/RewriteHistory`,
`ObjectPoolService/FetchIntoObjectPool`, and the fetches (`FetchRemote`,
`FetchBundle`, `FetchSourceBranch`, `ReplicateRepository`). `HookService` and
`StorageCoreService` RPCs are never intercepted.

A repository that exists on disk but was never imported is answered with
`FailedPrecondition` ("repository is not imported into the storage core").

## Configuration

```toml
[graft]
standalone = true                               # required by authoritative mode

[graft.storage]
mode = "authoritative"                          # off | shadow | authoritative
database = "sqlite:///var/opt/graft/meta.db"    # or postgres://graft@db.internal:5432/graft?sslmode=verify-full
bucket = "file:///var/opt/graft/objects"        # gocloud URL: file://, s3://, gs://, azblob://
node_id = "node-a"                              # default: the host name
sync_timeout = "10s"                            # budget of one sync round per request
shadow_timeout = "5s"                           # shadow work budget per transaction
gc_grace = "1h"                                 # age before a dead pack is deleted
gc_margin = "5m"                                # writers' deadline is gc_grace - gc_margin
checkpoints = "auto"                            # auto (default in authoritative mode) | off
gc = "auto"                                     # off (default) | auto: background garbage collection
maintenance_interval = "1h"
checkpoint_changesets = 1000                    # 0 = never by count
checkpoint_pack_bytes = 268435456               # 0 = never by size
lease_ttl = "10m"
```

Every key is described in [CONFIGURATION.md](CONFIGURATION.md#graftstorage).
Credentials are never in the TOML file: Postgres reads the standard `PG*`
environment variables and `.pgpass`, and buckets use the standard environment
of their cloud SDK (`AWS_*`, `GOOGLE_APPLICATION_CREDENTIALS`, `AZURE_*`).

All nodes of a deployment use the same `database` and `bucket` and different
`node_id` values. SQLite is for one node (the tests also run several servers
on one SQLite file, at the cost described in [Limits](#limits)); a deployment
with more than one node needs Postgres and a shared bucket. The database schema is
created and migrated at startup.

Startup fails when the mode is unknown, authoritative mode is set without
`standalone = true`, the database URL is missing or neither `sqlite:///` nor
`postgres://` (`postgresql://`), the bucket is missing or not a URL, `node_id`
contains spaces or slashes, a duration is not a positive Go duration,
`gc_grace` is not greater than `sync_timeout + gc_margin`, or `checkpoints` or
`gc` is `"auto"` outside authoritative mode. In authoritative mode the
background checkpoints run by default and the background garbage collection
does not:
- A checkpoint deletes nothing (it adds one pack and its rows), and it is what
  keeps a hydration short. Without one, a node that joins replays every
  changeset of the repository.
- Garbage collection deletes packs, so it is turned on deliberately
  (`gc = "auto"`), or run with `graft storage gc`. Until it runs, the bucket
  only grows.
- Space cost with the defaults (checkpoints on, `gc` off). The bucket holds
  every changeset pack (it grows with the history), plus the checkpoints at or
  above `C_gc`: the newest checkpoint at least `gc_grace` old and the newer
  ones. The background checkpoints drop the older ones, under the same rule as
  garbage collection (a checkpoint below `C_gc` has no reader), so a repository
  keeps about one or two checkpoints, not one per `checkpoint_changesets`.
- A checkpoint is a whole copy of the repository: one pack of every object
  reachable from its references, about the size of a fresh clone.
- With `gc = "auto"` the changeset packs that a checkpoint `C_gc` covers go
  too, and the bucket holds about one or two checkpoints plus the packs of the
  changesets after `C_gc`.
- Orphan packs stay until garbage collection runs: those of failed writes, and
  of a drop interrupted between the row and the pack.

The metrics are `graft_storage_maintenance_*` (runs, checkpoints, packs and
bytes deleted, skipped repositories, failed steps).

## Operations

The `graft storage` commands are the operator's view. They take the server
from the workspace, or `-address`, `-storage` and `-repo` without one, and
`GRAFT_TOKEN` should be the shared token (`[auth] token`).

```shell
graft storage import -address unix:/var/opt/graft/run/gitaly.sock -repo group/project.git
graft storage state  -address unix:/var/opt/graft/run/gitaly.sock -repo group/project.git
graft storage verify -address unix:/var/opt/graft/run/gitaly.sock -repo group/project.git --connectivity
```

`--json` prints one JSON object. `verify` exits with an error when a reference
diverges or the connectivity check fails.

### Import

`graft storage import` (`StorageCoreService/ImportRepository`) registers an
existing repository: changeset 1 with operation `import` holds every reference
(`HEAD` included) and one pack with every object they reach, and the file
changelog and the file reviews are copied with their sequences and IDs, so
that the cursors of changelog consumers stay valid. It works in shadow and in
authoritative mode.

On a repository that is already imported, the import brings in the history
written into the files since then (shadow mode writes the file changelog and
the file review store only): every file changelog entry with a sequence greater
than the database's, with its sequence, every file review whose ID the database
does not have, and every review that an earlier import copied and that was
changed in the files since then (same creation time, later update time; the
database copy is replaced and gets a new version). A review that the database
changed later than the file is kept. References are not touched, and running it
again imports nothing new. The response reports the current head sequence and the
numbers of entries and reviews imported (no references and no pack), and
`graft storage import` prints `already imported at changeset N: imported K
changelog entries and R reviews written since`. It only reads the local copy
that was imported into that registration (its watermark names the repository
ID); another local repository at the same path is answered with
`AlreadyExists`.

The import is resumable. It registers the repository first (with its changelog
and reviews), then uploads the pack, then commits changeset 1. If it fails in
between, nothing is rolled back: the repository stays registered with head
sequence 0, which every node treats as an import that is not complete (requests
fail with `FailedPrecondition`, writes are refused in authoritative mode, and
the local repository is never touched). `graft storage state` shows such a
repository as tracked with head sequence 0. Run the import again; it resumes
with the same repository ID and commits changeset 1.

A files-backend repository can be imported. In authoritative mode the next
request replaces its local copy with a reftable copy hydrated from the storage
core; the old copy, with its file changelog and file reviews, is deleted. The
replacement waits until the database holds that history: while the file
changelog has entries beyond the database's last sequence, or a file review
created or changed in shadow mode is not in the database yet, requests fail
with `FailedPrecondition` ("the shadow-mode history of this repository is not
in the storage core yet: run graft storage import") and the copy is left
alone. To convert the repository in place
instead, which keeps its files, run `git refs migrate --ref-format=reftable`
on it before switching to authoritative mode.

A repository with the MVCC reference backend cannot be imported.

### State and verify

`graft storage state` shows the mode, whether the repository is tracked, its
ID, the database head sequence, the sequence this node's copy has applied, and
the node ID. It does not catch up, so it shows a node that is behind.

`graft storage verify` compares every local reference with the database and
lists the divergences. With `--connectivity` it also runs `git fsck
--connectivity-only` on the local copy and checks the bucket: it indexes the
pack of the newest checkpoint, if there is one, and the pack of every changeset
after it into an empty repository in the storage's temporary directory (a dry
hydration: it downloads what a hydration would) and checks that every object
the database references reach is there. A failure of
either check is reported as a connectivity error; a bucket problem starts with
`storage core objects:`. In authoritative mode it catches the local copy up
first, so a divergence means a real inconsistency. In shadow mode it compares
git with what the database recorded. A catch-up that fails because the database
holds references git cannot hold together is reported as that, with the way out
(see [Failures](#failures)).

If the bucket check fails (objects of a git write that the storage core did not
record, left out of the packs of the writes on top of it by a version before
the shadow packs excluded the database references), the database state of the
repository cannot be rebuilt on another node. Repair it in shadow mode, with
the writers of the repository stopped: delete the repository's row from
`graft_repositories` (`DELETE FROM graft_repositories WHERE storage = '<storage>'
AND relative_path = '<relative path>'`; its changesets, references, changelog
and reviews go with it) and run `graft storage import` again. The new
registration gets a new repository ID and a full pack, and takes the changelog
and the reviews from the files again, so do this before switching to
authoritative mode. The deletion records the tombstone of the old ID (a
database trigger, whoever deletes the row), so garbage collection deletes the
old objects `gc_grace` later. Rows deleted this way before this release left
their objects in the bucket; garbage collection finds them and records their
tombstones (see `gc --removed` below).

### Checkpoints and garbage collection

How it stays safe:
- Every write uploads its pack under a new key of its own, and commits the
  database row that references it within `gc_grace - gc_margin` of a database
  read that found the repository. A write that misses that deadline fails, and
  its pack is an orphan.
- Garbage collection deletes a pack only when:
  - no row references it, and the collector itself observed that at least
    `gc_grace` earlier;
  - or it is covered by a checkpoint created at least `gc_grace` ago.
- Every "how old" decision uses the database clock: Postgres's `now()`; with
  SQLite, the host clock, since SQLite serves one host. The clocks of the
  nodes never decide.
- The writer's deadline is checked on the database clock, in the commit's own
  transaction. The deadline is the database time of the record read that
  preceded the upload plus `gc_grace − gc_margin`. Garbage collection observes
  a pack it may delete on the same clock, and only after that read. So a
  committed row never references a deleted pack, however long the writer
  stopped: a suspended host, a paused VM, a long garbage-collection pause of
  the process.
  - The writer also stops its upload early on its own monotonic clock. That
    only saves work; it is not what makes it safe.
  - The one assumption left is that the database clock does not step backward
    by more than `gc_margin` (5 minutes by default). A forward step is safe:
    it makes the commit check refuse earlier.
- `gc_grace` must also be greater than `sync_timeout + gc_margin`, so that a
  sync that started from an older checkpoint never meets a pack deleted under
  it.

```sh
graft storage checkpoint -address unix:/var/opt/graft/gitaly.sock -repo @hashed/ab/cd/x.git [--json]
graft storage gc         -address unix:/var/opt/graft/gitaly.sock -repo @hashed/ab/cd/x.git [--dry-run] [--json]
graft storage gc --removed -address unix:/var/opt/graft/gitaly.sock [-storage default] [--dry-run] [--json]
```

- `checkpoint` (authoritative mode) consolidates the repository at its current
  database head: one pack of every object reachable from its references, and
  the reference snapshot. A fresh node then hydrates from the checkpoint
  instead of replaying every changeset, and garbage collection can delete the
  packs the checkpoint covers. A checkpoint that exists at the head is
  reported, not built again.
- `gc` deletes the packs of one repository that no reader needs any more:
  - the changeset packs covered by a checkpoint at least `gc_grace` old, and
    the checkpoints that one supersedes;
  - the objects no database row references, once garbage collection observed
    them unreferenced at least `gc_grace` earlier (the packs of writes that
    died before their database commit). The first run only observes them.

  It runs in shadow mode too (orphans only: there are no checkpoints). It holds
  the repository's maintenance lease while it runs, renewed like the background
  maintenance's, and fails with `FailedPrecondition` when another maintenance
  of the repository (the background one, another node) holds it, or `Aborted`
  when the lease is lost during the collection.
- `gc --removed` deletes every object of the repositories of the storage that
  were removed at least `gc_grace` ago. It first reconciles the bucket: a
  repository ID allocated at least `gc_grace − gc_margin` ago that has objects
  in the bucket and neither a row nor a tombstone (a row deleted by hand before
  this release) gets a tombstone, so its objects go `gc_grace` later. A run
  examines at most 256 IDs, from where the previous run stopped, and reads at
  most one object of each; it prints what it found. Objects of IDs above the
  database's allocator (a database restored from an older backup) are reported
  with a warning and never touched: the database will hand those IDs out again.
  The background maintenance does the same with `gc = "auto"`.
- `--dry-run` reports what would be deleted and changes nothing. It is a usage
  error on the subcommands that do not read it, like `--connectivity` outside
  `verify` and `--removed` outside `gc`.

These commands are for operators: they need the shared token. A per-agent token
can never call the storage core service, whatever `agent_rpcs` says.

### Upgrades

Upgrading from v0.2.x to this release changes the storage core's database
(migrations `20261018000000_graft_storage_gc` and
`20261019000000_graft_storage_reconcile`: the tombstone trigger and the state
of the bucket reconciliation) and the keys of the packs in the
bucket (owned keys, see Packs). A node of the old version cannot work next to
a node of the new one:
- **It cannot start.** A v0.2.x node that restarts after the migration
  refuses the database, with an error about the unknown migrations.
- **It cannot read.** A v0.2.x node still running reads only the old 64-hex
  pack keys, so its reads of a repository that a new node wrote fail with
  "invalid pack key".
- **It cannot write.** Its writes carry old keys, and the database refuses
  them ("the pack key is not an owned key; every node must run the same
  version").

So upgrade the whole deployment at once:
1. Stop `gitaly serve` on every node.
2. Install the new binaries on every node.
3. Start the nodes. The first one migrates the database; the others find it
   migrated.

**Downgrade is not supported past this migration.** The database keeps the
data of the new version (checkpoints, tombstones, owned keys) that a v0.2.x
binary cannot read, and there is no down migration that would make the
bucket's owned keys readable to it. Going back means restoring the database and
the bucket from a backup taken before the upgrade.

A binary that finds a database migrated by a newer version refuses to start:
"the database was migrated by a newer version of the storage core (unknown
migrations ...): this binary cannot run on it; every node must run the same
version, so upgrade this node". This is how a node left on the older release
shows up after the next upgrade, too.

### Switching from shadow to authoritative

1. Run every node in shadow mode and import every repository
   (`graft storage import`). Writes that commit while an import is running
   (between its read of the references and the commit of changeset 1) are not
   recorded: they are counted in `graft_storage_untracked_writes_total` and
   logged. Stop the writers of a repository while it is imported, or check
   that counter afterwards and run `graft storage verify` on the repositories
   that were written to.
2. Watch `graft_storage_shadow_divergences_total`,
   `graft_storage_shadow_errors_total` and, once every repository is imported,
   `graft_storage_untracked_writes_total`; all must stay at zero. Run `graft
   storage verify --connectivity` on every repository: it must report
   `consistent`. (The connectivity check covers the local copy and the bucket,
   see [State and verify](#state-and-verify). The bucket stays complete even
   after a write the storage core did not record: shadow mode builds each pack
   against the values the database records, not against the local references,
   so the next recorded write of a reference uploads every object its value
   reaches that no recorded value reaches.)
3. Stop the writers and run `graft storage import` once more for every
   repository, on the node whose files hold its shadow-mode history: it brings
   the changelog entries and the reviews written in shadow mode since the import
   into the database (see [Import](#import)).
4. Set `standalone = true` (if Praefect is in use, remove it: authoritative mode
   does not support Praefect) and `mode = "authoritative"` on every node, and
   restart. Each node catches its copies up on first use; files-backend copies
   are replaced by reftable copies, which deletes their file changelog and file
   reviews (they are in the database after step 3).

Shadow mode does not write the database changelog, so without step 3 the
database changelog would reuse the sequences of the file entries written in
shadow mode, and the reviews created in shadow mode would not be served. In
authoritative mode the gate therefore refuses a repository whose local file
changelog has entries beyond the database's last sequence, or whose file review
store has a review created or changed in shadow mode that is not in the
database yet, with `FailedPrecondition` ("the shadow-mode history of this
repository is not in the storage core yet: run graft storage import"), until the
import brought them in. The check runs before the catch-up, on the local copy
as it is (the catch-up may replace a files-backend copy, files included), reads
the last sequence of the file log and the reviews, and is remembered per
repository and process once it passed. The catch-up itself also refuses, with
the same error, to replace a local copy whose file changelog or file reviews
hold such history, also for the requests that do not go through the gate
(`VerifyRepository`, the retries of the land paths), so a forgotten step 3
never deletes that history.

### Switching back (rollback)

Authoritative mode keeps complete git repositories (objects and references) on
every node that served a repository, so the references and objects need no
export:

1. Stop the writers. On the node that will keep serving, run `graft storage
   verify` for every repository: in authoritative mode it catches the copy up
   to the database head.
2. Set `mode = "shadow"` (or `"off"`) on that node and restart it. Without the
   storage core in authoritative mode there is no catch-up between nodes, so
   serve each repository from one node again (or put Praefect back in front of
   the nodes).

The Graft changelog and the reviews are a different matter: after the
rollback they are served from the files of the local copy again, and there is
no export from the database into those files. What the files hold depends on
how the copy came to be:

- **A copy that was never replaced** (a reftable repository imported in shadow
  mode on this node, or a files-backend one converted in place with `git refs
  migrate` before the switch) has the file changelog and the file reviews as
  they were when authoritative mode started. Everything of the authoritative
  period (changelog entries and reviews) is in the database only and is not
  served.
- **A hydrated copy** (every copy on a node other than the one that imported
  the repository, a copy rebuilt after a removal or a repository ID change,
  and every files-backend repository that authoritative mode replaced by a
  reftable copy, also on the importing node) has no file changelog and no
  file reviews at all. After the rollback its changelog is empty and its
  reviews are gone, also those from before authoritative mode; the next write
  gets file changelog sequence 1 and the next review ID 1, so the cursors of
  changelog consumers are invalid and review IDs are reused.

Choose, for every repository, a serving node whose copy was never replaced if
the changelog and the reviews matter, and tell changelog consumers to reset
their cursors otherwise. The database keeps everything of the authoritative
period until the repository's row is deleted.

Switching to authoritative mode again after a rollback does not merge
histories. The database's changelog continues from its own last sequence, and
`graft storage import` brings in only the file entries with a greater sequence,
and the guard compares only the last sequences. When the file log after the
rollback stays at or below the database's last sequence (a hydrated copy that
restarted at 1, typically), the guard passes and the entries written after the
rollback are never imported. When it goes beyond it, the guard refuses until
the import, which then appends only the entries above the database's sequence,
whose sequences have a different history in each log. Reviews follow the import
rule of [Import](#import): a file review whose ID the database already has for
another review (one created in the authoritative period) is skipped, and a
review that both sides have from before the first switch is replaced by the
file's copy when the file's update time is later, which drops the changes the
authoritative period made to it in the database. Before switching back, compare `graft log` with the files,
or re-import the repository from scratch (see
[State and verify](#state-and-verify) for deleting its row).

### Failures

- **Database or bucket unavailable.** Authoritative mode: writes fail with
  `Unavailable` and git aborts them; requests that need a catch-up fail with
  `Unavailable` after `sync_timeout`. Shadow mode: writes proceed, the failure
  is logged and counted.
- **A node dies after uploading a pack and before the database commit.** The
  pack is an orphan: no changeset references it, no node serves it, and the
  write can be retried. Garbage collection deletes an orphan `gc_grace` after
  a collection first observed it (see [Garbage collection](#garbage-collection)).
- **A node dies after the database commit and before git committed.** The
  database has the write, other nodes serve it, and the node's own copy is
  behind until its next request catches it up. The counter
  `graft_storage_local_commit_failures_total` counts the transactions that git
  aborted after the database commit.
- **A node with an empty disk.** Its first request for a repository hydrates
  it.
- **A push with an update git rejected for its old value.** Without `--atomic`,
  git lets one update of a push fail and still shows it to the
  reference-transaction hook, without marking it. Two cases: a create of a
  branch another client just created (the old value is zero, the reference
  exists), and an update or deletion whose old value is not the node's local
  value. The second case can find the database holding that old value, when
  another node moved the reference and moved it back and this node's copy is
  between the two (an ABA that the client did not see), so the database's
  compare-and-swap would accept an update the client is told was rejected. The
  database must record neither, so the storage core refuses the transaction
  that holds the update. The storage core tells this by comparing every old
  value of a push with the local value it reads while git holds the reference
  lock, which git checked the update against. What lands depends on git, which
  runs a non-atomic push in two transactions, the deletions first, then the
  other updates, each with its own reference-transaction hook and commit:
  - a rejected deletion fails the first transaction: nothing of the push lands;
  - a rejected create or update fails the second: the deletions git accepted
    have landed already, in git and in the database, and the other references
    of the push do not land, unlike a plain git server.
  Either way receive-pack dies: the client sees `fatal: in 'prepared' phase,
  update aborted by the reference-transaction hook` (over SSH; over smart HTTP
  only that the remote end hung up), no status per reference, and the
  post-receive hook does not run, also not for the deletions that landed. Check
  which references moved and push the others again. A push whose transaction
  the database refuses for another reason (a compare-and-swap lost to another
  node, a name conflict) behaves the same way. In shadow mode the write proceeds
  unrecorded, and counts in `graft_storage_shadow_errors_total`. Shadow mode on
  a files-backend repository also counts one such error for the packed-refs half
  of a deletion of a packed reference (`0 0 <ref>` while the reference still
  exists): noise only, the loose half records the deletion.
- **Two nodes create conflicting names.** git cannot hold `refs/heads/a` and
  `refs/heads/a/b` together, and each node's git only sees its own copy. The
  database refuses, in its commit, a changeset that would leave such a pair
  (also forced, so also in shadow mode), so the repository stays something
  every node can apply. A database that holds a pair already is repaired as
  the next item says. What a client sees in authoritative mode (shadow mode,
  below, never refuses a git write):
  - **A push** (SSH or smart HTTP, with or without `--atomic`): the update
    hook, which git runs for each reference before its transaction, refuses a
    new reference that conflicts with a reference of the database. The client
    gets `remote: refusing refs/heads/a/b: git cannot hold it next to
    refs/heads/a; push the other references without it` and `! [remote
    rejected] refs/heads/a/b (hook declined)` where a plain git server says
    `(refname conflict)`. Without `--atomic` the other references of the push
    land as usual. A name the pusher may not see is not named: an agent reads
    `a reference hidden from you` instead (in practice receive-pack already
    refuses such names with `deny updating a hidden ref`). Names are shown
    with control, format and bidirectional characters escaped (`\u202e`).
  - **A push that creates both names of a pair**, such as `refs/heads/a` and
    `refs/heads/a/b` in one push without `--atomic`, is refused as a whole.
    The update hook cannot see the conflict, since each
    name conflicts only with the other. git refuses one of the two names, but
    still hands both to the reference-transaction hook (a known git behaviour
    this release does not work around), and the database refuses that
    changeset. receive-pack then dies: nothing of the push lands, except its
    deletions, which git commits first in a transaction of their own (see the
    previous item). Over SSH the
    client sees the `refusing ...` line as a plain line, and `fatal: in
    'prepared' phase, update aborted by the reference-transaction hook`. Over
    smart HTTP git sends the output of that hook to no client, so the client
    only sees `fatal: the remote end hung up unexpectedly`. Push the names
    separately; one of them git cannot hold in any case. The same happens when
    another node commits the conflicting name between this push's update hook
    and its commit.
  - Where the server drives the reference transaction, the code of git's own
    refusal: `FailedPrecondition` from `UserCreateBranch` and `UserCommitFiles`,
    for example. Through a git process that the server runs without a push,
    `Internal` (`UpdateReferences`), as for git's own such conflict.
  - If the update hook cannot read the database, it refuses the reference
    (`hook declined`), as any write needs the database in this mode.

  **Shadow mode never refuses a git write because of the database**, which may
  be behind git there. A push behaves as on a plain git server: git refuses a
  name only when it conflicts with the node's own copy. The update hook logs a
  name that conflicts with a reference of the database and counts it in
  `graft_storage_shadow_errors_total`, and so does a failed read. The database
  still refuses to record a changeset that would leave a pair (it is forced,
  but this check holds), so it records none of the references of such a push,
  and none of a non-atomic push in which git refused a conflicting name (git
  hands the hook the refused name too). `graft storage verify` reports the
  references the database misses; the next write of each records it.

  Cost: an update or deletion of an existing reference reads nothing more. The
  commit of a changeset that creates references adds one primary key lookup of
  their parent names and one primary key range query of the names below them,
  each per 500 created names, in the commit's transaction. **Known limit:** a
  push's update hook adds, for each reference it creates, a read of the
  repository's row, one primary key lookup of the parent names and one range
  probe, each in a read transaction of its own: measured at about 0.04 ms on
  SQLite and 2.0–2.5 ms on Postgres per created reference
  (`BenchmarkRefNameCheck`, local Postgres in Docker), so a push that creates
  1,000 references spends about 2–3 s more in its update hooks on Postgres, more
  over a network.
- **A repository removed on another node.** The local copy has a watermark
  whose key the database no longer has; it is moved into the storage's
  temporary directory, and the repository is reported missing on that node too.
- **A database that already holds such a pair.** Only a version before this
  refusal can have written one. Every catch-up and hydration of the repository
  fails with `Internal` on every node (`cannot process 'refs/heads/a' and
  'refs/heads/a/b' at the same time`, or `conflicting reference updates in the
  same transaction` on a hydration), so the repository serves nothing until one
  side is gone. `graft storage verify` names the pairs and their values. Nothing
  repairs this by itself, because either side may be the one that matters:

  1. Choose the reference to delete, and note its value, which `verify` prints.
  2. Run `graft storage delete-conflicting-ref REFERENCE EXPECTED_TARGET`. It
     refuses a reference that is not part of a pair or has another value, and
     records an ordinary changeset that the nodes apply when they catch up. Both
     checks run in the transaction of that changeset: once one side of a pair
     is gone, a request for the other side is refused, so two operators who
     delete the two sides at once delete only one. It also fails while the
     reference is the root of an open review.
  3. Create the deleted reference again under another name at the value you
     noted, if you still want it. Do this before the next checkpoint: the pack
     that holds its objects stays until a checkpoint that does not reach them
     covers it and garbage collection runs at least `gc_grace` after that
     checkpoint.

  It needs authoritative mode. In shadow mode no node catches up from the
  database and git's copy is what the database must learn, so repair a shadow
  database as the bucket check above says, by importing again.

- **Local reference writes and unclean shutdowns (authoritative mode).** In
  authoritative mode the database commit is the durable point of a reference
  update. A node therefore writes its local references without fsync, and makes
  them durable in a background step a second after a repository goes idle
  (`doc/graft/PLAN7.md`, LOCAL REFERENCE WRITES RULE). Until then the
  repository has a dirty marker in `<storage>/+gitaly/graft-ref-durability/`,
  which holds the kernel's boot identity. The watermark of a dirty repository is
  written without fsync too, and the background step makes it durable before it
  removes the marker.
  - *Process crash or restart* (same kernel boot): nothing is lost, because the
    page cache kept every write. The restarted node settles the markers.
  - *Kernel crash or power loss*: the references written since the last durable
    step may be lost or torn, and git reads a torn reftable stack as an empty
    repository without an error. At startup, before it opens any listener,
    `gitaly serve` moves aside every repository that was dirty under the
    previous boot, and the first request hydrates it again from the database and
    the bucket. This hydration starts from the newest checkpoint when there is
    one, so its cost is bounded by how recent the checkpoints are (the
    background maintenance creates them, see `checkpoints`; with checkpoints
    off, by the full history).
    Nothing acknowledged is lost. Cost after a power
    loss: one hydration per repository that had a write in the second before the
    loss, or that was still being written. On a busy node that can be every
    active repository. A repository is only moved when the database holds it
    with changesets; other markers are dropped.
  - *A write the kernel reports as failed* (a writeback error, same boot): the
    durable step's fsync fails, the repository is moved aside at once and logged,
    and the next request hydrates it. The kernel reports such a failure once, so
    from then on the repository is untrusted. Every request for it fails with
    `Unavailable`, and its marker is never removed, until it is moved aside.
    "Every request" means from the next one on. Each request checks once, in
    its sync round, so a request already past its round when the failure was
    found completes on what it read. `graft storage import` refuses an
    untrusted repository too. If
    the move fails, it is retried every second. A repository that is not a
    cache of the storage core (the database cannot rebuild it) stays in place,
    untrusted and not served, for an operator. Other errors of the step (a file
    that cannot be opened for a moment) are retried every second and counted.
  - *Process restart*: a marker found is kept while the repository's reftable
    directory holds a git lock or a table being written (a git process of the
    previous process may still be finishing), and for 2 s at least after the
    restart and after the last such file seen. A repository the previous
    process had found untrusted is moved aside.
  - A marker stays while a git process of the node that writes without fsync
    is running, even when its repository was moved aside meanwhile: the
    process names the repository by its path, so a hydrated copy there would
    get its writes.
  - **Kept copies.** A repository moved aside that may hold shadow-mode history
    not imported yet goes to `<storage>/+gitaly/graft-untrusted/<stale-*>/repo.git`
    and stays there. That is history written in shadow mode (file changelog
    entries, file reviews) that the database does not have yet, or history
    files that cannot be read. The repository itself is hydrated from the
    database by its next request.
    `graft_storage_kept_untrusted_repositories` and
    `graft_storage_kept_untrusted_bytes` report the kept copies per storage. No
    request can name a path under `+gitaly/`: the gate and the storage core
    RPCs reject it with `InvalidArgument`.

    To import what a kept copy holds:
    1. Take the node out of service: no client traffic to it during the
       procedure. `gitaly serve` keeps running.
    2. Move the repository's current copy at `<storage>/<relative path>` out of
       the storage, then move the kept `repo.git` to `<storage>/<relative path>`.
    3. `graft storage import -address <node> -storage <storage> -repo <relative path>`.
       It imports the file changelog entries and the reviews the database does
       not have, without touching the references.
    4. Remove the copy at `<storage>/<relative path>` again: its references are
       not trusted. The next request hydrates the repository from the database.
       `graft storage verify -repo <relative path>` must then report
       `consistent`.
    5. Remove the kept directory, and put the node back in service.

    A kept copy whose history files cannot be read cannot be imported: repair
    them, or extract what is readable by hand. Removing a kept copy loses only
    its history that was not imported.
  - *Downgrade*: not supported past this release's storage migration (see
    [Upgrades](#upgrades)). A v0.2.x binary would also ignore the markers.
  - *Clean stop*: after the servers stopped, `gitaly serve` makes every dirty
    repository durable and removes its marker, so a clean reboot hydrates
    nothing.
  - *Graceful upgrade* (`GITALY_UPGRADES_ENABLED`): the new process serves while
    the old one drains. One process per storage owns the deferred durability, by
    an exclusive lock on `<storage>/+gitaly/graft-ref-durability/owner.lock`.
    Until the old process exits, the new one writes references with fsync, as
    in the other modes; then it takes over (`graft_storage_ref_durability_owner`).
    During the overlap two processes also open an SQLite database, and their
    writes meet in SQLite's own lock wait, see Limits; Postgres is not affected.
  - Offline tools that read a repository from disk (`gitaly git -C <repo>`,
    `gitaly-debug simulate-http-clone`) must not be pointed at a repository of
    a node after an unclean shutdown before `gitaly serve` has started once:
    they would read the torn stack.
  - Scope: Linux only (the boot identity is `/proc/sys/kernel/random/boot_id`).
    Elsewhere, and in off and shadow modes, git makes every reference write
    durable as before.
  - The node's reference writers (hook transactions and catch-up) wait for the
    repository's reference lock in arrival order instead of git's polling.
- **A corrupt dirty marker** stops `gitaly serve` at startup with the marker's
  path: the repository it named cannot be identified. Check the storage, then
  remove the marker; the repository's next request hydrates it if it is stale.

Milestone 3 is proved by `internal/graft/e2e/storage_gc_test.go` and
`storage_localrefs_test.go`, on SQLite and Postgres:
- a repository written on two nodes is checkpointed on one; the other collects
  the covered packs once the checkpoint is `gc_grace` old;
- a node that joins afterwards hydrates from the checkpoint and serves every
  write, and writes go on after the collection;
- orphan packs and removed repositories are collected;
- with the real `gitaly` binary: the deferred reference durability across
  process kills, a simulated power loss and a graceful upgrade; the background
  maintenance; and the daily housekeeping that backfills the keep-around
  references of a review from before the REVIEW ROOTS RULE.

These behaviours are proved by the end-to-end tests
`internal/graft/e2e/storage_*_test.go`, with several real servers sharing one
database and one bucket, on SQLite and Postgres: writes on one node and reads
on another, concurrent lands on one branch from two and three nodes (no lost
update, every land in the changelog exactly once), a node that joins with an
empty disk, a repository created on one node and removed on another, crash
injection at both fault points, and the benchmark's mixed load in shadow mode
without a single divergence.

### Metrics

| Metric | Description |
|---|---|
| `graft_storage_changesets_total{mode,result}` | Reference transactions handled by the write path; results `committed`, `noop`, `conflict`, `error`, `skipped`. |
| `graft_storage_preparing_seconds` | Time of the write path in the Preparing phase, before git locks the references (pack build and upload). |
| `graft_storage_preparing_step_seconds{kind,step}` | The steps of the Preparing phase in authoritative mode: `record` (the read of the repository record), `walk` (the objects reachable from the new tips and not from a local reference, until their list ends; observed for every transaction with new tips), `pack` (writing and hashing a non-empty pack) and `upload` (to the bucket). `kind` is `create` for a transaction that only creates references (a commit onto a new branch), `update` otherwise (a land). |
| `graft_storage_prepared_seconds` | Time of the write path in the Prepared phase, while git holds the reference locks (expected values and database commit; also the pack when the Preparing phase did not prepare it). |
| `graft_storage_prepared_step_seconds{kind,step}` | The steps of the Prepared phase in authoritative mode, while git holds the reference locks: `record` (the read of the repository record), `local_read` (the local references the expected values need), `pack` (only for a transaction with new tips whose Preparing phase did not prepare its pack; unlike the Preparing phase's `pack`, it covers the walk, the packing and the upload), `commit` (the database commit of the changeset) and `durability` (the dirty marker, only when reference durability is deferred). Like the Preparing steps, a step is observed only when it succeeded. The steps do not sum to `graft_storage_prepared_seconds`: the reference backend check, the commit deadline of the pack, the bookkeeping of the committed transaction and the error paths are outside them, so their sum is a lower bound of the phase. `kind` as for the Preparing steps. Buckets from 0.5 ms, fine between 1 and 100 ms. |
| `graft_storage_pack_bytes_total` | Bytes of packs uploaded. |
| `graft_storage_shadow_divergences_total` | Shadow mode changesets recorded despite a mismatch. |
| `graft_storage_shadow_errors_total` | Shadow mode failures; the git write proceeded. |
| `graft_storage_untracked_writes_total` | Shadow mode writes of repositories that are not imported. |
| `graft_storage_mvcc_skipped_total` | Shadow mode transactions of MVCC repositories, skipped. Authoritative mode rejects every repository that is not reftable (MVCC included) with `FailedPrecondition` and does not count it here. |
| `graft_storage_local_commit_failures_total` | Transactions committed to the database that git then aborted. |
| `graft_storage_sync_total{result}` | Sync rounds before a request; results `fresh`, `caught_up`, `hydrated`, `partial` (the round ran out of its budget before the head; the request fails with `Unavailable` and the next round goes on from there), `seeding` (the round ended while the repository was being seeded from its checkpoint in the background), `error`. |
| `graft_storage_sync_seconds` | Duration of the catch-up before a request. |
| `graft_storage_sync_changesets_total`, `graft_storage_sync_bytes_total` | Changesets applied and pack bytes downloaded by catch-up and hydration. |
| `graft_storage_sync_checkpoint_seeds_total` | Hydrations that started from a checkpoint instead of changeset 1. |
| `graft_storage_build_cleanup_failures_total` | Build directories of hydrations (`+gitaly/tmp/hydrate-*`) that the startup cleanup could not check or remove; they are kept and logged. |
| `graft_storage_maintenance_runs_total{result}` | Runs of the background maintenance; `ok`, or `error` when at least one step failed. |
| `graft_storage_maintenance_checkpoints_total` | Checkpoints created by the background maintenance. |
| `graft_storage_maintenance_packs_deleted_total`, `graft_storage_maintenance_bytes_reclaimed_total` | Bucket objects, and their bytes, deleted by the background garbage collection. |
| `graft_storage_maintenance_skipped_total{reason}` | Repositories the background maintenance skipped: `no_local_copy`, `nothing_due` (no checkpoint due, no superseded checkpoint, no collection; decided without the lease), `lease_held`. |
| `graft_storage_maintenance_failures_total{step}` | Failed steps of the background maintenance: `reconcile`, `collect_removed`, `list`, `lease`, `checkpoint`, `collect`. |
| `graft_storage_maintenance_leaks_found_total`, `graft_storage_maintenance_leaks_tombstoned_total` | Repository IDs with objects in the bucket and neither a row nor a tombstone, found by the reconciliation, and the tombstones it recorded for them. |
| `graft_storage_maintenance_ids_ahead_of_allocator` | Repository IDs above the database's allocator with objects in the bucket at the last reconciliation (a database restored from an older backup); never touched. Alert on a value above 0. |
| `graft_review_roots_backfilled_total` | Keep-around references of open-review commits written by the backfill (daily housekeeping and before every checkpoint). |
| `graft_review_roots_deleted_total` | Keep-around references deleted because no open review uses them. |
| `graft_review_roots_missing_total` | Open-review commits the backfill found missing from the repository; the review can only be closed. |
| `graft_review_roots_sweep_failures_total{trigger}` | Failed sweeps of review keep-around references: `close`, `housekeeping`. |
| `graft_storage_ref_admission_wait_seconds{writer}` | Wait of a local reference writer (`hook`, `syncer`) for its turn at the repository's reference lock. |
| `graft_storage_ref_admission_waiting` | Writers waiting for their turn, all repositories. |
| `graft_storage_ref_admission_bypassed_total{writer}` | Writers that went on to git's lock without their turn after `reftable.lockTimeout` (10 s). |
| `graft_storage_ref_admission_expired_total` | Turns ended by the 10 s bound instead of their release (a live git process that stayed between its phases, or a hook call without a tracked process). |
| `graft_storage_ref_admission_released_unreported_total` | Turns released because the git process of the transaction ended, or went on to its next transaction, without reporting Committed or Aborted: a prepare that failed in git (a stale old value, for example, in `update-ref` or a push), or a killed process. Each is also logged at INFO. |
| `graft_storage_ref_dirty_repositories` | Repositories with reference writes not yet made durable. |
| `graft_storage_ref_dirty_marked_total`, `graft_storage_ref_dirty_cleared_total` | Dirty markers written, and removed (the writes are durable, or the repository was moved aside, and no git process can still write it). |
| `graft_storage_ref_writes_inflight` | git processes of this node that write references without fsync and have not exited; their repositories keep their markers. |
| `graft_storage_ref_durability_owner` | 1 when this process owns the deferred durability of its storages, 0 while another process (the old one of a graceful upgrade) does. |
| `graft_storage_ref_durable_step_errors_total` | Durable steps that failed with a transient error (not a failed fsync); retried every second. |
| `graft_storage_ref_untrusted_total{reason}` | Repositories moved aside to be hydrated again: `reboot` (dirty at a kernel restart), `sync_failed` (the durable step's fsync failed). |
| `graft_storage_kept_untrusted_repositories{storage}`, `graft_storage_kept_untrusted_bytes{storage}` | Untrusted copies kept in `+gitaly/graft-untrusted/` for an operator (see Kept copies), and their size. Read at every scrape. |

## Benchmark

`examples/graft/demo/bench-storage.sh` runs standalone servers built from the
example configuration (batched landing on), identical except for
`[graft.storage]`: off, authoritative with SQLite, and authoritative with
Postgres, each with a file bucket. graft-bench lands 5 changes per writer with
1, 10 and 50 concurrent writers on one branch per run, and the script reports
throughput, latency and the storage core histograms of each run.

One run on 2026-10-15, after the LOCK AND HISTORY RULE of PLAN6.md (the pack is
built and uploaded in the Preparing phase, before git locks the references, and
git waits up to 10 s for the reftable lock, like for `packed-refs`): WSL2 on an
Intel Core Ultra 7 255H (16 threads), with other test suites running on the
same machine (load average 27 to 28 during the whole run, more than twice that
of the earlier run below, so the absolute times are inflated), a local Postgres
in Docker, conflict rate 0, seed 1. "Preparing" is the
`graft_storage_preparing_seconds` histogram (pack build and upload, outside the
lock), "locked" the `graft_storage_prepared_seconds` histogram (expected values
and database commit, while git holds the repository-wide reftable lock), "sync"
the `graft_storage_sync_seconds` histogram (once per catch-up check before a
request), all as the difference of the scrapes before and after the run (the
script's report has the preparing, locked and sync columns; the preparing call
counts equal the voter transactions and are left out of the table below). Every voter transaction of the run went
through the Preparing phase (equal counts), and no server logged a "reference
is already locked" error or a failed Preparing phase.

| run | landed / changes | lands/min | p50 (ms) | p95 (ms) | voter transactions | preparing mean (ms) | locked mean (ms) | syncs | sync mean (ms) |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| off, 1 writer | 5 / 5 | 156.7 | 405.4 | 458.7 | 0 | – | – | 0 | – |
| sqlite, 1 writer | 5 / 5 | 65.1 | 568.5 | 2126.4 | 11 | 41.62 | 17.22 | 17 | 0.42 |
| postgres, 1 writer | 5 / 5 | 13.4 | 1158.1 | 11815.2 | 11 | 293.89 | 21.58 | 17 | 1.73 |
| off, 10 writers | 50 / 50 | 227.3 | 2281.1 | 3806.7 | 0 | – | – | 0 | – |
| sqlite, 10 writers | 50 / 50 | 145.7 | 3148.9 | 6092.7 | 84 | 79.38 | 29.65 | 136 | 17.94 |
| postgres, 10 writers | 50 / 50 | 185.4 | 3073.8 | 4458.2 | 83 | 63.20 | 18.74 | 123 | 13.98 |
| off, 50 writers | 250 / 250 | 373.4 | 7291.1 | 10921.9 | 0 | – | – | 0 | – |
| sqlite, 50 writers | 250 / 250 | 308.9 | 8640.2 | 13210.3 | 370 | 45.00 | 17.60 | 625 | 7.61 |
| postgres, 50 writers | 250 / 250 | 319.8 | 8101.3 | 13681.3 | 369 | 52.75 | 16.90 | 566 | 12.54 |

The earlier run on the same day, before the fix (load average about 12; its
"voter mean" was the whole voter run, pack and upload included, under the
lock):

| run | landed / changes | lands/min | p50 (ms) | p95 (ms) | voter transactions | voter mean (ms) | syncs | sync mean (ms) |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| off, 1 writer | 5 / 5 | 207.0 | 299.5 | 333.7 | 0 | – | 0 | – |
| sqlite, 1 writer | 5 / 5 | 145.7 | 436.8 | 477.9 | 11 | 41.61 | 17 | 0.36 |
| postgres, 1 writer | 5 / 5 | 113.5 | 542.5 | 612.6 | 11 | 58.04 | 17 | 1.94 |
| off, 10 writers | 50 / 50 | 332.5 | 1673.3 | 2066.3 | 0 | – | 0 | – |
| sqlite, 10 writers | 25 / 50 | 171.8 | 1974.6 | 2412.8 | 43 | 82.07 | 110 | 11.39 |
| postgres, 10 writers | 27 / 50 | 74.0 | 4166.3 | 10786.9 | 48 | 155.69 | 96 | 43.71 |
| off, 50 writers | 250 / 250 | 304.8 | 8186.1 | 14935.5 | 0 | – | 0 | – |
| sqlite, 50 writers | 52 / 250 | 277.5 | 3008.7 | 4253.2 | 69 | 63.79 | 323 | 5.32 |
| postgres, 50 writers | 41 / 250 | 284.3 | 2023.7 | 2623.2 | 56 | 66.67 | 189 | 8.34 |

Lands per minute and latencies are over the landed changes only. What the runs
show:

- **Concurrent writers no longer fail.** Before the fix, 50 to 84% of the
  changes of the concurrent authoritative runs failed in graft-bench's commit
  step (`UserCommitFiles` onto each change's own branch) with
  `FailedPrecondition: reference update: reference is already locked`: reftable
  has one reference lock per repository, the voter held it while it built and
  uploaded the pack and committed to the database, and git waited only its
  default 100 ms for it. Now every change of every run landed (50 of 50 and 250
  of 250 on SQLite and Postgres), with one attempt each. The time under the
  lock is the "locked" column, 17 to 30 ms on average, and the pack work runs
  before the lock is taken.
- **The concurrent runs are now comparable with the off runs**, since they
  land the same changes. Under this load they reach 64 to 82% of the off
  server's lands per minute with 10 writers and 83 to 86% with 50 writers.
- **One writer:** the run had a load average of 27, and its single-writer
  numbers are dominated by it (in the Postgres run the slowest of the five changes
  took 11.8 s, and its Preparing mean is 294 ms).
  The earlier, less loaded run is the better measure of the per-land overhead:
  about 140 ms (SQLite) and 240 ms (Postgres) on a land of about 300 ms. A
  catch-up check that finds the copy fresh costs well under 2 ms.

### Where the Preparing phase spends its time

Measured on 2026-10-20 with `graft_storage_preparing_step_seconds`, to decide
whether a land should upload the pack it already wrote instead of building a
second one.

Setup:
- the same machine;
- 1-minute load below 2 at the start of every run, a fresh server per run;
- authoritative mode, file bucket, gitai land mode, conflict rate 0, seed 1;
- 1 writer × 20 changes and 10 writers × 5 changes, two runs per backend.

The land transactions are `kind="update"`: 20 per run with 1 writer, 14 to 16
with 10 writers, since a batch lands about 3.3 changes in one transaction. The
commits onto the changes' own branches are `kind="create"`.

Mean per land transaction, run by run:

| step | SQLite (ms) | Postgres (ms) |
|---|---:|---:|
| record | 0.2 | 1.1–2.2 |
| walk | 3.3–6.9 | 4.3–5.9 |
| pack | 3.6–6.2 | 3.7–6.8 |
| upload | 7.5–59.0 | 14.6–44.1 |

The creates are in the same size class, for walk and pack alike, with larger
uploads: 9–37 ms on SQLite and 18–130 ms on Postgres.

What this shows:
- **The upload dominates, in the mean and in the tail.** The p99 of a land's
  upload reached 462 ms on SQLite. A commit's upload reached the top bucket,
  about 2.2 s, in both single-writer Postgres runs.
- **The upload is fsync latency.** The file bucket fsyncs every pack file and
  its directories before `Put` returns. On this machine the WSL disk is shared
  with the Postgres container, whose own fsyncs compete with the bucket's. This
  is why the Postgres runs have the worst upload tails. An object-store bucket
  behaves differently, and was not measured.
- **Reusing the land's pack is not worth it.** Reuse could only remove the
  `pack` step: 3.6–6.8 ms per land transaction, about 1–2 ms per land with
  batching. That is before the cost of checking that the land's pack holds
  every object the upload needs. The design required a 10 ms drop of the
  Preparing mean on both backends, so the reuse was not built.

### Where the upload time goes

Measured on 2026-10-23, to decide whether the uploads of a file bucket should
share their directory fsyncs.

Setup:
- `Put` of a file bucket timed step by step: create the temporary file, write
  and hash, fsync the file, link it to its key, remove the temporary name, and
  fsync the directories. The timing was a temporary patch, not shipped.
- The same WSL ext4 disk as above (`data=ordered`), with nothing else running:
  the CPU was at least 98.5% idle and its pressure (PSI, `some avg10`) at most
  2.53 before and after every series.
- Pack keys of one repository (`same-repo`), or of one repository per writer
  (`repo-per-worker`); packs of 4 KiB (the size class of a land) and 256 KiB;
  1, 10 and 50 concurrent writers, 20 uploads each, after one upload per
  repository that made its directories durable.
- Two modes: as shipped, and with the directory fsync skipped. The second mode
  is the upper bound of any scheme that shares or groups directory fsyncs.
- Four series of each mode, alternating. The numbers are the median of the four
  series; the range is the lowest and the highest series.

Mean of one upload, as shipped, in ms:

| layout | pack | writers | upload (range) | p99 | file fsync | directory fsync |
|---|---:|---:|---:|---:|---:|---:|
| same-repo | 4 KiB | 1 | 6.7 (5.5–7.4) | 7.9 | 3.5 | 2.9 |
| same-repo | 4 KiB | 10 | 12.2 (10.5–12.7) | 15.7 | 5.6 | 5.4 |
| same-repo | 4 KiB | 50 | 13.2 (12.6–15.0) | 21.3 | 6.2 | 5.4 |
| same-repo | 256 KiB | 1 | 11.4 (10.9–12.4) | 14.2 | 5.8 | 3.4 |
| same-repo | 256 KiB | 10 | 21.8 (19.8–24.4) | 30.7 | 11.1 | 7.6 |
| same-repo | 256 KiB | 50 | 46.4 (36.9–206.8) | 82.8 | 22.6 | 17.6 |
| repo-per-worker | 4 KiB | 10 | 18.5 (12.3–32.4) | 26.3 | 9.1 | 8.7 |
| repo-per-worker | 256 KiB | 50 | 45.5 (36.4–149.5) | 86.4 | 22.0 | 18.7 |

What this shows:
- **An upload is two fsyncs.** Once its directories are durable, an upload
  fsyncs its file and one directory, each 3–6 ms on an idle disk. The other
  steps take less than 1 ms up to 10 writers.
- **Sharing the directory fsync is not worth it.** Skipping it entirely saves
  0.9–4.3 ms per upload of a 4 KiB pack, and 2.5–3.2 ms for a 256 KiB pack with
  one writer. With 256 KiB packs and 10 or 50 writers it makes the upload
  slower, by 18–100 ms in the mean, with a p99 of 357–575 ms: without the
  directory fsync pacing the writers, each file fsync carries a larger journal
  commit (the file fsync grows from 22 ms to 125–136 ms at 50 writers). The
  file fsync itself cannot be shared, because every pack must be durable before
  its link and its database row. Not built.
- **The tails above come from the disk, not from `Put`.** In isolation, at the
  size of a land's pack and 10 writers, an upload averages 12–18 ms with a p99
  of 16–26 ms. The 462 ms and 2.2 s tails measured during the benchmark come
  from the other writers of the same disk's fsyncs: the database, git and the
  bucket.
- **The variance is large.** Identical series differ by up to 4× at 256 KiB
  and 50 writers, and at 10 writers the sign of the difference between the two
  modes is not stable. Four series per mode were enough to rule out a gain of
  the size we require (10 ms), not to measure small differences.

Where to put a file bucket: on a different device from the database and from
the repositories, so that its fsyncs do not queue behind theirs. An
object-store bucket has no local fsync; it was not measured here.

### Inline compaction stays

Git compacts a repository's reftable stack inline: every reference update that
leaves the stack out of its geometric shape merges tables before it releases
the repository's lock, and Gitaly sets no `reftable.autoCompaction`. Measured
on 2026-10-23, to decide whether that compaction should move out of the write
path, to housekeeping.

Setup:
- The bundled Git with the options Gitaly passes: `core.fsyncMethod=fsync`,
  `reftable.lockTimeout=10000`, and `core.fsync` without `reference` (the
  deferred durability of authoritative mode) or with it (off and shadow mode).
- A fresh reftable repository per run: 1000 branches in one table, and one
  branch per writer. 1, 10 or 50 writers, each updating its own branch 40
  times, one `git update-ref --stdin` transaction (start, update with its old
  value, prepare, commit) per update, as Gitaly runs them.
- Two modes: as shipped, and with compaction off
  (`GIT_TEST_REFTABLE_AUTOCOMPACTION=false`), the upper bound of moving it out
  of the write path. After the writers, the reads of the final stack (median of
  21 runs): `show-ref --verify` of one branch, `for-each-ref` of one branch and
  of every reference; with compaction off, also the `git pack-refs --auto` that
  housekeeping would run next.
- Four series of each mode, alternating, run only on an idle machine (CPU
  pressure at most 0.51, at least 98.6% idle, before and after every series).
  The numbers are the median of the four series.

Deferred durability (authoritative mode), in ms:

| writers | compaction | update mean (range) | update p99 | tables after | read: verify / one / all | pack-refs |
|---:|---|---:|---:|---:|---:|---:|
| 1 | inline | 6.4 (5.7–6.8) | 9.8 | 2 | 4.5 / 3.0 / 12.2 | |
| 1 | off | 8.2 (5.8–12.4) | 15.8 | 41 | 3.4 / 4.5 / 14.2 | 6.9 |
| 10 | inline | 13.3 (9.8–18.2) | 97.9 | 3 | 5.5 / 3.9 / 14.7 | |
| 10 | off | 11.6 (10.8–12.0) | 61.4 | 401 | 6.3 / 6.7 / 17.9 | 22.8 |
| 50 | inline | 37.5 (36.6–49.9) | 455.6 | 4 | 6.1 / 6.0 / 15.1 | |
| 50 | off | 98.5 (97.0–105.4) | 1447.9 | 2001 | 23.8 / 23.1 / 36.5 | 137.0 |

What this shows:
- **Compaction keeps writes and reads cheap.** With compaction inline the
  stack stays at 2–4 tables. Without it, it grows by one table per update, to
  2001 with 50 writers: every update and every read then loads a longer stack,
  so at 50 writers the updates are 2.6× slower in the mean and 3× in the p99,
  the reads 4× slower, and housekeeping inherits a `pack-refs` of 137 ms. At
  10 writers turning compaction off saves 1.8 ms in the mean, below the 10 ms
  we require. Not built.
- **In off and shadow mode the cost is the fsync of every update.** With
  `reference` in `core.fsync`, turning compaction off lowers the update mean
  (from 23.9 to 16.7 ms with one writer, from 135 to 84 ms with 10, from 643 to
  437 ms with 50: a compaction there writes and fsyncs one more table under the
  lock). But the p99 stays at 1.4–1.5 s with 10 writers and 6.4–7.8 s with 50
  in both modes, and at 50 writers 26 (inline) and 63 (off) of 2000 updates per
  series ran out of `reftable.lockTimeout` ("cannot lock references"). The
  updates queue on the repository's lock behind one fsync each; fewer fsyncs
  per update, not less compaction, is what would shorten that queue.

## Limits

- **Git still needs a local repository.** "Stateless" means that the local
  repository of a node is a cache that the node can rebuild from the database
  and the bucket, not that nodes keep no local state. Every request runs git
  on a local copy, a node's first request for a repository downloads the pack
  of its newest checkpoint and every changeset pack after it, and there is no
  virtual filesystem.
- **Pack garbage collection runs in the background only with `gc = "auto"`**
  (authoritative mode; off by default). Otherwise changeset packs, orphans and
  the objects of removed repositories are deleted only when an operator runs
  `graft storage gc` (see [Checkpoints and garbage collection](#checkpoints-and-garbage-collection)).
  The background maintenance still drops superseded checkpoints and their
  packs with `gc = "off"`. The background checkpoints are on by default.
  With `checkpoints = "off"` and no `graft storage checkpoint`, a hydration
  replays every changeset of the repository from 1. Its rounds resume, but that
  takes many requests on a long history.
- **A sync round is bounded by `sync_timeout`.** A hydration or catch-up that
  needs more than one round answers `Unavailable` until it reaches the head;
  each round keeps what it did. The checkpoint pack, a whole copy of the
  repository, is not bound to a round. A node that rebuilds a repository from
  its checkpoint downloads and indexes that pack in the background, for up to
  `gc_grace − gc_margin`, and answers `Unavailable` ("being seeded") until it is
  done. A changeset pack that cannot be downloaded and indexed within one
  round never completes. Measured (2026-10-19, Postgres,
  file bucket, small changesets of the land benchmark): about 4 ms per
  changeset, so at the default `sync_timeout` of 10 s (7.5 s of fetch per
  round) one round covers about 1800–2000 changesets after the newest
  checkpoint. A copy that is further behind needs several requests. With the
  default checkpoints (every 1000 changesets) a repository stays within one
  round. Before the concurrent fetch, the same repository took about 40 ms per
  changeset and never finished at 10 s.
- **Praefect is not supported in authoritative mode.** Configuration validation
  requires `[graft] standalone = true` for authoritative mode. Shadow mode
  works with or without Praefect.
- **S3, GCS and Azure are untested here.** The bucket is any gocloud URL, but
  the test suite and the benchmark use `file://` and `mem://` buckets only;
  the S3 tests run only when `GRAFT_TEST_S3_URL` points at a test bucket.
- **File buckets need a filesystem with hard links** (every local Linux or
  macOS filesystem, and NFS): objects are published with link(2), see Packs. On
  a filesystem without hard links every write fails with an error that says so.
- **Concurrent writes to one repository share one lock** in authoritative
  mode: reftable has one reference lock per repository. Writers take it in
  arrival order, and the hold is the voter's expected values and database commit
  plus a git write without fsync, about 15 ms with SQLite. The pack is uploaded
  before the lock, and the references are made durable later, see
  [Failures](#failures). So concurrent writes queue: about 125–190 ms of queue
  at 50 and 100 agents on one repository, measured. A write whose turn does not
  come within 10 s goes on to git's own wait, and fails with "reference is
  already locked" if that also passes 10 s. See [Benchmark](#benchmark).
- **A repository that is never idle for a second keeps its dirty marker**, so
  after a power loss it is always hydrated again, even though its references
  were made durable less than a second earlier: the marker does not record which
  writes are already durable.
- **SQLite serves one node.** Only `gitaly serve` opens the database: the
  `graft` CLI and the other tools go through the server. SQLite has a single
  write lock. The writes of one gitaly process take it in the order they
  arrive, but several processes on one SQLite file (several nodes on one host,
  or the old and the new process during a graceful upgrade) meet in SQLite's
  own lock wait,
  which keeps no queue: under steady contention an unlucky write can wait past
  the 10 s busy timeout and fail. A deployment with more than one node uses
  Postgres; SQLite is for one node, or for tests.
- **Some mutators are refused in authoritative mode**, see
  [RPCs in authoritative mode](#rpcs-in-authoritative-mode).
- **Switching back from authoritative mode** has no export of the changelog
  and the reviews into the files: those of the authoritative period stay in
  the database only, and a hydrated copy (every copy on the other nodes, and
  every replaced files-backend copy) serves an empty changelog and no reviews,
  whose sequences and IDs then restart at 1, see
  [Switching back](#switching-back-rollback). The switch to authoritative mode
  brings the shadow-mode history in with `graft storage import`. After a
  rollback, the file review store can hand out an ID that a review of the
  authoritative period already has in the database; on the next switch the
  database review is kept and that file review is not imported.
- **No per-directory permissions** in the storage core; access is decided by
  `[[graft.policy]]` as before.

## Clean-room note

The storage core adopts ideas that Anthropic described publicly for its
internal "Scion" system: repository state in a consistent database plus object
storage, and servers that are re-creatable caches (see
[ARCHITECTURE.md](ARCHITECTURE.md)). Scion's internal design and code are not
public and are not reproduced. The implementation was written from the
public description and the existing Gitaly code base only: nothing under the
`ee/` directory of GitLab was read or used, and no code was taken from the
GPL-licensed `git-mvcc` sources in `_build/deps/git-mvcc`. The storage core
uses only `database/sql` with the existing SQLite and Postgres drivers and
gocloud buckets, so it is not tied to one vendor.
