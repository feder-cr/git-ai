# Graft phase 7: pack garbage collection and checkpoint compaction (binding plan)

Status: binding contract for WP71–WP79 (storage core milestone 3). First
version 85189d4; **amended 2026-10-18 after the PM review** (changes A–H, the
answers to the open questions, and the "No patches: refactor the cause" section
of PM.md). It extends the phase-6 foundation (`doc/graft/PLAN6.md`, which stays
binding: the CAS semantics, the write path, the read path and catch-up, the
REFTABLE, IMPORT, STALE CACHE and LOCK AND HISTORY rules, and RULES 1–15). This
plan adds tables, interface methods and packages, and **refactors two phase-6
contracts** (the pack key format and `BlobStore.Put`, see the PACK OWNERSHIP
RULE), authorized by the PM provided every phase-6 storage test is green
afterwards. It never edits a released migration.

Second amendment (PM approval of be50089): change A (per-row pack ownership)
approved; the B/C refinement (GC BOUNDARY rule, no missing-pack restart)
confirmed; the REVIEW ROOTS RULE added (open reviews are a second kind of root
into the object graph, and the review service is now m3's for that check);
the enforcement points made explicit (every upload goes through one deadline
helper, `sync_timeout` and `W` are context deadlines, not assumptions).

Owner workstream: **m3** (`internal/graft/corestore/**`,
`internal/gitaly/service/storagecore/**`, the storage sub-commands of
`cmd/graft`, `proto/storage_core.proto`, the `GraftStorage` part of the config,
`doc/graft/STORAGE.md`, and the storage core block of `internal/cli/gitaly/serve.go`).

## Problem

`doc/graft/STORAGE.md`, "Limits", states two gaps left after the first release:

1. **Packs are never deleted.** Every reference transaction uploads a pack
   before it commits (PLAN6, write path), and nothing ever removes one:
   * **orphans** of writes that uploaded a pack and then did not commit (an
     aborted transaction after the Preparing phase, a node that died before the
     database commit);
   * **superseded** changeset packs, whose objects a later consolidated state
     already contains;
   * the packs of **removed repositories** (`RemoveRepository` cascades the
     database rows, the bucket keeps `repos/<repo_id>/`).
2. **No checkpoint compaction.** Hydration (`syncer.hydrate`) and the dry
   hydration of `graft storage verify --connectivity` replay every changeset
   from sequence 1. Their cost grows without bound with the history.

A third, latent problem was found in review and is the subject of the PACK
OWNERSHIP RULE: **bucket objects are silently shared between writes.** Pack keys
are content-addressed and `BlobStore.Put` returns success without writing when
the key exists, so a write can commit a changeset whose pack is an object it did
not write and whose liveness is decided by other rows. Any collector, however
careful, can then delete an object that a write in flight relies on. Phase 6 never
deletes, so the defect was harmless; phase 7 must remove its cause before it
deletes anything.

## Goal and non-goals

* **Pack ownership.** Every pack object in the bucket is written by, and belongs
  to, exactly one database row (a changeset or a checkpoint). No write relies on
  an object it did not upload.
* **Checkpoints.** Per repository, a checkpoint at sequence `N` = one consolidated
  pack holding every object reachable from the references at `N` and from the
  head and base commits of the reviews open at the time of the build, plus the
  reference snapshot at `N`, recorded atomically after the pack is uploaded.
  Hydration starts from the newest checkpoint and replays only changesets `> N`.
  Created on demand (`graft storage checkpoint` + RPC) and by the background task
  when it is enabled. Writes are never blocked while a checkpoint is built.
* **Pack GC.** Mark and sweep, database authoritative, idempotent, crash-safe
  after every step. Reclaims orphans, changeset packs covered by a checkpoint old
  enough that no reader can still need them, superseded checkpoint packs, and the
  packs of removed repositories. `graft storage gc` (+ `--dry-run`), `graft
  storage gc --removed`, and the background task.

Out of scope (stated as limits or future work in STORAGE.md): per-directory
permissions; S3/GCS/Azure testing (no server here, file and mem buckets only);
renaming anything; local-repository housekeeping (no `git gc` of a node's cache;
the cache can be rebuilt from a checkpoint); compaction of changeset, changelog or
review rows (only packs are deleted).

## Design

### Overview

The expensive, immutable state is the packs in the bucket. The cheap state is the
changeset metadata (`graft_changesets`, `graft_changeset_refs`) and the changelog
(`graft_changelog`). Phase 7 deletes packs and never touches those rows, so the
WAL, the changelog and their consumers' cursors are unaffected. A checkpoint
supersedes the *packs* of the changesets up to its sequence, not their rows.

```
seq:   1    2    3   ...   N (checkpoint)    N+1  ...  head
packs: p1   p2   p3  ...   pN                p_{N+1} ... p_head
                           C_N (consolidated pack of the checkpoint)
rows:  every changeset row is kept (WAL + changelog)
```

`p1..pN` are reclaimed only once the checkpoint `N` is old enough that no
hydration, catch-up or verification that could still read them is running (the
GC BOUNDARY rule). Every pack has exactly one owner row (the PACK OWNERSHIP
RULE), so "is this pack still needed" is a question about its owner row only.

### PACK OWNERSHIP RULE (change A, binding)

**Root cause.** The dedup race reported in review (a write's `Put` finds the key
and skips; GC deletes the key; the write commits a changeset referencing a missing
pack) is not a timing window to be narrowed. Its cause is structural: pack keys
are content-addressed and shared, `Put`-exists makes a write depend on an object
it did not write, and that object's liveness belongs to other rows (an older
covered changeset, or a failed write's orphan). The phase-6 orphan grace was a
time assumption, not an enforced invariant. Both are removed at their source.

**Decision: per-row ownership** (direction 1 of the PM review). Rejected:
database fencing of deletions with a doomed set (direction 2), see "Rejected
alternatives".

1. **Owned pack keys.** A pack key is
   `repos/<repo_id>/packs/<sha256>-<nonce>.pack`, where `<sha256>` is the SHA-256
   of the bytes (64 lowercase hex, still verified on every `Put` and `Get`, RULE
   22) and `<nonce>` is 16 random bytes (`crypto/rand`, 32 lowercase hex) drawn
   for that one upload. Every writer (the voter, `ImportRepository`, the
   checkpoint builder) uploads under a key it just generated
   (`corestore.NewPackKey(repoID, sum)`). There is no cross-row dedup: two writes
   with byte-identical packs store two objects. Legacy keys
   `repos/<repo_id>/packs/<sha256>.pack`, written by v0.1.0 and still referenced
   by its rows, stay readable (`ParsePackKey` accepts both forms) and are never
   written again.
2. **`BlobStore.Put` is strictly create-only.** `Put` writes with
   `blob.WriterOptions{IfNotExist: true}`; an existing key is the new error
   `corestore.ErrBlobExists`, never success. The `Exists` pre-check is removed (one
   bucket round trip less per write). A fresh nonce never collides in practice; if
   it does, the writer fails rather than share an object. This is a phase-6
   contract change: the blobstore tests "Put idempotent" and "concurrent Puts of
   the same key" assert the old requirement and are rewritten to the new one
   (exactly one concurrent `Put` of a key succeeds, the others return
   `ErrBlobExists`, and the stored content is that of the winner).
3. **Database fence for writers that are not upgraded.** A v0.1.0 binary cannot
   start against a migrated database (sql-migrate v1.8.1 refuses an unknown
   applied migration, and the store does not set `IgnoreUnknown`), but a node
   already running v0.1.0 keeps writing with shared keys. Migration 2 therefore
   installs a trigger on `graft_changesets` that rejects an insert whose
   `pack_key` is not NULL and not an owned key
   (`pack_key NOT LIKE 'repos/%/packs/%-%.pack'`). A write from an old binary that
   carries a pack fails at its database commit (authoritative: git aborts it;
   shadow: counted as a shadow error). Writes without a pack are unaffected.
   Operational consequence, documented: upgrade every node together; until a node
   runs the new binary, its pack-carrying writes fail.
4. **In-flight writes have an enforced deadline.** Without sharing, the only
   object a write relies on is its own upload, which is an unreferenced object
   until the write commits. The writer and the collector are bound by one rule:
   * **Writer.** Let `W = gc_grace − gc_margin`. The writer takes the time `t_r`
     (monotonic clock) of the repository-record read (`GetRepository`) that
     precedes its upload. It uploads the pack and commits the row that references
     it under a context whose deadline is `t_r + W`. When the deadline passes, the
     `Put` is cancelled (gocloud discards a cancelled writer, nothing is stored) or
     the commit transaction is not started, and the write fails like any storage
     failure (authoritative: `Unavailable`, git aborts; shadow: counted, the git
     write proceeds). The voter's Prepared phase uses the pack of the Preparing
     phase only when that pack's deadline has not passed; otherwise it builds and
     uploads a new owned pack itself (the existing "Preparing did not prepare"
     path), with a new `t_r` from its own record read. `ImportRepository` and the
     checkpoint builder re-read the record immediately before their upload, so a
     long pack build does not consume the deadline.
   * **Collector.** An unreferenced pack is deleted only when the collector itself
     **observed** it unreferenced at least `gc_grace` earlier, measured on the
     database clock (`graft_gc_orphans.first_seen`, see the CLOCK RULE), and it is
     still unreferenced in the same database snapshot whose time decides the age.
     The bucket's modification time is never used.
5. **Proof (no live pack is deleted).** Let `t_s` be the database time of the
   snapshot in which the collector finds key `K` unreferenced with
   `first_seen(K) <= t_s − gc_grace`. `first_seen(K)` was recorded after a listing
   that returned `K`, so the upload of `K` completed before `first_seen(K)`, and
   the writer's record read `t_r` was earlier still. The writer can commit only
   before `t_r + W < first_seen(K) + gc_grace − gc_margin <= t_s − gc_margin`. A
   commit before `t_s` is visible in the snapshot, so a committed row would
   reference `K`. Since none does, no writer will ever commit `K`, and deleting it
   is safe. The only clock assumption is that the drift of the clock **rates**
   (the writer's monotonic clock against the database clock) over `gc_grace` is
   smaller than `gc_margin`; absolute skew between machines does not enter.
   `gc_margin` also absorbs the latency of a commit that is in flight when the
   writer's deadline passes (a commit message sent just before the deadline may
   still complete after it). **Stated assumption** (repeated in STORAGE.md):
   `gc_margin` > about 2 × (relative clock-rate drift) × `gc_grace` + the longest
   in-flight commit. With ordinary clocks (drift well below 10⁻⁴) 5 min against
   1 h is far above it.
6. **Covered and superseded packs need no time.** A covered changeset pack (its
   owner row has `seq <= C_gc`) or the pack of a dropped checkpoint is owned by a
   committed row, so no writer relies on it (new writers never reuse a key, rule
   1), and its readers are excluded by the GC BOUNDARY rule. It is deleted in the
   run that finds it.
7. **Cost on the write path.** No database access is added. The `Put` loses its
   `Exists` round trip; the nonce and the deadline are negligible; the trigger is
   one `LIKE` per inserted changeset; byte-identical packs are stored twice (rare:
   retried identical pushes). The preparing and prepared histograms and the bucket
   bytes are measured before and after in the WP79 benchmark window.

### CLOCK RULE (binding)

Every time that decides a GC action is a **database time**: `graft_checkpoints.created_at`,
`graft_gc_orphans.first_seen`, `graft_gc_removed.removed_at` and the lease times
are written with the database clock (Postgres `now()` in the statement; SQLite
`time.Now()` of the host, which every node shares because SQLite is single-host),
and every "older than" comparison is made against the database time read in the
same transaction (`now()` on Postgres; on SQLite `time.Now()` taken before the
read transaction begins, so that every commit before it is visible). The writer's
deadline is a duration on its own monotonic clock. The only assumption is the
bounded rate drift of RULE 5 above, covered by `gc_margin`.

### GC BOUNDARY rule (changes B and C, binding)

Two boundaries are computed from the checkpoints, on the database clock:

* **`C_gc`** = the sequence of the newest checkpoint created at least `gc_grace`
  ago (0 when there is none). The collector reclaims changeset packs with
  `seq <= C_gc` and the packs of checkpoints with `seq < C_gc`. It keeps every
  changeset pack with `seq > C_gc` and every checkpoint with `seq >= C_gc`
  (`checkpoint_retain` of the first version is removed: the lag makes it
  unnecessary).
* **`C_sync`** = the sequence of the newest checkpoint created at least
  `gc_grace − sync_timeout − gc_margin` ago (0 when there is none). A sync round
  (catch-up or hydration, bounded by `sync_timeout` through its context) decides
  with `C_sync`: **re-hydrate when the watermark is below `C_sync`; otherwise
  catch up** (change C: a node that is a few changesets behind when a checkpoint
  is created catches up normally; the checkpoint does not count for it until it
  is about `gc_grace − sync_timeout` old).

Why no reader ever misses a pack. A checkpoint becomes `C_gc` only `gc_grace`
after its creation. For a collector run during a sync round that starts at `t0`
and ends before `t0 + sync_timeout` to reclaim a pack the round reads, the
checkpoint that defines its `C_gc` must have been created before `t0 + sync_timeout
− gc_grace`, so it was already `gc_grace − sync_timeout` old at `t0` (up to the
rate drift covered by `gc_margin`), so its sequence is `<= C_sync`. A catch-up
with watermark `>= C_sync` reads only packs `> C_sync`, a hydration reads the
newest checkpoint (whose sequence is `>= C_sync >= C_gc`) and the packs after it.
Configuration validation requires `gc_grace > sync_timeout + gc_margin`.

Consequence: under the invariant a sync never meets a reclaimed pack, so a
missing pack is corruption and stays an Internal error naming the pack (as in
phase 6). The restart on a missing covered pack of the first review is not added:
with these boundaries it would be unreachable code. (This refines B and C of the
PM review; confirmed by the PM.)

**Enforcement (binding).** The proof needs both bounds to be enforced, not
assumed. `sync_timeout` must bound **every** sync round and every hydration as a
context deadline that cancels the git processes and the bucket reads of the round;
WP76 audits the syncer's paths (the single-flight retry rounds, `fetch`,
`indexPack`, `applyBuild`, `install`), fixes any that can outlive the deadline, and
tests that a round slower than the bound is cancelled. Verify's dry hydration runs
under a deadline of `W` from its start, tested the same way.

### SHADOW BUDGET requirement (WP73b, binding; charter case B)

Found in WP73b: `TestStorageShadowModeBenchmarkHasNoDivergence` failed under
machine load with `select repository: context deadline exceeded`. Traced (pool
waits 0, SQLite lock waits under 50 ms; the failure reproduced byte for byte
with a 150 ms budget): Preparing's git work used up the shadow budget, and
Prepared failed at its first context-aware call, which was the `SELECT` of
`GetRepository`. The design behaved as specified; the test asserted more than
the design guarantees.

* **Old (implicit) requirement, asserted by the test:** in shadow mode every
  reference transaction is recorded in the database, under any load.
* **Requirement (RULE 4, stated):** shadow mode never fails or noticeably delays
  a git write: the storage-core work of one reference transaction, the Preparing
  and Prepared phases together, runs under `shadow_timeout`. A transaction whose
  work exceeds it is **not recorded**: the failure is counted in
  `graft_storage_shadow_errors_total` and logged at error level, and the git write
  proceeds. The database misses that write until the next write of the same
  reference, which is recorded as a forced changeset (counted in
  `graft_storage_shadow_divergences_total` and logged); meanwhile `graft storage
  verify` reports the reference as diverging. Recording every write is
  guaranteed only while the shadow work of every transaction fits the budget.
* **Operator rule:** switch to authoritative mode only with
  `graft_storage_shadow_errors_total`, `graft_storage_shadow_divergences_total`
  and `graft_storage_untracked_writes_total` at zero and `graft storage verify
  --connectivity` consistent on every repository.
* **Tests:** the mirroring test runs with a non-binding budget, because it tests
  the correctness of the mirroring and whether a load fits 5 s depends on the
  machine; `TestStorageShadowModeBudgetOverrun` tests the budget path
  deterministically, for an overrun in Preparing and one in Prepared (git write
  succeeds, error logged, nothing recorded, verify reports the divergence, the next
  write is forced and logged), and the voter's unit tests check the counter.

### SQLite write admission (WP73b, binding)

Found in WP73b: `TestCommitsOnOneReferenceAreDense/sqlite` (32 writers on one
reference) failed under load with `database is locked`. Traced on two cores: the
write lock is busy all the time (summed holds at least the wall time; mean hold
9 ms), but SQLite's busy handler keeps no queue, so the lock goes to whichever
waiter retries first: mean wait 0.16 s, longest 3.05 s where a fair queue bounds
it near 0.3 s, and under heavier load the tail crossed the 10 s busy timeout.
Postgres queues its row-lock waiters and is not affected.

* Every SQLite `Store` admits its write transactions one at a time, first come
  first served (`sqlstore.writeQueue`), before `BEGIN IMMEDIATE`; the admission is
  handed directly to the next waiter, so no newcomer overtakes it, and a writer
  whose context ends leaves the queue at once.
* **One admission covers one attempt** of `withTx`: it is taken right before
  `BEGIN IMMEDIATE` and released when that attempt committed or rolled back. A
  writer never holds it while `retryTx` sleeps before its next attempt, so the
  writers behind it never wait for a lock nobody uses. No transaction function
  starts another write transaction, and the admission is always taken before
  SQLite's lock, so the two cannot wait for each other (LOCK ORDER).
* Read transactions (the query-only pool) and plain reads need no admission.
* **Limit:** the fairness is per process. Several processes on one SQLite file
  (several nodes on one host) still meet in SQLite's busy handler; a multi-node
  deployment uses Postgres, SQLite is for one node or for tests (STORAGE.md).
* **Tests:** writers are admitted in arrival order; a writer whose context ends
  leaves the queue and writes nothing; the hand-over never loses the admission when
  it races a cancellation; the dense test and the LOCK ORDER `-race` test; and the
  dense test's wait distribution before and after, measured with the WP73b tracing
  under the same pinning.

### Liveness is per key (change G)

Liveness is decided per key from the rows that reference it: a key is live when
**any** changeset with `seq > C_gc` or any checkpoint with `seq >= C_gc`
references it. The snapshot therefore keeps, per key, the **maximum** referencing
sequence. With owned keys every key has one row, but legacy v0.1.0 keys can be
shared by several changesets, and the maximum makes them correct.

### What happens to unreachable objects (binding decision)

The checkpoint pack holds exactly the objects reachable from:
* the object-ID targets of the references at `N`;
* the `head_commit_id` and `base_commit_id` of every review that is **open** when
  the checkpoint is built (answer 2 of the review).

Objects reachable only from earlier states (commits orphaned by a force-update,
deleted branches or tags, rebased-away lineages) and not from an open review are
not carried into the checkpoint pack; once the changeset packs that introduced
them are reclaimed, they are gone from object storage, as `git gc` prunes
unreachable objects. Hydration and catch-up only reconstruct states at or after a
checkpoint, verification checks reachability from the current references, and
the changeset log keeps its metadata. **Documented limit:** the diff of a closed or
merged review whose commits are reachable from no reference at a checkpoint may
no longer be available.

### REVIEW ROOTS RULE (binding; WP74b, approved by the PM)

**Root cause.** Open reviews are a second kind of root into the object graph,
next to the references, and phase 6 did not count them. Every mechanism that
decides liveness respects references only:
* local Gitaly housekeeping keeps what local references reach;
* the voter packs what `--not --all` leaves out;
* a checkpoint's closure is the references at `N`;
* GC keeps what the boundary computed from them keeps.

So an open review's commit whose branch was deleted was garbage to all of them.
Tested: real housekeeping (a full repack with an expired cruft pack and a
prune, expiry now) removed it on today's code, on both dialects
(`TestOpenReviewCommitsSurviveHousekeeping`, run before the change).

**Rule: review roots are references.** The head and base commits of a review
are protected by `refs/keep-around/review/<oid>` (target `<oid>`), written through
the ordinary write path: the reference-transaction hook, so the voter and the
database CAS in shadow and authoritative mode. Then:
* every node keeps them through housekeeping (they are local references after
  catch-up or hydration);
* the voter uploads a commit that only the serving node had;
* a checkpoint's closure covers them by the ordinary "references at `N`" rule;
* GC treats their packs like any reference's.

`refs/keep-around/` is a hidden internal prefix in Gitaly: it is neither
advertised nor writable over the git protocols
(`uploadpack.hideRefs`/`receive.hideRefs`). The Graft changelog does not
record it, because it records only branches and tags. The review roots have a
namespace of their own, `refs/keep-around/review/`, because GitLab writes
`refs/keep-around/<oid>` for its own purposes (pipelines, merge requests). No
sweep and no database check ever touches a reference outside that namespace
(tested: a GitLab keep-around reference of an open review's commit is neither
guarded nor swept).

* **Writes (review service, every mode).** `CreateReview` writes the references
  of head and base (`reviewroots.Protect`: one reference transaction that
  creates the absent ones), then records the review. `UpdateReview` writes the
  references of the head and base it stores, then updates the review; this
  also holds when the head does not move, and it backfills reviews written
  before this rule. Comments and verdicts write no reference.
* **Invariant (authoritative mode, enforced in the database).** Every open
  review written with its roots required has the keep-around references of its
  head and base in `graft_refs`. Two checks, both under the repository row lock
  (LOCK ORDER):
  * `InsertReview` and `UpdateReview` with `requireRoots` (the review store of
    `CreateReview` and `UpdateReview`, `dbreview.WithRequiredRoots`) require the
    references to exist with their commit as value, else `ErrReviewRootsMissing`
    and nothing is written;
  * `CommitChangeset` refuses a non-forced changeset that deletes or moves a
    keep-around reference whose value is the head or base of an open review
    (`ErrReviewRootInUse`, `FailedPrecondition` from the voter). This covers
    every write path: `DeleteRefs`, `WriteRef`, `UpdateReferences`, pushes,
    and the sweep.
  A review write that loses its reference to a sweep between the two steps
  fails in the database. The service then writes the reference again, at most
  3 attempts in all, then `Aborted`. This is correct because of the invariant;
  the bound only stops a pathological loop.
* **Deletion.**
  * `CloseReview` deletes the references of the closed review's commits that no
    other open review uses (`reviewroots.DefaultSweeper.Sweep` with those
    candidates, trigger `close`). Each deletion is its own transaction expecting
    the value read. A deletion that a concurrent review write makes the database
    refuse is kept. A deletion that fails otherwise is not the request's error,
    because the close is already recorded: it is logged at warning level and
    counted (`graft_review_roots_sweep_failures_total{trigger="close"}`), and
    the backstop below deletes the reference. Not returning it is a design
    decision with a backstop, not a swallowed error.
  * **Backstop, every mode.** Before Gitaly's daily housekeeping of a repository
    (`DailyOptimizationWorker`, on by default, the same job that prunes
    unreferenced objects), `reviewroots.Sweeper.Housekeep` runs the backfill
    and then the full sweep of the repository's review references. Before, not
    after (integrated review m3): the prune takes unreferenced objects older
    than its grace, so a review commit left without its reference would be
    taken in the same run that was about to protect it; a repository whose
    backfill fails is not optimized that day, and the error is the job's. The
    backfill writes the references that open reviews lack: reviews written
    before this rule, or imported from shadow-mode files. The full sweep
    deletes the references no open review uses: leftovers of a failed close
    sweep or of a crash. In authoritative mode it first catches the repository
    up and uses the database's reviews; it skips an untracked repository or an
    incomplete import. In off and shadow mode it uses the review files. Metrics
    (`reviewroots.DefaultSweeper`, registered by `gitaly serve`):
    `graft_review_roots_deleted_total`, `graft_review_roots_backfilled_total`,
    `graft_review_roots_missing_total`, and
    `graft_review_roots_sweep_failures_total{trigger}`, so a persistent failure
    shows as a growing counter. Lost commits are logged with their review ID
    and commit. Limit: an operator who disables the daily maintenance also
    disables this backstop (and the scheduled object pruning it guards).
  * **Precondition of a checkpoint, and so of garbage collection (integrated
    review m3).** In authoritative mode the commits of a review are kept in the
    storage core by the checkpoint closure, which is the references. A review
    without its keep-around references whose branch is gone has its commits
    only in changeset packs, and garbage collection deletes the packs a
    checkpoint covers. So `checkpoint.Create` runs `Housekeep` first (step 0),
    through the write path, before it reads the reference snapshot; a build
    whose backfill fails records no checkpoint (`Unavailable`), and without a
    checkpoint garbage collection deletes no changeset pack. A commit already
    missing from the repository is logged; the checkpoint cannot hold it.
    Test: `TestStorageCheckpointBackfillsReviewRoots` (e2e; a mutant without
    step 0 fails it on both dialects).
  * **Bound (integrated review m2).** The references are written in every
    mode, so an unbounded number of open reviews would be an unbounded number
    of references and kept objects, with the storage core off too.
    `[graft.review] max_open_reviews` (default 1000) bounds the open reviews of
    a repository. `CreateReview` checks it before it writes any reference
    (`ResourceExhausted`, nothing written, no reference transaction); the store
    checks it again atomically with the record (`store.Store` under its file
    lock, `InsertReview` under the repository row lock with
    `ReviewInsert.MaxOpen`), so concurrent creates never pass it. A create that
    passed the first check and lost the second has written its references:
    the service releases them as a close does (sweep of those commits unless
    another open review uses them; the backstop otherwise). Imports are not
    bounded. Tests: `TestCreateIsBoundedByTheOpenReviews` (both stores, twelve
    concurrent creates from two handles), `TestCreateReviewIsBoundedByMaxOpenReviews`,
    `TestCreateReviewPastTheLimitReleasesItsRoots`; mutants without either
    check fail them.
  * A sweep may skip references younger than `gc_grace` to make the
    creation/sweep race rare. That is an efficiency measure only; correctness
    comes from the invariant and the bounded retry.
* **A review whose commit is lost (decision).** A review written before this rule
  whose head or base commit is gone from the repository cannot be kept any
  more. `UpdateReview` refuses it with `FailedPrecondition` naming the review
  (metadata `review_id`) and the commit (`commit_id`). The base is protected
  before the descent check, so the error is never a misleading "head does not
  descend". The backfill reports the same pair (`MissingRoot{ReviewID,
  Commit}`). The review can still be read and closed.
  Why not fetch the object from the storage core as hydration does: nothing
  kept it live there either. Only references keep objects live, and this commit
  had none, so after GC no pack holds it. Before GC some changeset pack may
  still hold it, but no index maps an object to its pack, so finding it means
  downloading packs until it turns up. That would be a fallback that works only
  until the next collection (RULE 24 forbids such paths). The state is known
  and documented, so a precise `FailedPrecondition` is the answer.
* **Off and shadow mode.** The review store is the files store and the same
  references are written, so housekeeping keeps the commits there too; in
  shadow mode the voter records them in the database. The database invariant
  is not enforced there: the files are the source of truth, and shadow
  changesets are forced, so `CommitChangeset` does not refuse them. The close
  sweep and the housekeeping backstop run there as in authoritative mode.
* **What it costs.** One reference transaction per review creation and per
  head move (one changeset, plus a pack only when the commit was not yet in the
  storage core), one per released commit at a close, and no changelog entry.
  Measured in WP79.
* **What it replaced (WP74 → WP74b).** The first design checked, at each review
  write, that the commits were reachable from a reference snapshot, and recorded
  a root set with each checkpoint. Because the commits were not references, it
  could not protect them on the nodes, and a review whose commits were lost
  blocked every checkpoint. It is removed with its errors, metric, tests and
  `gitops.IsReachable`. `CheckpointInput` has no root set, and the checkpoint
  build reads no reviews.
* **LOCK ORDER (PM check 3; self-contained, as of the integrated review).**
  *What it guarantees.* The write transactions of the storage core on one
  repository never deadlock with each other, except the one case named below,
  which Postgres detects and the store retries. The rule also gives the
  serialisation that the invariants need: a changeset, a review write that
  requires its roots, and the conditions checked with them (the review roots,
  `max_open_reviews`) see each other in a total order.

  *Background.* The database has one row per repository (`graft_repositories`,
  "the row") and child tables keyed by `repo_id` (references, changesets,
  changelog, reviews, nodes, checkpoints and their references, orphan
  observations, leases), all with `ON DELETE CASCADE` foreign keys. SQLite has
  one write lock for the whole database, taken at `BEGIN IMMEDIATE`: its write
  transactions run one at a time and cannot deadlock. The rest is about
  Postgres, where locks are per row. There, an insert of a child row takes
  `FOR KEY SHARE` on the parent row (the foreign key check) until the
  transaction ends; `FOR NO KEY UPDATE` does not conflict with `FOR KEY SHARE`;
  a `DELETE` of the row takes `FOR UPDATE`, which does.

  *The rule.*
  1. **Row-first transactions.** A transaction that must serialise with the
     writers of the repository locks the row **as its first statement**, before
     it reads or writes a child table: `SELECT ... FOR NO KEY UPDATE` (Postgres;
     on SQLite the write lock is already held). These are `CommitChangeset`,
     `RegisterRepository` (it inserts the row), `ImportHistory`, `InsertReview`
     (always; it allocates the review ID, checks the roots when required and
     counts the open reviews for `max_open_reviews`), `UpdateReview` with
     `requireRoots`, the three lease methods, and `DeleteRepository` (which
     then deletes the row: the only upgrade, to `FOR UPDATE`, of a lock it
     already holds; the trigger `graft_repositories_tombstone` inserts the
     tombstone, a table without a foreign key).
  2. **Child-only transactions.** Every other write transaction never locks the
     row explicitly and never changes a `repo_id`:
     * insert-only: `CreateCheckpoint` (since M4, below), `RecordOrphans`,
       `ReportApplied` (an upsert), and the reconciliation's `RecordLeak`
       (`graft_gc_removed` has no foreign key);
     * update or delete of child rows only: `UpdateReview` without
       `requireRoots` (comments, verdicts, closes; one row, with a version
       check), `ForgetOrphans`, `DeleteCheckpoint` (one checkpoint row, whose
       references cascade), `DeleteRemovedRepository`;
     * tables of their own: `ObserveAllocator` and `AdvanceReconcile`
       (`graft_gc_allocator_marks`, the one row of `graft_gc_reconcile`).

  *Why no cycle.* A cycle needs every transaction in it to hold a lock while it
  waits for the next. Take the waits one by one:
  * A row-first transaction waits for the row before it holds anything of the
    repository, so that wait closes no cycle. Holding the row, it can wait only
    for a child row that another transaction holds.
  * The row-first transactions that hold the row run one at a time, so the
    child row it waits for is held by a child-only transaction.
  * A child-only transaction waits on the row only at its first insert (the
    key share), while it holds nothing yet: no cycle through that wait. Its
    other waits are for child rows.
  * The child-only transactions that hold one child row and wait for another
    are the multi-row ones, and each kind touches rows nobody else holds at the
    same time: `ForgetOrphans` and `RecordOrphans` run in a collection, which
    holds the repository's maintenance lease, so one at a time per repository;
    `CreateCheckpoint` inserts rows of its own sequence (a concurrent build of
    the same sequence waits on the primary key and then does nothing).
  * The single-row ones (`UpdateReview` without roots, `DeleteCheckpoint`'s
    checkpoint row, `ReportApplied`) wait for nothing once they hold their row.
  So a chain of waits never comes back to its first transaction, except
  through the cascade of a repository removal, below.

  *The exception: removing a repository.* `DeleteRepository` holds the row
  `FOR UPDATE` and its cascade deletes child rows in the database's order. A
  child-only transaction that deletes several child rows in its own order
  (`ForgetOrphans`) can then hold one row the cascade needs while waiting for
  another the cascade holds. Postgres detects the cycle after
  `deadlock_timeout` and aborts one transaction with SQLSTATE 40P01, which the
  store retries like a serialisation failure (`isRetryable`, `withTx`): the
  outcome is that of a serial order (the repository is gone, or the
  observations were forgotten first). This costs a second at worst, only while
  a repository is removed during its collection, and changes no result.

  *Changed by the integrated review (M4).* `CreateCheckpoint` used to be
  row-first, and inserted its snapshot one statement per reference: at 100k
  references on Postgres a writer of the repository waited 38–46 s, locally.
  It now reads the row without locking it and writes the snapshot 1000
  references per statement. Nothing in it needs the writers excluded:
  * the head only grows, so a sequence at or below the head read stays so;
  * the primary key makes one checkpoint per sequence: a concurrent build of
    the same sequence inserts nothing (`ON CONFLICT DO NOTHING`) and returns the
    one that committed, and a newer checkpoint covers the request and is
    returned too (m7);
  * a checkpoint below a newer one is never the newest, so no seed reads it;
  * a concurrent removal of the repository either waits for it (its key share)
    or makes its first insert fail on the foreign key: nothing is recorded,
    and its pack is an orphan of a removed repository, collected with it;
  * the pack's write deadline is checked in the same transaction on the
    database clock (`PackDeadline`, m6), as for a changeset.
  Measured at 100k references (`TestCheckpointOfManyReferences`): on Postgres
  the record takes 1.6–1.7 s while about 500 changesets commit, the longest in
  15–20 ms; on SQLite 0.23–0.27 s (before: 0.63–0.70 s), during which writers
  wait for SQLite's one write lock.

  *Tests.* `TestLockOrderConcurrentWriters`, with `-race` on Postgres and
  SQLite: concurrent lands, review creations and head moves (requiring their
  roots), a sweeper that deletes keep-around references until the writers are
  done, comments, checkpoint commits and leases on one repository. It checks
  that no deadlock surfaces (the retry hook counts them, and the count must stay
  at zero), the invariant at the end, and dense sequences.
  `TestConcurrentCheckpointsOfOneSequence` (six concurrent records of one
  sequence), `TestCheckpointOfManyReferences` (writers during a large record),
  `TestCreateIsBoundedByTheOpenReviews` (concurrent creates against the
  limit, two handles).
* **Files.** `internal/graft/corestore/reviewroots/**` (Protect, Sweep,
  Backfill), `internal/gitaly/service/review/**` (the roots writes and the
  close release), `internal/graft/corestore/dbreview/**`
  (`WithRequiredRoots`), `sqlstore` (the two checks), `voter` (the error code).

**Tests (WP74b).**
* `gate`, on real servers in authoritative mode, SQLite and Postgres:
  * housekeeping keeps an open review's commit after its branch is deleted, on
    the serving node and on a node that hydrated after the review was created
    (the reproduction above, now green);
  * `DeleteRefs`, `WriteRef` and `UpdateReferences` cannot delete or move an
    open review's keep-around reference, and nothing changes locally or in
    the database;
  * an unused keep-around reference can be deleted.
* `sqlstore`, both dialects: the two checks, writing nothing when they refuse;
  a forced (shadow) changeset is not refused; closing a review releases its
  references; the LOCK ORDER race above.
* `review`:
  * a review of a commit that only the serving node has records its
    references in the database;
  * a head move adds one;
  * a close releases only what no other open review uses;
  * a sweep between the reference write and the review write is survived
    after one retry, and exhaustion is `Aborted` with nothing recorded;
  * off and shadow mode write the references, and housekeeping keeps the
    commit.
* `reviewroots`: Protect (idempotent, fixes a wrong value, refuses an absent
  object), Sweep (candidates, everything, kept versus deleted), Backfill
  (counts and missing review IDs, including invalid object IDs).
* Mutation checks:
  * disabling the deletion guard fails the gate test and the sqlstore test on
    both dialects, and the LOCK ORDER invariant on Postgres;
  * disabling the roots requirement fails the sqlstore test and both review
    race tests.

### LOCAL REFERENCE WRITES RULE (binding; WP76b, approved by the PM)

**Problem (measured).** A reftable repository has one lock for all its references
(`tables.list.lock`). In authoritative mode `UserCommitFiles` spent most of its time
waiting for it: with SQLite and a file bucket, at 50 and 100 agents, `u.prepare`
(the wait) was 643 and 1198 ms mean, `u.commit` 252 and 230 ms, the voter's Prepared
phase 19–26 ms. With `core.fsync` without `reference`, `u.commit` fell to 17 and 7 ms
and the wait to 248 and 238 ms. So the hold is git's fsync of the new table and of
`tables.list`, inside the lock. The wait that is left, about 6 holds, is git's lock
wait: a polling sleep that grows with each failed try (`reftable.lockTimeout` =
10 s). There is no queue, so newcomers win over long waiters and the lock sits free
between polls. The same unfairness starved the syncer's catch-up behind writers
(the flake of `TestSyncerConcurrentLocalWritesAreNeverReverted`).

**Root cause.** In authoritative mode the durable commit point of a reference
update is the database commit in the voter's Prepared phase; the local repository
is a cache that any node rebuilds from the database and the bucket. Each
transaction then made the same update durable a second time, inside the
repository-wide lock.

**Rule.**
1. *Admission* (`corestore/localrefs.Admission`). The reference writers of a node
   wait in a FIFO queue per local repository in front of git's lock:
   * the hook's transactions from the end of the voter's Preparing phase (after
     the pack upload, before git takes the lock) to Committed or Aborted;
   * the syncer around each catch-up `update-ref`.

   Admission orders writers only; git's lock stays the only mutual exclusion. A
   writer that waited `reftable.lockTimeout` goes on to git's lock as before
   (`bypassed`). A hook's admission belongs to the git process of the
   transaction (`gitcmd.BeginInvocation`, by its invocation): git reports neither
   Committed nor Aborted for a prepare that fails in `update-ref`, which dies, in
   an atomic push, or for a killed process. So the process' end, or its next
   transaction, releases the admission whatever git reported
   (`released_unreported`). An admission not released within the same bound
   ends (`expired`; a live process between phases). A writer whose context ends
   or whose process ended leaves the queue. Lock order: the syncer's repository lock, then the admission. No
   admitted writer waits for the syncer lock: `NoteCommitted` only tries it, and
   runs after the release.
2. *Deferred durability* (`corestore/localrefs.Durability`). The hook's
   `UpdateWithHooks` transactions and the syncer's catch-up write references with
   `core.fsync` without `reference`. Only the voter can turn this on
   (`DeferredReferenceDurability`: authoritative mode, the kernel's boot identity
   readable, and this process owns the storage's deferred durability). Objects
   stay durable as before.
   * *One owner per storage.* The deferred durability of a storage belongs to the
     process that holds an exclusive lock (flock) on
     `<storage>/+gitaly/graft-ref-durability/owner.lock` for its whole life.
     * Only the owner writes references without fsync, runs the durable steps,
       recovers, and removes a durable step's leftovers. A second process on the
       storage (a graceful upgrade overlaps the old and the new process) writes
       references with fsync and touches no marker or lock. It tries every second
       to take the ownership, and recovers when it does.
     * Ownership only goes from false to true within a process. So a write
       decided as deferred always runs in the owner.
     * Why this is safe:
       * The unfsynced writes, the live transactions and the syncer lock that
         the durable step reasons about all belong to one process.
       * A non-owner's writes are durable by themselves. Its compactions fsync
         the new table and list before they delete the tables they replace.
       * A new owner takes over only after the old process exited (the flock is
         released at exit), so no durable step of the old process is still
         running.
       * While an old process lives, no foreign-boot marker can exist: that
         process recovered at its own start in this boot.
     * Measured with real binaries: a second `gitaly serve` during the first's
       life, then the first killed.
   * *Dirty marker.* Before the first such write after a durable step, the
     repository gets a marker, written durably, with the kernel's boot identity
     (`<storage>/+gitaly/graft-ref-durability/<sha256(path)>.json`, not the state
     directory, whose files the cache walker removes).
     * The hook path writes it in Prepared, after the database commit and before
       git writes anything, so only repositories the database tracks get one. If
       the marker cannot be written, Prepared fails and git aborts; catch-up
       repairs the database's write, like any local failure.
     * The syncer writes it under its repository lock.
     * The durable step never clears the marker while a git process of the owner
       that writes references without fsync is alive.
       * Only `UpdateWithHooks`' update-ref writes that way. It is counted from
         just before it starts (`DeferReferenceWrites`, which also decides the
         deferral) until after it exited (`updater.Close` waits for it).
       * The syncer's update-ref runs under the syncer lock, which the durable
         step holds too.
       * The count follows the process, not the hooks. git skips the Aborted and
         Committed hooks in several cases: a failed prepare (a stale old value,
         a busy lock, a name conflict), a failed finish, initial transactions, a
         kill. Its Committed hook names a symbolic reference's target instead of
         the reference. Neither can leak a count or end the wrong one.
       * Reasons from git's source: the new table is written in
         `transaction_finish` after "prepared", and auto-compaction runs inside
         `reftable_addition_commit`, before "committed".
   * *Durable step* (`Settle`, every second for every dirty repository, under the
     syncer's repository lock).
     * It fsyncs the tables not yet fsynced, `tables.list` and the directory.
     * When the repository has been idle for a second, or the process stops,
       and nothing is live, the step clears the marker:
       * It fsyncs the watermark and its directory (see *Watermark* below),
         before git's lock, so that this fsync does not hold the transactions.
       * It takes git's lock without waiting. The lock is created by linking a
         temporary file that holds a tag and a random nonce.
       * It fsyncs what was written since, marks the state clean, and releases
         the lock. The release removes the lock only if it is still the same
         file with the same nonce: an inode is reused as soon as it is freed, so
         a lock that someone else replaced is never removed.
       * Last, it removes the marker, under a per-repository file lock that
         `MarkDirty` waits for.
     * A busy lock leaves the marker for the next step.
     * The set of fsynced tables keeps only the tables the stack still lists.
     * A graceful stop runs a last step for every dirty repository after the
       servers stopped, so a clean reboot does not rehydrate them.
   * *Watermark.* While a repository of the owner is dirty and has a reftable
     stack, the syncer writes its watermark (`graft/storage/applied`) atomically
     without fsync (`DefersWatermark`).
     * After a kernel restart a dirty cache is moved aside whatever its watermark
       holds, and after a process restart the page cache kept the write. An
       advance only changes the sequence; the repository ID was written with
       fsync by the hydration, so a lost or torn advance leaves the recovery's
       decision as it was.
     * Every removal of a dirty marker fsyncs the watermark and its directory
       first, if there is one: the step that clears it (before it takes git's
       lock), the step of a repository whose stack is gone, and `Forget`. A
       failed fsync makes the repository untrusted, as for the stack.
     * The watermark cannot change between that fsync and the removal: the
       syncer writes it only under its repository lock, which the step and
       `Forget` hold.
     * Hydration, import and the gate write it with fsync, as does a write
       whose directory is missing.
   * *Write failure.* Only a failed fsync means the kernel may have dropped
     writes, and the kernel reports a failed writeback once: a later fsync of the
     same file succeeds.
     * So the first failed fsync makes the repository *untrusted* for good. The
       marker is rewritten durably with that failure, so a restart of the
       process in the same boot keeps it untrusted too.
     * An untrusted repository's marker is never cleared; only the move aside is
       retried, until it succeeds. Then the hydrated repository starts clean.
     * If even rewriting the marker fails, the disk is failing; the state of the
       process still keeps the repository untrusted until it stops.
     * A repository moved aside (`Forget`) while a git process of the owner
       that writes without fsync still runs stays dirty with a plain marker
       until that process exited and a durable step settled it. git names the
       repository by its path, so the process's later writes land in what is
       there now, the hydrated repository say.
     * A copy that holds shadow-mode history not imported yet (the
       `checkReplaceable` guard of a sync's hydration) is moved into
       `<storage>/+gitaly/graft-untrusted/` instead of being deleted: it is
       never served, and the history is not lost.
     * The repository is moved aside at once, under the repository lock: the
       move (both directories and the new entry) is made durable, then the
       marker removed.
     * This happens only when the repository is a cache of the storage core:
       the database holds it with changesets, as for the recovery. The failure is logged and
     counted (`sync_failed`), and the next request hydrates the repository. Any
     other error (opening a file, reading the list) is transient: counted
     (`graft_storage_ref_durable_step_errors_total`) and retried at the next
     step.
   * *Recovery* (`Syncer.RecoverLocalReferences`). `gitaly serve` runs it before
     any listener opens and before any background worker, when it owns the
     storage (`Syncer.StartLocalReferences`); otherwise it runs when the process
     takes the ownership.
     * A marker of another boot means the kernel restarted with writes that may
       not have reached the disk. git reads a torn stack as an empty repository
       with exit code 0 (experiment 1, kept as
       `TestTornReftableStackReadsAsAnEmptyRepository`). So the repository is
       moved aside before anything reads it (`reboot`), and the next request
       hydrates it.
     * The move only happens when the database holds the repository with
       changesets. A sync would replace such a local copy too when it is stale,
       whatever its watermark. Any other marker is dropped, keeping a repository
       whose local copy may be the only one (an import that is not complete).
     * A marker of the same boot is a process restart: the page cache kept
       every write, so the repository stays dirty to be settled.
       * A git process of the previous owner that outlived it (killed with it,
         not reaped yet) may still be committing. So the marker stays while the
         repository's reftable directory holds a git lock or a table being
         written, which a writing git process always has: `tables.list.lock`
         (prepare to commit), `<table>.lock` (the tables a compaction merges),
         `*.temp.*` (a table being written). On top of that there is a floor
         of 2 s (`RecoveryGrace`) from the recovery and from the last git
         activity seen, for the instant between a commit's rename and the
         start of its auto-compaction.
       * A marker that records an untrusted repository is handled like a
         marker of another boot: the repository is moved aside if it is a
         cache; otherwise its marker is never settled, and it is not served.
       * Why the grace bound holds: when the previous owner dies, the stdin
         of its `update-ref --stdin` children closes.
         * A child that has not been sent `commit` aborts at end of input and
           writes nothing.
         * A child that has been sent `commit` already holds the reference lock
           (taken in `prepare`). It only writes its table and list and
           compacts, which takes milliseconds.
         * A child still waiting for the lock writes nothing afterwards: the
           next thing it reads is the end of its input.
         * A child that is committing is not bounded in time. Hence the rule
           on git's lock and temporary files above, and only a short floor.
       * No parent-death signal (`Pdeathsig`) instead, deliberately (PM).
         Go's `Pdeathsig` fires when the OS thread that created the child exits,
         not the process, unless the spawn is pinned with `LockOSThread`. In the
         shared command package that is easy to get subtly wrong.
     * The ownership is taken in two steps. The locks come first; the recovery
       runs; only then are the process's writes deferred (`Activate`). So no
       writer of the new owner writes a marker that the recovery could take for
       a leftover.
     * A lock with the durable step's tag and the step's temporary files, left
       by a process that died, are removed.
     * A corrupt marker fails the startup: its repository is unknown.
   * Not Linux (no boot identity): references stay durable per transaction.
3. *Read paths.* After the recovery, no read path in authoritative mode can see
   a stack torn by a kernel restart: the gRPC listeners (unix, TCP, TLS), the
   Prometheus listener, the maintenance workers, the bundle generation, the
   tempdir cleaner and the repository counter all start after it. Within the
   same boot the stack is never torn (the page cache has every write). A failed
   writeback is caught by the durable step within a second and the repository is
   moved aside. Outside `gitaly serve`, only `gitaly-debug simulate-http-clone`,
   `gitaly-debug dump-mvcc-manifest` and the `gitaly git` passthrough, pointed
   at a repository by an operator, read references from disk. STORAGE.md says
   not to run them on a node after an unclean shutdown before the server has
   started once.

**As implemented.**
* `localrefs` (`admission.go`, `durability.go`); `Core.RefAdmission`/`RefDurability`.
* `voter` (`admit`, `deferDurability`, Committed/Aborted).
* `syncer` (`refWriter`, `durability.go`).
* `serve.go`: `openLocalReferenceWrites`; `StartLocalReferences` before the
  listeners (ownership, recovery, durable-step loop), and its stop, deferred so
  that it runs after the servers stopped.
* PM grant: `updateref.WithDeferredReferenceDurability`; the `gitcmd` constants
  `FsyncComponents`, `FsyncComponentsWithoutReferences` and
  `ReftableLockTimeoutMilliseconds`; `hook/storage_voter.go`
  (`DeferReferenceWrites`); the optional interface asserted in
  `UpdateWithHooks`.
* Follow-up after the independent review of 310facd:
  * MAJOR: process-local state with overlapping processes. Fixed by one owner
    per storage.
  * The unlock is ownership-checked (nonce and file).
  * An abort ends its transaction (the live count).
  * The fsynced-table set is pruned.
  * A final step runs on a graceful stop.
  * Only a failed fsync distrusts a repository, and only after the cache check.
  * The directory fsyncs (the marker directory's parent; the stale and temporary
    directories of a move) are added.
  * The step's temporary files are cleaned at recovery.
* Second review (of 8953e8e):
  * M1: the live count is tied to the deferred git process itself; it no
    longer depends on hooks git may skip. `StorageVoter.Denied` is gone.
  * M2: untrusted is sticky.
  * The recovery runs before the writes are deferred (`TryOwn`, `Recover`,
    `Activate`).
  * The recovery grace for recovered markers.
  * The dirty gauge counts a repository once.
  * Tests:
    * the reused inode is simulated deterministically;
    * a non-owner neither recovers nor removes the live owner's lock;
    * the e2e upgrade test reads the owner and marker counters instead of the
      racing marker file;
    * a non-owner's write marks nothing, at unit, syncer and e2e level;
    * git skipping its hooks, with the race, the failed prepare and the
      symbolic split;
    * a failed fsync then a successful one; and the same with a failed move
      aside.
* Third review (of 18e152a):
  * M-A: the untrusted state is in the marker and survives a process restart.
  * m-1: Forget keeps a repository with a live git process dirty.
  * m-2: recovered markers wait for git's lock and temporary files, with a 2 s
    floor.
  * m-3: the cache check applies the sync's shadow-history guard; such copies
    are kept, not deleted.
  * m-4: a test proves the end of a deferred process runs after it exited. The
    mutation that swaps the two defers fails it.
  * Clean states are dropped; MarkDirty takes its state from the map again.
* Fourth review (of 32d0ac4 and 03ef6c2). Four reviews in a row found marker
  defects in different branches (Settle, Forget, Recover). The root cause:
  "a marker may go only when that is safe" was re-implemented branch by
  branch. Fixed one level up:
  * One function, `dropMarker`, removes markers and drops states. Every path
    goes through it: Settle, the missing-path case, Forget, recovered markers,
    pruning. It checks the whole condition under the state's file lock (which
    MarkDirty waits for) and `d.mu` (which Begin takes):
    * no live deferred git process (`nLive`);
    * not untrusted;
    * an existing repository only under git's reference lock, after its stack
      was made durable under that lock. A missing repository has no stack, and
      the syncer's repository lock keeps it from being hydrated meanwhile;
    * no git activity in the stack (stack.c's lock and temporary names). Seeing
      some pushes the 2 s floor forward.
  * Finding 1 (MAJOR): the missing-path branch no longer removes a marker that
    a live process needs (moved aside, then hydrated, then the orphan write).
  * Finding 3: a recovered marker of a missing repository is settled. No
    reftable directory means no activity.
  * Finding 5: a state leaves the map only in `dropMarker`, in the same `d.mu`
    section that checks `nLive`, so a Begin is never lost.
  * Finding 6: the floor counts from the last activity seen.
  * Forget rewrites a marker that is not a plain marker of the running boot.
    The model test found this one: a marker of another boot, kept for a live
    process, made the next restart distrust the hydrated copy.
  * A marker of another boot makes the state untrusted (`errReboot`), so
    nothing settles it before the move.
  * Untrusted means never served: `EnsureFresh`, which the gate runs for every
    repository RPC, answers `Unavailable` for an untrusted path. A repository
    that is not a cache stays in place, unserved, for an operator.
  * Finding 2:
    * `+gitaly` is reserved (`corestore.CheckRelativePath`), in the gate for
      every repository RPC and in every storage core RPC;
    * STORAGE.md has a procedure for importing a kept copy;
    * metrics `graft_storage_kept_untrusted_repositories` and `_bytes`.
  * Finding 4: a local history file that cannot be read means keep (moved to
    `graft-untrusted`), never a failed startup. Database errors still fail the
    recovery and are retried.
  * Finding 7: the downgrade is documented (STORAGE.md, Failures).
  * Finding 8: `StorageCoreService` is never allowed for per-agent tokens.
  * Finding 9: a flag that a storage subcommand does not read is a usage error.
  * Finding 10: CollectGarbage holds its lease with the runner's renewal
    (`maintenance.HoldLease`). A lease lost during the collection is `Aborted`.
  * `TestMarkerLifecycleModel`: 40 seeds × 250 random steps.
    * The steps: MarkDirty; Begin and end; table writes without fsync; Settle,
      final or not; failed fsyncs; move aside and Forget; hydration; a process
      restart with git processes of the previous owner in the middle of a
      write; a kernel restart; acceptance in place.
    * After every step it asserts that a write that may not be durable means a
      marker on disk, and that untrusted equals reported untrusted, with the
      marker kept. The model learns durability from the fsyncs themselves.
    * Mutation check: removing any one condition fails the model or
      `TestDropMarkerGuards`. The conditions: live processes, git's lock, git
      activity, untrusted, the plain-marker rewrite, the missing-path removal,
      the recovered untrusted record, the reboot distrust.
    * Known limits of the model, each covered elsewhere:
      * the grace floor is 0 in the model, so time does not decide
        (`TestGitActivityPushesTheGraceForward`, and the recovery tests);
      * the syncer's own writer, which marks the repository under the
        repository lock, is not modelled (the syncer tests);
      * auto-compaction is not a separate step: it holds git's lock and
        temporary tables, which `gitActive` sees (`TestRecoveredMarkerWaitsForGit`);
      * the steps are sequential: what git's reference lock excludes is the
        `ownLock` guard (`TestDropMarkerGuards`).
* Fifth review (of 70d4a2b): no blocker, no major. Minors:
  * A repository without a reftable stack (gone, or a files-backend copy being
    imported) has nothing written without fsync (`hasStack`). Settle, Forget and
    `dropMarker` treat it so, and the recovery never fails on one
    (`TestRepositoryWithoutAStackHasNothingToMakeDurable`).
  * Lock and temporary files of a stack older than an hour, the grace of
    Gitaly's housekeeping (`ReftableLockfileGracePeriod`), are left by a git
    process that died. They are not activity, and the durable step removes such
    a list lock as housekeeping would, so a crash cannot pin a marker
    (`TestStaleGitLockDoesNotPinTheMarker`).
  * `ImportRepository` (passthrough) refuses an untrusted repository
    (`TestImportRefusesAnUntrustedRepository`). The other passthrough RPCs:
    * `VerifyRepository` and `CreateCheckpoint` run `EnsureFresh`, which
      refuses;
    * `GetStorageState`, `CollectGarbage` and `CollectRemovedRepositories`
      read no local references;
    * the hook RPCs run inside a write that the gate admitted.
  * STORAGE.md: "never served" means from the next request on.

**Tests.**
* `localrefs`:
  * admission: FIFO order, a cancelled waiter, the wait bound, the hold bound,
    unique keys;
  * ownership: one owner per storage; a non-owner neither defers, settles nor
    recovers; ownership taken after the owner closes; the marker directory
    created durably;
  * durable step: the marker; idle, live transactions (including two with the
    same key), git's lock held, the final step; the checked release (a
    replaced lock with a reused inode is kept); failed fsync versus a transient
    error; pruning; repository gone;
  * recovery: process restart, kernel restart, stale lock and temporary files,
    corrupt marker;
  * the torn-stack experiment.
* `voter`: committed, aborted (now settled) and rejected transactions; a denied
  and an allowed transaction with the same input; a marker that cannot be
  written; durability per mode (off, shadow and authoritative without boot
  identity keep the fsync).
* `updateref`: the effective `core.fsync` of `update-ref`.
* `hook/updateref`: only a voter that defers turns it off.
* `syncer`: catch-up marks; reboot recovery hydrates (a read before it lists
  nothing); process-restart recovery keeps; non-caches are kept; a failed fsync
  moves aside and hydrates; a transient error is retried; a second process waits
  for the owner, then owns and settles; the stop settles a repository that is
  not idle.
* e2e with the real `gitaly` binary:
  * SIGKILL after writes: same boot, kept, settled;
  * SIGKILL in the durable step: the lock is removed at restart and writes go
    on;
  * a simulated power loss (torn table, other boot): moved aside, every write
    served;
  * a graceful-upgrade overlap: a second `gitaly serve` on the same storage
    writes with fsync while the first owns; after a SIGKILL of the first, the
    second owns and defers;
  * stack depth under 4×30 concurrent writes: git's geometric compaction, at
    most 2⌈log₂ n⌉+2 tables; measured 4 for 122 updates.

**Measured** (2026-10-19, same method as the problem's numbers, alternating
before = b665f56 and after = WP76b, two runs each, load 3–7; mean ms, 50 / 100
agents):

| | before | after |
|---|---|---|
| `UserCommitFiles` | 914–990 / 1013–1954 | 363–432 / 447–450 |
| git lock wait (`u.prepare`) | 500–552 / 585–878 | 0.4 / 0.4 |
| git commit (`u.commit`) | 105–143 / 176–452 | 1–2 / 1 |
| lands/min | 731–972 / 677–817 | 1411–1486 / 1491–1673 |
| land p99 | 6.4–6.7 s / 8.7–17.2 s | 2.3–2.5 s / 4.1–4.3 s |

The wait that remains is the admission queue (mean 124–190 ms, inside the
Preparing phase): the held section is now the voter's Prepared phase (database
commit, about 15 ms) plus a 1–2 ms git write. Bypassed and expired admissions
were 0. During the load the repository was never idle for a second, so its
marker stayed while the stack was made durable every second.

### Data model (new migration, both dialects)

Migration `20261018000000_graft_storage_gc`, appended to `sqliteMigrations` and
`postgresMigrations` in `internal/graft/corestore/sqlstore/migrations.go`. A
v0.1.0 database gets it at `Open`; with no checkpoints, observations or tombstones
every phase-7 path degrades to phase-6 behaviour.

```sql
-- A checkpoint: the consolidated pack and (graft_checkpoint_refs) the reference
-- snapshot at seq.
CREATE TABLE graft_checkpoints (
    repo_id    <int> NOT NULL REFERENCES graft_repositories (repo_id) ON DELETE CASCADE,
    seq        <int> NOT NULL,           -- 1..head_seq
    pack_key   TEXT  NOT NULL,           -- an owned key
    pack_bytes <int> NOT NULL DEFAULT 0,
    created_at <ts>  NOT NULL,           -- database clock
    PRIMARY KEY (repo_id, seq)
);

-- The reference snapshot at a checkpoint (no absent values). target is a hex
-- object ID or "ref:<name>", as in graft_refs.
CREATE TABLE graft_checkpoint_refs (
    repo_id <int> NOT NULL,
    seq     <int> NOT NULL,
    ordinal <int> NOT NULL,
    name    BLOB  NOT NULL,              -- BYTEA on Postgres
    target  TEXT  NOT NULL,
    PRIMARY KEY (repo_id, seq, ordinal),
    FOREIGN KEY (repo_id, seq) REFERENCES graft_checkpoints (repo_id, seq) ON DELETE CASCADE
);

-- The collector's observations of unreferenced packs (PACK OWNERSHIP RULE 4).
-- Written only by the collector, never on the write path.
CREATE TABLE graft_gc_orphans (
    repo_id    <int> NOT NULL REFERENCES graft_repositories (repo_id) ON DELETE CASCADE,
    pack_key   TEXT  NOT NULL,
    first_seen <ts>  NOT NULL,           -- database clock
    PRIMARY KEY (repo_id, pack_key)
);

-- One maintenance lease per repository (exclusivity and efficiency; GC and
-- checkpoint correctness do not depend on it, see change E).
CREATE TABLE graft_gc_leases (
    repo_id     <int> NOT NULL PRIMARY KEY REFERENCES graft_repositories (repo_id) ON DELETE CASCADE,
    holder      TEXT  NOT NULL,          -- node_id
    acquired_at <ts>  NOT NULL,          -- database clock
    expires_at  <ts>  NOT NULL           -- database clock
);

-- A tombstone of a removed repository, written in the transaction that deletes
-- the repository row. No foreign key: the row is gone.
CREATE TABLE graft_gc_removed (
    repo_id       <int> NOT NULL PRIMARY KEY,
    storage       TEXT  NOT NULL,        -- for logs
    relative_path TEXT  NOT NULL,        -- for logs
    removed_at    <ts>  NOT NULL         -- database clock
);
```

Plus the owned-key trigger (PACK OWNERSHIP RULE 3), both dialects:

```sql
-- SQLite
CREATE TRIGGER graft_changesets_owned_pack BEFORE INSERT ON graft_changesets
WHEN NEW.pack_key IS NOT NULL AND NEW.pack_key NOT LIKE 'repos/%/packs/%-%.pack'
BEGIN
    SELECT RAISE(ABORT, 'storage core: the pack key is not an owned key; every node must run the same version');
END;

-- Postgres
CREATE FUNCTION graft_changesets_owned_pack() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.pack_key IS NOT NULL AND NEW.pack_key NOT LIKE 'repos/%/packs/%-%.pack' THEN
        RAISE EXCEPTION 'storage core: the pack key is not an owned key; every node must run the same version'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER graft_changesets_owned_pack BEFORE INSERT ON graft_changesets
    FOR EACH ROW EXECUTE FUNCTION graft_changesets_owned_pack();
```

`<int>` = `INTEGER` on SQLite / `BIGINT` on Postgres; `<ts>` = RFC 3339 text in UTC
on SQLite / `TIMESTAMPTZ` on Postgres. The `Down` steps drop the trigger (and the
Postgres function) and the five tables. WP71 checks that sql-migrate executes the
trigger bodies (they contain `;` inside `BEGIN ... END` and `$$ ... $$`) as single
statements on both drivers, with a test.

### Types and pack keys (`internal/graft/corestore/types.go`)

```go
// Checkpoint is a consolidated state of a repository at a changeset sequence.
type Checkpoint struct {
    RepoID    int64
    Seq       uint64
    PackKey   string
    PackBytes int64
    CreatedAt time.Time // database clock
}

// RemovedRepository is a tombstone whose bucket prefix is still to be reclaimed.
type RemovedRepository struct {
    RepoID    int64
    Key       RepoKey
    RemovedAt time.Time // database clock
}

// GCState is what the collector decides on, read in one database snapshot.
type GCState struct {
    Now         time.Time            // database time of the snapshot
    Packs       map[string]uint64    // changeset pack key -> greatest referencing seq
    Checkpoints []Checkpoint         // ascending by Seq
    Orphans     map[string]time.Time // observed unreferenced key -> first_seen
}

var (
    ErrBlobExists             = errors.New("storage core: blob already exists")
    ErrCheckpointNotFound     = errors.New("storage core: checkpoint not found")
    ErrPackDeadline           = errors.New("storage core: the pack upload is older than the write deadline")
    ErrReviewRootsMissing     = errors.New("storage core: a commit of the review has no keep-around reference")
    ErrReviewRootInUse        = errors.New("storage core: the keep-around reference is the root of an open review")
)

// KeepAroundPrefix and KeepAroundRef name the references of the REVIEW ROOTS RULE.
const KeepAroundPrefix = "refs/keep-around/review/"
func KeepAroundRef(oid string) string

// NewPackKey returns a new owned pack key for a pack of the repository with the
// given SHA-256: "repos/<repo_id>/packs/<hex sha256>-<hex nonce>.pack", with a
// random 16-byte nonce.
func NewPackKey(repoID int64, sum [32]byte) string

// ParsePackKey returns the repository ID and the SHA-256 of an owned key or of a
// legacy key "repos/<repo_id>/packs/<hex sha256>.pack", and false for anything
// else. Only canonical spellings are accepted (lowercase hex, no leading zeros).
func ParsePackKey(key string) (repoID int64, sum [32]byte, ok bool)

// IsOwnedPackKey reports whether key is an owned key (not a legacy one).
func IsOwnedPackKey(key string) bool
```

`PackKey(repoID, sum)` (the legacy constructor) is removed: no caller may produce a
legacy key any more. Every call site (voter, `shadowpack.go`, import, tests) moves
to `NewPackKey`.

### BlobStore changes (`core.go`, `blobstore`)

```go
// BlobInfo describes a stored object.
type BlobInfo struct {
    Key  string
    Size int64
}

type BlobStore interface {
    // Put stores the content under key, which must not exist: an existing key is
    // ErrBlobExists and nothing is written. A pack key's content must hash to its
    // SHA-256 (ErrBlobCorrupt otherwise, nothing stored). A cancelled context
    // stores nothing.
    Put(ctx context.Context, key string, r io.Reader) error
    // Get, Exists, Close: unchanged.

    // List calls fn for every object whose key starts with prefix, in no defined
    // order, objects only (no directory entries). An error from fn stops the
    // iteration and is returned.
    List(ctx context.Context, prefix string, fn func(BlobInfo) error) error
    // Delete removes the object; a missing key is success.
    Delete(ctx context.Context, key string) error
}
```

**Atomic create-only on every driver (verified in the gocloud v0.46.0 sources).**
memblob checks and inserts under its bucket-wide mutex, so its `IfNotExist` is
atomic. fileblob is not: its writer's `Close` runs `os.Stat` then `os.Rename`
under a mutex created per writer (`fileblob.go` 764 and 789), which excludes
nothing, so two concurrent writers of one key both succeed and the second rename
overwrites the first; this holds within one process and between processes
sharing a file bucket. The phase-6 package doc's "fileblob supports IfNotExist" is
true only for sequential writes and is corrected. For file buckets `Put`
publishes the object itself with **link(2)**, the filesystem's own atomic
create-only primitive (PM review: an advisory lock binds only the writers that
honour it, and lock semantics vary on network filesystems, while a file bucket
shared by several hosts over NFS is a supported setup): fileblob's writer
creates the temporary file `<path>.<hex nanos>.tmp` in the key's directory (so
the key-to-path mapping stays fileblob's) and receives the content, `Put` fsyncs
it, `os.Link`s it to the key's path (EEXIST is `ErrBlobExists`), discards the
writer (whose cancelled `Close` only removes the temporary name) and fsyncs the
directories. A network filesystem can report EEXIST for a link that succeeded
but whose reply was lost and retransmitted; when the name is the same file as the
temporary one (`os.SameFile`), the publish was this writer's. No `.attrs`
sidecar is bypassed (file buckets are opened with `MetadataDontWrite`). File
buckets therefore need a filesystem with hard links (every local Linux/macOS
filesystem and NFS); one without them fails every `Put` with a clear error. The
flock version of 9c8bb58 is replaced. The upstream defect is a stated limit in
STORAGE.md. S3, GCS and Azure use conditional requests, atomic on the server
(untested here).

`List` uses `bucket.List(&blob.ListOptions{Prefix: prefix})` and skips `IsDir`
entries; `Delete` maps `gcerrors.NotFound` to success. fileblob lists every file
except its `.attrs` sidecars, **including the temporary file of a write in flight
and one that a crashed writer left behind** (`<path>.<nanos>.tmp`, in the key's
directory because the store opens file buckets with `NoTempDir`). The collector
therefore applies the orphan protocol to every unreferenced object under a
repository prefix, whether its key parses as a pack key or not: a temporary file
older than `gc_grace` belongs to a writer whose deadline has passed. `BlobInfo` carries no
modification time on purpose (the CLOCK RULE: GC never reads bucket times). A
file bucket may keep empty directories after the last key under them is deleted;
the keys, which are what matters, are gone (documented).

### MetaStore additions (`metastore.go`, `sqlstore`)

Implemented on both dialects with the phase-6 patterns (`bind`, `withTx` /
`withReadTx` with the busy/serialization retry, `timeArg`/`scanTime`,
`bytesArg`/`targetArg`, name chunks of `maxNamesPerQuery`).

```go
// CheckpointInput is the state CreateCheckpoint records.
type CheckpointInput struct {
    Seq       uint64            // 1..head_seq, greater than the newest checkpoint's Seq
    Refs      map[string]Target // the reference snapshot at Seq: no absent value, not empty
    PackKey   string            // an owned key, already uploaded
    PackBytes int64
}

type MetaStore interface {
    // ... phase-6 methods ...

    // CreateCheckpoint records a checkpoint and its snapshot in one transaction
    // with created_at = the database time. It locks the repository row and
    // requires Seq <= head_seq, Seq > the newest checkpoint's Seq and an owned
    // PackKey of the repository (else an error, nothing written). A checkpoint at
    // Seq that exists already is returned unchanged with created=false (its pack
    // is kept; the caller's pack becomes an orphan). ErrRepositoryNotFound for an
    // unknown repository.
    CreateCheckpoint(ctx context.Context, repoID int64, in CheckpointInput) (cp Checkpoint, created bool, err error)
    // NewestCheckpoint returns the checkpoint with the greatest Seq; ok is false
    // when there is none.
    NewestCheckpoint(ctx context.Context, repoID int64) (cp Checkpoint, ok bool, err error)
    // SyncBoundary returns the Seq of the newest checkpoint created at least age
    // ago on the database clock, 0 when there is none (the C_sync of the GC
    // BOUNDARY rule).
    SyncBoundary(ctx context.Context, repoID int64, age time.Duration) (uint64, error)
    // CheckpointRefs returns the snapshot of the checkpoint at seq;
    // ErrCheckpointNotFound when there is none.
    CheckpointRefs(ctx context.Context, repoID int64, seq uint64) (map[string]Target, error)
    // DeleteCheckpoint removes the checkpoint row and its snapshot; false when
    // there was none.
    DeleteCheckpoint(ctx context.Context, repoID int64, seq uint64) (bool, error)

    // GCSnapshot reads, in one read transaction, the database time, the changeset
    // pack keys with their greatest referencing sequence, the checkpoints and the
    // orphan observations of the repository. ErrRepositoryNotFound if unknown.
    GCSnapshot(ctx context.Context, repoID int64) (GCState, error)
    // RecordOrphans stores first_seen = the database time for every key that has
    // no observation yet (existing observations are kept).
    RecordOrphans(ctx context.Context, repoID int64, keys []string) error
    // ForgetOrphans removes the observations of the keys (deleted, or referenced
    // since they were observed).
    ForgetOrphans(ctx context.Context, repoID int64, keys []string) error

    // AcquireMaintenanceLease takes the lease for holder until the database time
    // plus ttl, by compare-and-swap: it succeeds when there is no lease, the lease
    // has expired, or holder holds it already. RenewMaintenanceLease extends it
    // when holder still holds it (held=false otherwise). ReleaseMaintenanceLease
    // frees it when holder holds it. They never bar a read or a write.
    AcquireMaintenanceLease(ctx context.Context, repoID int64, holder string, ttl time.Duration) (acquired bool, err error)
    RenewMaintenanceLease(ctx context.Context, repoID int64, holder string, ttl time.Duration) (held bool, err error)
    ReleaseMaintenanceLease(ctx context.Context, repoID int64, holder string) error

    // ListRepositories returns every repository of the given storage whose import
    // is complete (head_seq > 0), ascending by ID. An empty storage lists the
    // repositories of every storage.
    ListRepositories(ctx context.Context, storage string) ([]Repository, error)

    // ListRemovedRepositories returns the tombstones of the given storage (every
    // storage when empty) whose removed_at is at least minAge ago on the database
    // clock (change F).
    ListRemovedRepositories(ctx context.Context, storage string, minAge time.Duration) ([]RemovedRepository, error)
    // DeleteRemovedRepository removes a tombstone; idempotent.
    DeleteRemovedRepository(ctx context.Context, repoID int64) error
}
```

**InsertReview and UpdateReview (changed, REVIEW ROOTS RULE).** Both take a new
last parameter `requireRoots bool`. When it is set, the transaction locks the
repository row first and requires the keep-around references of the review's head
and base to exist with their commit as value, else `ErrReviewRootsMissing` and
nothing is written. Comments, verdicts, closes and imports pass false.
`dbreview.WithRequiredRoots` is the store of the writes that pass true.

**CommitChangeset (changed, REVIEW ROOTS RULE).** A non-forced changeset that
deletes or moves a keep-around reference whose value is the head or base of an
open review is refused with `ErrReviewRootInUse`; nothing is written. Forced
(shadow) changesets are not refused.

**DeleteRepository (changed).** In the transaction that deletes the repository row
it reads the row's `repo_id` and inserts the `graft_gc_removed` tombstone with
`removed_at` = the database time. The cascade removes changesets, references,
changelog, reviews, checkpoints, observations and the lease; the tombstone stays.
An unknown key writes nothing and returns false, as before. Signature unchanged.

### Core additions (`core.go`, `open/open.go`)

`corestore.Core` gains `GCGrace` and `GCMargin time.Duration` (from the config,
also in shadow mode: GC runs there too, so the writers' deadline applies) and the
method `func (c *Core) WriteDeadline() time.Duration` = `GCGrace − GCMargin` (the
`W` of the PACK OWNERSHIP RULE).

### gitops addition

```go
// BuildPackFrom writes a self-contained (non-thin) pack of every object
// reachable from tips: `git rev-list --objects --stdin` (the tips on stdin, no
// --not) piped into `git pack-objects --stdout`, hashed into a file in tmpDir.
// Zero tips are ignored; no tips is an empty Pack. A tip that is not in the
// repository is an error, never a partial pack.
func BuildPackFrom(ctx context.Context, repo *localrepo.Repo, tips []git.ObjectID, tmpDir string) (Pack, error)

// MissingObjects returns the tips that are not in the repository, in the order
// given (one `cat-file --batch-check` run). Used by reviewroots.Backfill.
func MissingObjects(ctx context.Context, repo *localrepo.Repo, tips []git.ObjectID) ([]git.ObjectID, error)
```

### Writers (WP73, change A on the write path)

**One upload helper (binding).** Every path that uploads a pack and then records
a row (the voter's Preparing and Prepared phases, `ImportRepository`, the
checkpoint builder; no other production code calls `Put`) goes through

```go
// PackUpload is a pack uploaded under a new owned key, with the deadline before
// which a row may reference it (PACK OWNERSHIP RULE 4).
type PackUpload struct {
    Key      string
    Bytes    int64
    Deadline time.Time // readAt + WriteDeadline(), monotonic
}

// UploadPack uploads the pack read from r (size bytes, SHA-256 sum) under
// NewPackKey(repoID, sum), with the Put under a context whose deadline is
// readAt + WriteDeadline(). readAt is the time of a repository-record read that
// found the repository, taken before the upload.
func (c *Core) UploadPack(ctx context.Context, repoID int64, readAt time.Time, sum [32]byte, size int64, r io.Reader) (PackUpload, error)

// CommitContext returns ctx bounded by the upload's deadline, or an error
// (ErrPackDeadline) when the deadline has passed: the caller must not start the
// commit and must not reference the key.
func (u PackUpload) CommitContext(ctx context.Context) (context.Context, context.CancelFunc, error)
```

Each path is tested past `W` (with a shortened `gc_grace` or a hook): the `Put` is
cancelled or the commit is not started, the write fails as a storage failure, and
no row references the key.

* **Voter.** `preparePack` records `t_r` after its `GetRepository` and uploads with
  `NewPackKey` under a context with deadline `t_r + W`; `preparedPack` keeps the
  deadline. `prepare` (Prepared) uses the prepared pack only when the repository ID
  matches **and** its deadline has not passed; otherwise it calls `buildAndUpload`
  with a new `t_r` from the record it has just read. The `CommitChangeset` call runs
  under the pack's deadline (the earlier of the request context and `t_r + W`). A
  transaction without a pack has no deadline. Shadow mode: the same, inside the
  shadow timeout (which is far shorter than `W`).
* **ImportRepository.** After building the full pack it re-reads the record
  (`GetRepository`, which also confirms the registration still exists), uploads
  with `NewPackKey` and commits changeset 1 under `t_r + W`. A resumed import uploads
  a new owned pack; the pack of the interrupted attempt is an orphan.
* **Checkpoint builder.** See below; same rule.

### Checkpoint creation (`internal/graft/corestore/checkpoint`)

```go
func New(core *corestore.Core, logger log.Logger, locator storage.Locator,
    repoFactory localrepo.Factory, fresh corestore.Freshener) *Checkpointer

// Create builds a checkpoint of repo at the current database head and records
// it. created is false when a checkpoint at the head exists already.
func (c *Checkpointer) Create(ctx context.Context, repo *gitalypb.Repository) (cp corestore.Checkpoint, created bool, err error)
```

The fault hook is an unexported field of the `Checkpointer` that tests set
(`export_test.go`), like the syncer's test hook, not a package variable, so
that parallel tests do not share it. Its points are `after_upload` and
`after_commit`. `Create` takes `*gitalypb.Repository` because the repository
factory builds from it; both callers (the RPC handler and the maintenance
runner) have one.

Binding order:

1. Require authoritative mode (else `FailedPrecondition`, SHADOW SCOPE). Read
   `(refs, N) = Meta.Refs(repoID)` (one snapshot). If `NewestCheckpoint` is at
   `N`: return it, `created = false`. (WP74: the reftable check of the draft is
   dropped. The local repository may not exist before step 2, and EnsureFresh
   replaces a copy that is not reftable, so the check could only refuse a node
   that has no copy yet. The build only reads objects, which does not depend on
   the reference backend. The object hash comes from the repository record.) The
   object tips of `refs` (symbolic references skipped) are the closure; the open
   reviews' commits are among them through their keep-around references (REVIEW
   ROOTS RULE), so the build reads no reviews. No object tip → return
   `created = false` (nothing to consolidate).
2. **Change D:** `EnsureFresh(repo)`. EnsureFresh catches up to a head read after
   the call arrived, which is `>= N`; then read the local watermark and require
   `wm.RepoID == repoID && wm.Seq >= N` (else `Internal`: the Freshener contract is
   broken, never retried around). The local repository now holds every object of
   the changesets `<= N`, hence every object reachable from `refs`.
3. `pack = BuildPackFrom(local, tips, tmpDir)`; a missing object fails the build
   (`Internal`), never a partial pack.
4. Re-read the record (`t_r`), `Core.UploadPack`. `//! after_upload`.
5. `CreateCheckpoint(repoID, {Seq: N, Refs: refs, PackKey, PackBytes})` under the
   upload's `CommitContext`. `//! after_commit`. In its transaction the last
   statements, after the snapshot is written, check the pack's deadline on the
   database clock and stamp `created_at` with the database time (final minor 1):
   the age of a checkpoint (C_gc, C_sync) counts from a time one statement
   before the commit, whatever the size of the snapshot, so the visibility lag
   does not grow with the number of references or lean on `gc_margin`. Tests:
   `TestCheckpointOfManyReferences` (created_at after the snapshot, 100k
   references), `TestCheckpointDeadlineIsCheckedAfterTheSnapshot` (a deadline
   that passes during the snapshot fails the record, nothing recorded).

A crash after 3 leaves the pack as an orphan (reclaimed by the orphan protocol); a
re-run starts over from the head at that time. The build holds no reference lock
and never blocks or fails a write (CKPT-2).

**Change E (fencing).** `CreateCheckpoint` does **not** check the maintenance lease
in its transaction, and checkpoint correctness does not rely on the orphan grace
being a guess either: it relies on (a) per-sequence idempotency (a second build
at the same `N` returns the existing checkpoint, its own owned pack becomes an
orphan), (b) monotonic sequences (a checkpoint at a smaller `N` than the newest
is refused), and (c) the enforced orphan protocol for the loser's pack. Two
checkpoints at different sequences built by two nodes whose leases overlapped are
both valid states. The lease only prevents wasted builds.

### Pack GC (`internal/graft/corestore/gc`)

```go
func New(core *corestore.Core, logger log.Logger) *Collector

type Report struct {
    Boundary           uint64 // C_gc
    CoveredDeleted     int
    CheckpointsDropped int
    OrphansDeleted     int
    OrphansObserved    int    // newly observed, kept for now
    OrphansInGrace     int    // observed earlier, not old enough yet
    PacksDeleted       int
    BytesReclaimed     int64  // sizes from List
    RemovedRepos       int
    DryRun             bool
}

// CollectRepository reclaims the dead packs of one repository. The caller holds
// the repository's maintenance lease (efficiency only).
func (c *Collector) CollectRepository(ctx context.Context, repoID int64, dryRun bool) (Report, error)

// CollectRemoved reclaims the bucket prefixes of the tombstones of the storage
// (every storage when empty) that are older than gc_grace, then deletes the
// tombstones.
func (c *Collector) CollectRemoved(ctx context.Context, storage string, dryRun bool) (Report, error)

```

The fault hook is an unexported field set by tests (as for the checkpointer),
with the points `after_drop_row` (a superseded checkpoint's row deleted, its
pack not yet), `mid_sweep` (after the first deletion of the sweep),
`after_delete` (the sweep's deletions done, the observations not yet updated)
and `after_prefix` (a removed repository's objects deleted, its tombstone not
yet). Errors carry gRPC codes: `NotFound` for an untracked repository,
`Unavailable` for the database and the bucket, `FailedPrecondition` for
invalid GC times.

**List under concurrent deletion (found by the WP75 race test).** fileblob's
own listing reads a directory and then stats each entry, and fails with
`NotFound` when an entry was deleted in between, which is exactly what two
overlapping collections do. `blobstore.List` on a file bucket therefore walks
the directory itself: an entry that vanished is an object that is gone and is
not listed. The `BlobStore.List` contract states that an object deleted during
a listing is listed or not, never an error
(`TestListWhileDeleting`, which fails with fileblob's listing).

`CollectRepository` (binding):

1. `st = GCSnapshot(repoID)`. `C_gc` = the greatest `Seq` among `st.Checkpoints`
   with `CreatedAt <= st.Now − gc_grace` (0 when none).
2. **Drop superseded checkpoints:** for each checkpoint with `Seq < C_gc`:
   `DeleteCheckpoint` (row first), `//! after_drop_row`, then `Blobs.Delete` of its
   pack (owned by that row only, and no reader uses a checkpoint below `C_gc`).
   A crash in between leaves an unreferenced pack, reclaimed by the orphan protocol.
3. **Live set** (per key, change G): kept checkpoint packs (`Seq >= C_gc`) and
   every key of `st.Packs` whose greatest sequence is `> C_gc`. **Covered set:**
   keys of `st.Packs` whose greatest sequence is `<= C_gc`, minus the live set.
4. **Sweep** `Blobs.List("repos/<repoID>/packs/")`, for each key:
   * live → keep;
   * covered → `Blobs.Delete`;
   * referenced by no row (a pack key or any other object under the prefix, such
     as a file bucket's leftover temporary file): if `st.Orphans[key] <= st.Now − gc_grace` →
     `Blobs.Delete` and then forget the observation; if it has a younger
     observation → keep (in grace); if it has none → collect it for
     `RecordOrphans`.
   `//! mid_sweep` after the first deletion. Finally `RecordOrphans(new)` and
   `ForgetOrphans` for the observations whose key is now referenced or was not
   listed.
5. Dry run: steps 1 and 3, and the sweep only counts (no delete, no observation
   written).

Every decision comes from one snapshot and the PACK OWNERSHIP proof; `Delete` of a
missing key is success; a crashed run converges on the next one. Two collectors
racing on one repository (lease expired) both make only safe decisions: deletes
are idempotent, observations are inserted with "do nothing on conflict".

### Removed repositories (change F)

`CollectRemoved`: `ListRemovedRepositories(storage, gc_grace)`; for each tombstone,
`Blobs.List("repos/<repo_id>/")` and delete every key, `//! after_prefix`, then
`DeleteRemovedRepository`. A tombstone is swept only once `removed_at` is at least
`gc_grace` old: a writer whose record read found the repository did so before
`removed_at`, and its upload is bounded by `t_r + W`, so no upload can arrive in the
prefix after the sweep (the PACK OWNERSHIP RULE makes this an enforced bound, not
an assumption). A crash after a partial prefix deletion leaves the tombstone; the
next run finishes it. No lease (the repository is gone and its ID is never reused).

**Every deletion has a tombstone (integrated review m4, approved by the PM).**
Before migration `20261019000000_graft_storage_reconcile`, only `DeleteRepository`
wrote the tombstone; a row deleted otherwise (the repair procedure of STORAGE.md,
"State and verify", deletes it by hand) left its prefix in the bucket forever.
The migration adds the trigger `graft_repositories_tombstone`: `AFTER DELETE` on
`graft_repositories`, it records the tombstone of the deleted row on the database
clock (`clock_timestamp()`; SQLite's `'now'`, the host clock that SQLite's
`wallTime` reads too), in the deleting transaction, whoever deletes.
`DeleteRepository` relies on it. On a tombstone of the same ID (a reconciled one,
below) the trigger replaces it: the grace of a removal starts at the removal.

**Reconciliation of the leaks from before the migration.**
`gc.Collector.Reconcile` records a tombstone with an empty key (`RecordLeak`) for
a repository ID that has objects under `repos/<id>/` and neither a row nor a
tombstone; `CollectRemoved` then treats it like any removal
(`ListRemovedRepositories` returns the keyless tombstones in every storage's
list). It runs in the background maintenance before `CollectRemoved` (with `gc =
"auto"`) and in `CollectRemovedRepositories` (`graft storage gc --removed`).
* *Which IDs.* The allocator (Postgres sequence, SQLite `sqlite_sequence`) has
  no time per ID, so every run records its position with the database time
  (`graft_gc_allocator_marks`, `ObserveAllocator`), and examines only IDs at or
  below the greatest position observed at least W ago (`Bound`): IDs allocated
  at least W ago. It keeps the newest such mark and those after it.
* *Proof that a creation in flight is never collected.* Invariant U: an object
  under `repos/<id>/` is written only by `Core.UploadPack(record, readAt, …)`,
  whose record is a read that found the row of `id` (RULE 26: the only uploader;
  `Blobs.Put` has no other caller outside tests); so a creation
  writes nothing before its row commits, and its prefix is empty: the
  reconciliation, which tombstones only a prefix with objects, leaves it alone.
  Two more barriers, for a writer that would break U: (1) an ID younger than W
  is not examined (the PM's guard; on Postgres the sequence counts an
  allocation before its row commits, on SQLite an uncommitted allocation is
  invisible and the single write lock serialises it with the reconciliation);
  (2) a tombstone whose ID has a row is not a removal: `ListRemovedRepositories`
  excludes it, so a creation that commits after it was tombstoned keeps its
  objects, and its later deletion replaces the tombstone with a fresh grace.
  Test: `TestReconcileNeverCollectsACreationInFlight` holds a Postgres
  transaction between the allocation and the commit of a row, with an object
  under its prefix, across reconciliations: not examined while younger than W;
  examined after, tombstoned, then committed, and not collected.
* *Safety of a tombstoned leak.* No row for the ID now, and objects under its
  prefix: by U the row was committed once and has been deleted (IDs are never
  reused: Postgres `BIGSERIAL`, SQLite `AUTOINCREMENT`). A writer that read the
  row before the deletion uploads until `t_r + W`; the tombstone's `removed_at`
  is after the deletion, so the collection, `gc_grace > W` later, comes after
  every such upload, as for any removal.
* *Bounded, paged, idempotent, resumable.* A run examines at most 256 IDs after
  its cursor (`graft_gc_reconcile.done_through`, `UnrecordedIDs`), reads at most
  one object of each prefix (one page of a listing, stopped at the first
  object), records tombstones with "do nothing on conflict" and then moves the
  cursor past the batch. A run that stopped leaves the cursor, and the next run
  examines the batch again, recording nothing twice. With the trigger, a new
  ID never needs a second look: its deletion has its tombstone. After the
  backlog, a run examines only the IDs allocated since the last one.
* *IDs above the allocator* (a database restored from an older backup: the
  allocator will hand those IDs out again) are probed, 16 above the position,
  counted and logged, never touched.
* Metrics: `graft_storage_maintenance_leaks_found_total`,
  `graft_storage_maintenance_leaks_tombstoned_total`,
  `graft_storage_maintenance_ids_ahead_of_allocator`, and the step
  `reconcile` of `graft_storage_maintenance_failures_total`; the RPC returns
  `leaks_found`, `leaks_tombstoned`, `ids_ahead_of_allocator`.
* Why not an operator RPC that lists the bucket: the leaks then depend on an
  operator remembering to run it, and a listing of the whole bucket costs O(all
  objects) on every run. Why not a full listing in the background: the same
  cost, every interval. The cursor makes the cost O(new IDs) after the backlog.
* Tests: `TestDeletedRowGetsATombstone`, `TestReconcileTombstonesLeaks`,
  `TestReconcileIsPagedAndResumable`, `TestReconcileNeverCollectsACreationInFlight`
  (both dialects where the dialect allows; mutants without the W guard or
  without the row exclusion fail them), `TestRunOnceReconcilesLeaks`
  (metrics), `TestCollectRemovedRepositoriesReconcilesLeaks` (RPC).

### Hydration from a checkpoint (syncer, WP76)

`hydrate` seeds from `NewestCheckpoint`:

1. Seed `S` = the newest checkpoint's `Seq` and its snapshot (`CheckpointRefs`),
   or `S = 0` without one.
2. `InitBare`; when seeded, download, verify (SHA-256 against the key, `repo_id`
   of the key) and index the checkpoint pack, and validate every name and target of
   the snapshot (RULE 22).
3. `f = newFetched(S)`; `fetch(..., f, r.HeadSeq)` downloads and indexes the packs
   `(S, head]` and folds their reference changes.
4. Final references = the snapshot, overwritten by `f`'s final values (an absent
   final value deletes); `applyBuild` once; watermark `{r.ID, f.head}`; `install`.

A missing checkpoint pack or a missing pack `> S` is an Internal error naming the
key (corruption, GC BOUNDARY rule).

*As implemented (WP76).* `syncer.seed` reads the newest checkpoint and its
snapshot. It checks the pack key's repository ID and validates every name and
target like a changeset's (names, targets, symbolic targets), then indexes the
pack through the existing `indexPack` (SHA-256 against the key). Final
references = snapshot overlaid by the fetched final values, where an absent
value deletes. `graft_storage_sync_checkpoint_seeds_total` counts the seeded
hydrations.

### Catch-up and the sync boundary (syncer, WP76, change C)

In `sync()`, each round reads `C_sync = SyncBoundary(r.ID, gc_grace − sync_timeout −
gc_margin)` and hydrates when `!ok || wm.RepoID != r.ID || !localCurrent ||
wm.Seq < C_sync`; otherwise it catches up as in phase 6 (the WP55 RULE unchanged).
`checkReplaceable` still runs before any hydration.

*As implemented (WP76).*
* `C_sync` is read only when a catch-up is needed: after the node's own noted
  sequences advanced the watermark, and only when it is below the head. So a
  fresh repository costs no extra query, and a node whose own writes carried it
  past `C_sync` is not re-hydrated.
* `install`'s errRestart condition is the exact negation of the new hydrate
  condition (`wm.Seq >= C_sync` added). Without it, a current but lagging copy
  would be sent back to sync forever.
* The age `gc_grace − sync_timeout − gc_margin` is taken as 0 when it is not
  positive (the configuration refuses that; only test cores have such
  times). With age 0 every checkpoint counts, which is the conservative end:
  more hydrations, never a collected pack.
* Audit of the bound: every pack read and git process of a round (`fetch`,
  `indexPack`, `applyBuild`, the catch-up loop, `install`'s lock) runs under
  the round's context, which `EnsureFresh` bounds by `sync_timeout` per flight.
  The only work outside it is the best-effort `ReportApplied` after the round
  (at most 1 s, no pack read).
* `TestSyncerRoundBoundedBySyncTimeout` shows that a bucket read is cancelled by
  the round's deadline and nothing is installed.

### Resumable rounds and the per-pack unit (after WP79's benchmark, approved by the PM)

The WP79 benchmark found that a hydration was one round, all or nothing,
within `sync_timeout`. At the default 10 s, a fresh node could not hydrate a
repository of about 1100 changesets with no checkpoint: every round started
from zero. Two root causes, both fixed. The bound itself stays: the GC BOUNDARY
rule needs every round to end within `sync_timeout`.

1. *Rounds keep what they did.*
   * `round` (`newRound`) stops the fetch from starting packs once only a
     quarter of the budget is left (`roundReserve`). The fetch then ends at the
     last changeset whose pack, and those of every changeset before it, are
     indexed.
   * A hydration installs its build at that changeset, with that watermark. It
     is at or above `C_sync`, since the seed is the newest checkpoint.
   * A catch-up applies the range it fetched; its extensions (a newer local
     write, an unexplained local value) are not bounded, because they are what
     makes the range explain the local repository.
   * A round that leaves the copy below the head answers `Unavailable` ("at
     changeset p of H"), counted as `graft_storage_sync_total{result="partial"}`.
     The next request's round goes on from p.
   * A round that fetched no changeset at all installs nothing.
2. *The unit of work is cheaper.*
   * `indexPacks` downloads, verifies and indexes up to `fetchConcurrency` (8)
     packs at once: a bucket read, a SHA-256 and a `git index-pack` each.
   * The packs are self-contained (index-pack without `--fix-thin`), so their
     order does not matter, and git writes each under its own name.
   * The reference changes were already folded in memory and applied once
     (`applyBuild` for a hydration, `applyMoves` for a catch-up).

**Who reads a partial copy outside `EnsureFresh`.** A partial copy is a valid
local repository at changeset p ≥ `C_sync`: what a node that was offline has.
It is read by the same readers as such a copy, and none of them serves it as
current:
* Every repository-scoped RPC goes through the gate, whose `EnsureFresh`
  answers `Unavailable` while the copy is below the head.
* The storage core RPCs: `VerifyRepository` runs `EnsureFresh` first;
  `GetStorageState` reports p as `applied_sequence`, which is what it is.
* The background workers:
  * the maintenance reads only the watermark, and its checkpoint runs
    `EnsureFresh`, so it waits for the head;
  * the review-roots sweeper runs `EnsureFresh`;
  * Gitaly's housekeeping optimizes the objects and serves nothing.
* Offline tools (`gitaly git`, `gitaly-debug`) read whatever is on disk, as for
  any lagging copy (STORAGE.md).

### Seeding in the background (integrated review M3)

The checkpoint pack is a whole repository. A hydration from it had to
download and index that pack within one sync round: at the default 10 s, a
repository of hundreds of MB was never hydrated again on a node that joined,
lagged below `C_sync` or had its copy moved aside. v0.2.0 caught up
incrementally instead. The round bound stays (GC BOUNDARY rule); the seed
leaves it:

* When a copy must be rebuilt and a checkpoint exists, `startSeed` runs the
  seed in a job of its own, one per repository. The job:
  * runs detached from the request, until `readAt + W` (W =
    `Core.WriteDeadline()`, `gc_grace − gc_margin`; `readAt` is taken before
    the checkpoint is read);
  * downloads, verifies and indexes the checkpoint pack, applies its validated
    snapshot and writes the watermark `cp.Seq`;
  * installs the build if `cp.Seq` is still at or above `C_sync`; otherwise it
    discards it (`errSeedSuperseded`), and the next round seeds from the
    newest checkpoint.
* Why W is enough, the argument of `checkStoredObjects`: the checkpoint read
  at `readAt` was the newest then. A newer one can become `C_gc`, and so let
  the collector delete this pack, only `gc_grace` after its own creation,
  which is after `readAt`.
* After the install, the changeset packs above `cp.Seq` are read by the
  ordinary bounded rounds, each of which checks `C_sync` again.
* The round waits for the job within its budget (`round.stopped`).
  * A seed that ends in time (a small repository) is caught up in the same
    round, counted `hydrated`.
  * Otherwise the round answers `Unavailable` ("being seeded from its
    checkpoint at changeset N"), counted
    `graft_storage_sync_total{result="seeding"}`, and the job goes on; later
    rounds join it rather than starting another.
  * A copy being seeded is never served: the local repository is the old one
    (moved aside at install), or none.
* A repository without a checkpoint is hydrated from changeset 1 by the
  resumable rounds.
* Test: `TestSyncerSeedsInTheBackground`. The seed is held by the test until
  three rounds have answered `Unavailable`; one seed only; then the next round
  catches up and every reference matches.
* Bench with a checkpoint pack of hundreds of MB: requested in a window (see
  the final report).
* **Deadline on the database clock (final minor 2).** The job's context ends
  at `readAt + W` on the host's monotonic clock, which stops while the host is
  suspended. A seed that fails is checked against the database clock
  (`checkSeedDeadline`: the record read again, `r.ReadAt + W`): past it, the
  failure is `Unavailable` "seed deadline passed", whatever the seed ran into
  (a checkpoint pack collected after the deadline reads as a missing pack,
  which would otherwise be reported as corruption). A seed that indexed the
  whole pack is installed even past its deadline: it holds every object of the
  checkpoint, verified against its key, and the changeset packs after it are
  covered by the C_sync check right before the install (`errSeedSuperseded`).
* **Removed mid-seed (final minor 3).** The same check finds a repository
  removed, or removed and registered again under a new ID: `Unavailable` "the
  repository was removed during the seed", nothing installed; the next request
  starts from the current state.
* **Lifecycle (final minor 3).** `StopSeeds` (deferred by `gitaly serve` after
  its servers, before the last durable step) cancels the seed jobs, waits for
  them, and refuses new ones ("the server is stopping"). Every build directory
  (`hydrate-*`) holds a flock on its `build.lock` until it is removed; after
  the flock the build checks that the file it locked is still the one at the
  path, else it fails (a cleanup found the lock free in between and removed
  the directory). `CleanBuilds`, at startup before the listeners, removes the
  directories whose lock is free (their process died), and those without a
  lock file older than the larger of W and `sync_timeout` (no hydration or
  seed lives longer; a v0.2 process, whose builds have no lock, may still run
  one during a graceful upgrade); it keeps those of a process that runs. A
  directory gone meanwhile (its process removed it) is not an error; one that
  cannot be checked or removed is kept, logged and counted
  (`graft_storage_build_cleanup_failures_total`): the cleanup never stops the
  server.
* Tests: `TestSeedSupersededByANewerCheckpoint`, `TestSeedPastItsDeadline`,
  `TestSeedOfARepositoryRemovedAndCreatedAgain`, `TestStopSeedsEndsTheSeedJobs`,
  `TestCleanBuildsRemovesOnlyDeadBuilds`, `TestCleanBuildsNeverStopsTheServer`,
  `TestLockBuildFailsWhenItsDirectoryWasCleanedUp`, and on Postgres
  `TestSeedInstalledPastItsDeadlineThenSuperseded` (the path above: a seed that
  indexed its pack is installed past its deadline on a database clock that
  runs ahead of the host's, then C_sync moves and the next pass seeds again
  from the newest).

**Tests.** `TestSyncerResumesAHydrationAcrossRounds`: rounds of 4 changesets
on a 10-changeset repository give Unavailable at 4 and at 8, with the
watermark installed each time, and then the head. All the existing syncer
tests run through the concurrent fetch.

### Verify after compaction (`verify_objects.go`, WP76)

`checkStoredObjects` indexes the newest checkpoint's pack and the changeset packs
`(S, head]` (all packs `1..head` without a checkpoint), then runs the reachability
check as today, and also checks the open reviews' head and base commits. It runs
under a deadline of `W` from its start (a dry hydration that read checkpoint `S`
must end before a checkpoint newer than `S` can become `C_gc`); a missing pack is
reported as a connectivity failure starting with `storage core objects:`.

*As implemented (WP76).*
* `checkStoredObjects` reads the newest checkpoint before the references. A
  checkpoint never exceeds the head at its creation, so the head read after it
  is at or above it.
* The open reviews' commits are checked through their keep-around references
  (REVIEW ROOTS RULE), which are database references; there is no separate
  review check.
* Running out of `W` is `Unavailable` ("did not finish within the write
  deadline").

### Background maintenance (`internal/graft/corestore/maintenance`, WP77)

```go
func New(core *corestore.Core, cp *checkpoint.Checkpointer, collector *gc.Collector,
    fresh corestore.Freshener, locator storage.Locator, logger log.Logger, cfg Config) *Runner
func (r *Runner) Start(ctx context.Context) (stop func())
```

**Off by default** (answer 3): it runs only with `maintenance = "auto"` and in
authoritative mode; the CLI is always available. Each tick (`maintenance_interval`):
`CollectRemoved` for this node's storages; then, for each repository of
`ListRepositories` that this node has a current local copy of:
`AcquireMaintenanceLease` (skip when held elsewhere), checkpoint when
`head_seq − newest checkpoint seq >= checkpoint_changesets` or the pack bytes of
the changesets after the newest checkpoint `>= checkpoint_pack_bytes`, then
`CollectRepository`, renewing during a long build and releasing at the end. Wired in
`serve.go` (the storage core block only), started with the server and stopped on
shutdown.

**As implemented (WP77).**
* Signature: `New(core, cp, collector, fresh, logger, cfg, storages []string)`.
  The node's storage names replace the locator, which has no list of storages,
  and `RunOnce` is exported for the tests.
* The lease holder is `node_id/PID`, so the old and the new process of a
  graceful upgrade never both hold a lease.
* A lost lease (a renewal that fails or finds another holder) cancels the run
  of that repository.
* The pack-byte threshold reads the changesets after the newest checkpoint a
  page at a time, only until the threshold is reached, and only when the count
  threshold did not decide already.
* A failure of one repository or one step does not stop the others; the
  failures are counted by step.
* `maintenance = "auto"` outside authoritative mode is a startup error, also
  when the mode is off.
* Metrics: `graft_storage_maintenance_{runs,checkpoints,packs_deleted,bytes_reclaimed,skipped,failures}_total`.
* Tests: checkpoint by count and by bytes; both thresholds off; skip without a
  local copy; skip when another holder has the lease, and the run's own release;
  an orphan pack deleted gc_grace after a run observed it; Start/stop.
* Not tested: a lease lost in the middle of a run (it needs a build slower than
  the renewal).
* **Changed after WP79 (PM): checkpoints are on by default, GC is opt-in.**
  With `maintenance = "off"` nothing created checkpoints, so every large
  repository eventually hit the hydration wall above. A checkpoint deletes
  nothing (it adds a pack and rows). So the setting is split:
  * `checkpoints` defaults to `"auto"` in authoritative mode;
  * `gc`, which deletes, defaults to `"off"`.
  `maintenance_interval` and the lease serve both, and `Config.Checkpoints` and
  `Config.Collect` gate the steps. Test: `TestRunOnceDoesOnlyWhatIsOn`. The e2e
  test of the background checkpoints runs with the defaults.
* **Integrated review M2 and m8.**
  * M2: with checkpoints on and GC off, every checkpoint stayed forever. The
    runs now drop the superseded checkpoints even with GC off
    (`gc.CollectCheckpoints`, step 2 of `CollectRepository` on its own), under
    the same `C_gc` rule: no reader uses a checkpoint below `C_gc`, whether the
    covered changeset packs are collected or not.
  * m8: what is due is decided by reads, before the lease: `checkpointDue` and
    `C_gc` = `SyncBoundary(gc_grace)`, and the count of checkpoints below it
    (`CheckpointsBelow`). A repository with nothing due is skipped
    (`nothing_due`) without a write. (Final minor 5: the runner first
    remembered the `C_gc` it dropped below; a build of an older sequence that
    committed after a newer one, which `CreateCheckpoint` allows since it does
    not lock the repository, then stayed until `C_gc` moved. The count sees it.)
  * Tests: `TestRunOnceDropsSupersededCheckpointsWithGCOff`,
    `TestRunOnceDropsALateLowerCheckpoint`.
* **Database openers (audit).** Only `gitaly serve` opens the storage database
  (`corestoreopen.Open`, one `sqlstore.Store` per process). The `graft` CLI and
  every other binary reach it through gRPC. Two Stores on one SQLite file exist
  only when two `gitaly serve` processes run on it: several nodes on one host,
  or the overlap of a graceful upgrade. STORAGE.md (Limits) states it.

### Configuration (`config_graft_storage.go`, my section)

```toml
[graft.storage]
# ... mode, database, bucket, node_id, sync_timeout, shadow_timeout ...
checkpoints           = "auto"     # auto | off: background checkpoints (authoritative only). Default auto there (changed after WP79).
gc                    = "off"      # off | auto: background GC (authoritative only). Default off.
maintenance_interval  = "1h"       # how often the background task runs
checkpoint_changesets = 1000       # checkpoint after this many changesets since the newest one (0 = never)
checkpoint_pack_bytes = 268435456  # ...or after this many pack bytes since the newest one (0 = never)
gc_grace              = "1h"       # age before an unreferenced pack is deleted and a checkpoint becomes C_gc
gc_margin             = "5m"       # safety margin of the writers' deadline (gc_grace - gc_margin) and the boundaries
lease_ttl             = "10m"      # maintenance lease duration, renewed during a build
```

Defaults are `Default*` constants with the values above. Validation (extends
`GraftStorage.validate`): `checkpoints` and `gc` in `{"", "off", "auto"}`, `"auto"` in authoritative mode only; durations positive;
thresholds `>= 0`; `gc_margin < gc_grace`; **`gc_grace > sync_timeout + gc_margin`**
(GC BOUNDARY rule). `checkpoint_retain` of the first version does not exist.

### RPCs (`proto/storage_core.proto`)

`StorageCoreService` is in the gate's `Passthrough`, so these RPCs are not
intercepted and need no `AuthoritativeMutators` entry.

```proto
// CreateCheckpoint builds a consolidated checkpoint of the repository at the
// current head and records it. Authoritative mode only.
rpc CreateCheckpoint(CreateCheckpointRequest) returns (CreateCheckpointResponse) {
  option (op_type) = { op: MUTATOR };
}
// CollectGarbage reclaims the packs this repository no longer needs: orphans
// observed at least gc_grace ago, packs covered by a checkpoint at least gc_grace
// old, and superseded checkpoints. It does not touch other repositories.
rpc CollectGarbage(CollectGarbageRequest) returns (CollectGarbageResponse) {
  option (op_type) = { op: MUTATOR };
}
// CollectRemovedRepositories reclaims the packs of the repositories of a storage
// that were removed at least gc_grace ago.
rpc CollectRemovedRepositories(CollectRemovedRepositoriesRequest) returns (CollectRemovedRepositoriesResponse) {
  option (op_type) = { op: MUTATOR scope_level: STORAGE };
}

message CreateCheckpointRequest  { Repository repository = 1 [(target_repository)=true]; }
message CreateCheckpointResponse {
  uint64 checkpoint_sequence = 1;
  bool   created             = 2; // false: a checkpoint at the head existed already
  int64  pack_bytes          = 3;
  uint32 references          = 4;
  uint32 review_commits      = 5; // open-review head/base commits included
}

message CollectGarbageRequest {
  Repository repository = 1 [(target_repository)=true];
  bool       dry_run    = 2;
}
message CollectGarbageResponse {
  uint64 boundary            = 1; // C_gc
  uint32 covered_deleted     = 2;
  uint32 checkpoints_dropped = 3;
  uint32 orphans_deleted     = 4;
  uint32 orphans_observed    = 5;
  uint32 orphans_in_grace    = 6;
  uint32 packs_deleted       = 7;
  int64  bytes_reclaimed     = 8;
  bool   dry_run             = 9;
}

message CollectRemovedRepositoriesRequest {
  string storage_name = 1 [(storage)=true];
  bool   dry_run      = 2;
}
message CollectRemovedRepositoriesResponse {
  uint32 repositories    = 1;
  uint32 packs_deleted   = 2;
  int64  bytes_reclaimed = 3;
  bool   dry_run         = 4;
}
```

Handlers (`service/storagecore/{checkpoint,gc}.go`): off → `FailedPrecondition`;
`CreateCheckpoint` in shadow → `FailedPrecondition`; `CollectGarbage` acquires the
lease (held elsewhere → `FailedPrecondition` "another maintenance operation of
this repository is in progress"), collects, releases. WP78 checks the exact
`op_type` options the repository's proto lint accepts for a storage-scoped mutator
and follows the existing storage-scoped RPCs.

### SDK and CLI

SDK (`sdk/storage_gc.go`): `func (r *Repo) StorageCheckpoint(ctx)
(*StorageCheckpoint, error)`, `func (r *Repo) StorageGC(ctx, dryRun bool)
(*StorageGC, error)`, and `func (c *Client) StorageGCRemoved(ctx, storage string,
dryRun bool) (*StorageGCRemoved, error)`.

CLI (`cmd/graft/storage.go`):

```
graft storage checkpoint [-repo R] [--json]
graft storage gc [-repo R] [--dry-run] [--json]
graft storage gc --removed [-storage S] [--dry-run] [--json]
```

`gc -repo R` touches only that repository (answer 1); `--removed` is the storage-wide
sweep of removed repositories and takes no `-repo`.

**As implemented (WP78).**
* `CreateCheckpointResponse` has no `review_commits`: since the REVIEW ROOTS
  RULE the review commits are references, counted in `references`.
* `CollectGarbage` takes the lease as `maintenance.RequestLeaseHolder`:
  `node_id/PID/request-N`, unique per request. So a request excludes the
  background runs (`node_id/PID`), and the other requests, of its own process
  too.
* The SDK methods are in `sdk/storage.go`, already among the operator-only
  sources of the agent allowlist test, not in a new `sdk/storage_gc.go`.
* `graft storage gc --removed` dials a client of the server without a
  repository: the workspace backend requires one.
* The handlers map the errors of `checkpoint` and `gc`, which carry their
  codes. Off is `FailedPrecondition`; a checkpoint in shadow mode is
  `FailedPrecondition`; an untracked repository is `NotFound`; an unknown
  storage is `InvalidArgument`.
* Tests:
  * the handlers on SQLite and Postgres: created and idempotent, refusals, an
    orphan collected after the grace, dry run, lease held by another holder
    and released after the request, removed repositories;
  * the SDK against a fake;
  * the CLI against a real authoritative server (`checkpoint`, `gc`,
    `gc --dry-run --json`, `gc --removed`) and its usage errors.

## Rejected alternatives

* **Local-only hidden keep-alive references for review roots** (option b of the
  WP74b decision). They would not touch liveness in the storage core, so a second
  root mechanism would stay beside the references. Every database-side reader of
  local references would need an exclusion: the voter's hook input,
  `ImportRepository`'s `ReadRefs`, verify, and the syncer's checks. The voter's
  `--not --all` would count them as reachable, so a later pack could omit objects
  that only such a reference protects. A node that hydrates later has no such
  reference until it learns about the review, so its housekeeping can prune in
  between. Review roots are database references instead (REVIEW ROOTS RULE).
* **Database fencing of deletions** (a `graft_gc_doomed` set serialized with
  `CommitChangeset`, direction 2 of the PM review). It keeps objects shared between
  rows and adds a lookup to every commit under the reftable lock. It is not complete
  by itself: after the collector deletes the blob and removes the intent, a writer
  whose `Put` skipped the key before the deletion can commit with neither the intent
  nor the blob present, so the writer needs an additional protocol (re-upload after
  the intent clears, and keep re-checking) whose correctness depends on the order of
  bucket operations outside the transaction. The in-flight orphans of each write still
  need a time rule on top. Ownership removes the sharing instead, needs no write-path
  database access, and makes the in-flight bound an enforced deadline.
* **Bucket modification times for orphans** (the first version). An object's mtime
  is set by the bucket provider's clock, with no bound on skew against the
  collector, untested for S3/GCS/Azure here, and under the old `Put` an existing
  object's mtime is not refreshed. Replaced by the collector's own observations on
  the database clock.
* **A pending record per in-flight upload.** Rigorous, but one more database write
  per transaction on the write path (RULE 4), and it still needs an expiry for
  records of dead writers, i.e. the same deadline. The deadline alone suffices.
* **Restart a sync when a covered pack is missing** (B of the first review).
  Under the GC BOUNDARY rule a sync never meets a reclaimed pack, so this path would
  be unreachable; a missing pack is corruption and is reported as such.
* **`checkpoint_retain`.** The lagging boundary already keeps every checkpoint a
  reader can use; a count adds nothing.
* **Fold the reference snapshot from changeset metadata at hydration time**: `O(N)`
  rows per hydration; the snapshot table is `O(refs)` rows written once.
* **Delete changeset and changelog rows below a checkpoint**: the changelog cursors
  must stay valid and the rows are cheap.
* **Additive catch-up from a checkpoint** (index the checkpoint pack into a
  far-behind cache): entangles the seed with the WP55 compare-and-swap logic;
  re-hydration is correct and the case is rare (the node lagged by about
  `gc_grace − sync_timeout`).
* **Carry unreachable objects into the checkpoint pack**: defeats compaction.
* **Full-bucket scan to find removed repositories**: `O(objects)` and racy; the
  tombstone is crash-safe and bounded.

## Invariants (binding; each has a test)

* **OWN-1.** Every pack object is written by exactly one writer and referenced by at
  most one row (legacy keys excepted); `Put` never succeeds on an existing key.
* **OWN-2.** A writer commits a row referencing its pack only before `t_r + W`; no
  writer of a not-upgraded binary can commit a pack-carrying changeset (trigger).
* **I1 (preserved).** Every object reachable from the current references (among
  them the keep-around references of the open reviews' commits) is in a kept pack.
* **ROOTS-1.** In authoritative mode every open review written with its roots
  required has the keep-around references of its head and base in the database;
  on every node that has caught up, housekeeping keeps those commits.
* **GC-1.** The collector never deletes a pack referenced by a changeset with
  `seq > C_gc` or a checkpoint with `seq >= C_gc`.
* **GC-2.** The collector never deletes a pack a write in progress may still commit
  (PACK OWNERSHIP proof).
* **GC-3.** No sync round and no verification ever needs a pack the collector
  deletes during it (GC BOUNDARY rule).
* **GC-4.** At most one checkpoint build or collection per repository at a time
  under normal operation (the lease); correctness does not depend on it.
* **GC-5.** A node whose watermark is below `C_sync` re-hydrates from the newest
  checkpoint; a node at or above it catches up without re-hydrating.
* **CKPT-1.** Hydrating from a checkpoint at `N` and replaying `(N, head]` yields
  exactly the state at head.
* **CKPT-2.** Building a checkpoint never blocks or fails a write.
* **REM-1.** A removed repository's prefix is swept only when its tombstone is at
  least `gc_grace` old, and no upload can arrive in it afterwards.
* **COMPAT-1.** A v0.1.0 database works after migration 2: no checkpoints means
  replay from 1; legacy keys stay readable and collectable.
* **CRASH/IDEMPOTENT.** Every GC and checkpoint step is crash-safe and a re-run
  converges; the database decides liveness.
* **HISTORY.** No changeset, changelog or review row is deleted by compaction.

## Crash and race cases (each tested)

1. Two writes with byte-identical packs (the dedup race of the review): each uploads
   its own key; the collector deletes the first's covered key; the second write's
   changeset and pack are intact (test with a fault hook between the second write's
   upload and its commit, the collector deleting in between, SQLite and Postgres).
2. A write whose upload is older than its deadline at Prepared time: it rebuilds and
   re-uploads a new owned pack; one whose commit cannot start before the deadline
   fails like a storage failure (test with a shortened `gc_grace`).
3. An unreferenced pack observed, then committed by its writer before its deadline:
   the next run forgets the observation and keeps the pack.
4. A not-upgraded writer inserts a legacy key: the trigger rejects it (test inserts a
   changeset with a legacy key through `CommitChangeset` bypassing `NewPackKey`).
5. Checkpoint crash after the upload, before the row: the pack is an orphan; a re-run
   commits a new checkpoint.
6. Collector crash after `DeleteCheckpoint`, before the pack deletion; mid-sweep;
   after a partial prefix deletion of a removed repository: the next run converges.
7. A node one changeset behind when a checkpoint is created catches up and does not
   re-hydrate (change C).
8. A node whose watermark is below `C_sync` re-hydrates and reaches head.
9. Hydration while the collector runs; GC concurrent with continuous lands; a
   checkpoint build concurrent with lands (all `-race`).
10. A checkpoint build on a copy that lags behind the head (change D): EnsureFresh
    brings it to `>= N` and the checkpoint is complete.
11. A write in flight on a repository being removed: its pack lands before the sweep
    (bound `t_r + W`), the sweep at `removed_at + gc_grace` deletes it.
12. Two nodes race for the lease: one wins; a holder that crashes lets the lease
    expire after `lease_ttl`.
13. Housekeeping on a node after an open review's branch is deleted, also on a node
    that hydrated after the review was created: the commit survives (keep-around
    reference). A sweep between a review's reference write and its database write:
    the write fails in the database and the service writes the reference again.

## Test plan (multilevel; SQLite and Postgres wherever a store is involved)

* **sqlstore unit** (both dialects): every new method; `CreateCheckpoint`
  monotonicity, idempotency (`created=false`), owned-key and `seq <= head`
  checks, snapshot round trip with non-UTF-8 names and symbolic targets;
  `GCSnapshot` maxima per key with legacy shared keys; `RecordOrphans` keeps an
  existing `first_seen`; lease CAS between two `Store`s and expiry on the database
  clock; `DeleteRepository` tombstone and cascade; `ListRemovedRepositories` age
  filter; `SyncBoundary`; the trigger rejects a legacy key and accepts an owned one
  and NULL; migration 2 on a migration-1 database (fixture) and the trigger bodies
  executed as single statements.
* **blobstore unit** (file and mem): `Put` on an existing key is `ErrBlobExists` and
  leaves the content unchanged; concurrent `Put`s of one key: exactly one wins;
  `List` by prefix, sizes, no directory entries; `Delete` idempotent.
* **types unit**: `NewPackKey`/`ParsePackKey`/`IsOwnedPackKey` round trips, legacy
  parsing, non-canonical spellings refused, nonce uniqueness.
* **gitops unit** (SHA-1 and SHA-256): `BuildPackFrom` is self-contained (`fsck` after
  indexing into an empty repository); `MissingObjects`.
* **voter/import unit**: owned keys on every upload; the deadline (prepared pack
  expired → rebuilt; commit past the deadline → refused); the phase-6 voter, import,
  syncer, gate and storagecore suites stay green.
* **checkpoint unit**: hydration from a checkpoint equals the source at head
  (references, `fsck`); a keep-around reference keeps its commit, a swept one
  does not; lagging copy (change D); `created=false`.
* **review roots**: see the REVIEW ROOTS RULE (WP74b tests).
* **gc unit**: every branch of the sweep; covered vs live per key; dropped
  checkpoints; observations (new, in grace, expired, referenced since); removed
  repositories younger and older than `gc_grace`; dry run deletes and writes
  nothing; re-run is a no-op.
* **-race**: crash/race cases 1, 9 and 12.
* **crash injection**: every `FaultHook` point of the checkpoint and gc packages,
  and the voter's `after_upload`, each followed by a re-run that converges and a
  `VerifyRepository --connectivity` that passes.
* **e2e** (`internal/graft/e2e/storage_gc_test.go`, two and three real gitaly
  processes, one database and one bucket, SQLite and Postgres): writes on A,
  `graft storage checkpoint`, `graft storage gc` with a shortened `gc_grace`, a fresh
  node B hydrates from the checkpoint (asserted from the downloaded keys and the
  `graft_storage_sync_*` metrics), serves, verify is consistent; a node below
  `C_sync` re-hydrates; `graft storage gc --removed` reclaims a removed repository;
  changelog and reviews intact.
* **binding tests of the second review** (each on SQLite and Postgres):
  * the deadline on every upload path (voter Preparing, voter Prepared, import,
    checkpoint): past `W` the `Put` is cancelled or the commit is not started, the
    write fails as a storage failure, and no row references the key;
  * `graft_gc_orphans` lifecycle: an observed key that a slow write then committed is
    forgotten by the next run and kept; an observed key gone from the bucket is
    forgotten; a crash between the blob deletion and the row deletion converges;
  * the trigger: a legacy-format insert is rejected with the documented error, an
    owned key and NULL are accepted, legacy rows stay readable, and GC uses the
    greatest sequence of a shared legacy key;
  * REVIEW ROOTS (WP74b, PM conditions): the housekeeping reproduction on two
    nodes; the sweep/create race within 3 attempts; the deletion guard through
    `DeleteRefs`, `WriteRef` and `UpdateReferences`; off and shadow mode; the
    backfill counts;
  * a sync round slower than `sync_timeout` and a dry hydration slower than `W` are
    cancelled;
  * (third review) a checkpoint build on a lagging node catches up first
    (EnsureFresh to `>= N`); the LOCK ORDER `-race` test on Postgres and SQLite.
* **measurement** (WP79, in the benchmark window the PM schedules): bucket size and
  fresh-node hydration time before and after a checkpoint and GC on a repository
  with many changesets; the preparing/prepared histograms before and after WP73.

## Acceptance criteria

* After a checkpoint and GC the bucket holds the kept checkpoint packs and the packs
  after `C_gc`, not the whole history; orphans and removed-repository packs are gone
  (measured).
* Fresh-node hydration time depends on `head − newest checkpoint`, not on `head`
  (measured).
* `graft storage verify --connectivity` is consistent after GC and compaction;
  `graft log`, `WatchChangesets` and reviews are unchanged.
* A v0.1.0 database migrates and every storage path works.
* Every phase-6 storage test is green (the blobstore `Put` tests rewritten to the
  new contract, stated in the report); the new tests pass on SQLite and Postgres.
* No live pack is ever deleted (OWN-1, OWN-2, GC-1..GC-3), proven by the race and
  crash tests.

## SHADOW SCOPE rule (binding)

Checkpoints and the background task are authoritative-mode only: a checkpoint seeds
hydration, which does not happen in shadow mode, and its snapshot is the database's
view, which is not authoritative there. `CollectGarbage` and
`CollectRemovedRepositories` also run in shadow mode (orphans and removed
repositories only; no checkpoints means no covered set). The pack ownership rule,
the trigger and the writers' deadline apply in both modes. Off mode opens no bucket:
every entry point returns `FailedPrecondition`.

## Work packages and split

| WP | content | depends on |
|---|---|---|
| WP71 | migration 2 (tables, trigger), every new `sqlstore` method, `DeleteRepository` tombstone | — |
| WP72 | `BlobStore` `List`/`Delete`, strict `Put`, owned pack keys in `types.go` | — |
| WP73 | writers: owned keys and the deadline in the voter, `shadowpack.go`, import; `Core` fields | WP71, WP72 |
| WP74 | `gitops.BuildPackFrom`/`MissingObjects`, `checkpoint` package | WP71–WP73 |
| WP74b | REVIEW ROOTS RULE: `reviewroots` (Protect, Sweep, Backfill), the review service, `dbreview.WithRequiredRoots`, the two database checks | WP74 |
| WP75 | `gc` package | WP71–WP73 |
| WP76 | syncer (seed, `C_sync`), `verify_objects.go` | WP71, WP74 |
| WP76b | LOCAL REFERENCE WRITES RULE: `localrefs` (admission, deferred durability), voter, syncer, `serve.go` recovery and durable steps | WP76 |
| WP77 | config, `maintenance` package, `serve.go` wiring | WP74–WP76 |
| WP78 | proto, handlers, SDK, CLI | WP74, WP75 |
| WP79 | e2e, STORAGE.md and CONFIGURATION.md, benchmark | all |

**As implemented (WP79, e2e and docs; the benchmark waits for its window).**
* `storage_gc_test.go`, in-process servers sharing the database and the
  bucket, on SQLite and Postgres:
  * a repository written on two nodes is checkpointed on node A;
  * node B collects every covered changeset pack once the checkpoint is
    gc_grace old;
  * a node that joins after the collection hydrates from the checkpoint and
    serves every write;
  * writes go on and every node serves them;
  * an orphan pack and a removed repository are collected.
* `storage_localrefs_test.go`, the real `gitaly` binary:
  * the daily housekeeping, scheduled a minute ahead, backfills the
    keep-around references of a review written without them (the
    authoritative `Housekeep` path end to end);
  * the background maintenance checkpoints;
  * the deferred durability scenarios.
* STORAGE.md:
  * checkpoints and garbage collection for operators;
  * the clock assumption of `gc_margin`, precisely: a rate drift over
    `gc_grace`, with a forward jump of the database clock counting;
  * the configuration;
  * the milestone's e2e.
* CONFIGURATION.md: every key.

**Opus writes** everything that deletes data, is concurrent, is a compare-and-swap
or is crash recovery: WP71–WP77 entirely, the handlers of WP78, and the race, crash
and e2e tests. **Haiku drafts** from an exact spec, monitored and checked by Opus:
the `zz_*_spec_test.go` spec tests of each WP (from this file and the signatures
only, in `/home/feder/m3-work/spec-tests7/<WP>/...`), the CLI and SDK plumbing of
WP78, table-driven scaffolding for the pure `sqlstore` methods, and first drafts of
the STORAGE.md and CONFIGURATION.md prose (Opus fixes and fills the measured
numbers). Work files live in `/home/feder/m3-work/`, never in `/tmp` (PM.md).

## Risks

* **Mixed-version clusters.** Old running nodes fail their pack-carrying writes as
  soon as migration 2 is applied (intended, the trigger); operators must upgrade every
  node together. Documented in STORAGE.md.
* **Lost dedup.** Byte-identical packs are stored once per write; measured.
* **Writer deadline.** A write slower than `gc_grace − gc_margin` (55 min by default)
  fails; an import of a very large repository must fit in it after its pack build
  (the deadline starts after the build).
* **Unreachable-object pruning** loses the diff of closed reviews whose commits are
  unreachable once their keep-around references are released (documented limit).
* **Reviews written before the REVIEW ROOTS RULE** whose commits are already lost
  cannot be protected; the backfill reports them and nothing else is held up.
* **Re-hydration** discards a far-behind cache's objects (rare).
* **Lease held by a crashed node** delays maintenance by at most `lease_ttl`.
* **S3/GCS/Azure untested**; the design reads no bucket times, so the clock risk of
  the first version is gone, but `List` and conditional `Put` are exercised on file
  and mem buckets only.

## Resolved questions (PM review of 85189d4)

1. `graft storage gc -repo R` touches only that repository; removed repositories are
   swept by `graft storage gc --removed` and by the background task.
2. The checkpoint closure includes the head and base commits of open reviews
   (through their keep-around references since WP74b).
3. `maintenance` defaults to `off` in this release. Superseded after WP79's
   benchmark (PM): `checkpoints` defaults to `auto` in authoritative mode, and
   `gc` stays `off` (see "Background maintenance").
4. The benchmark window is requested at WP79.

5. (Second review) Change A approved; the B/C refinement confirmed; the review
   service check is m3's (REVIEW ROOTS RULE); old running nodes' pack-carrying
   writes are counted as shadow errors in shadow mode, write failures in
   authoritative mode.

## Open questions for the PM

None open.

## Foundation touchpoints and ownership

New (m3): `corestore/checkpoint/**`, `corestore/gc/**`, `corestore/maintenance/**`,
`service/storagecore/{checkpoint,gc}.go`, `sdk/storage_gc.go`,
`internal/graft/e2e/storage_gc_test.go`. Changed (m3): `sqlstore/**`,
`corestore/{core,types,metastore}.go`, `blobstore/**`, `gitops/gitops.go`,
`voter/**`, `syncer/syncer.go`, `open/open.go`, `service/storagecore/{import,verify_objects}.go`,
`cmd/graft/storage.go`, `proto/storage_core.proto` (+ generated code),
`config_graft_storage.go`, `doc/graft/STORAGE.md`, `corestore/dbreview/**`,
`corestore/reviewroots/**`, and `internal/gitaly/service/review/**` (the REVIEW
ROOTS RULE writes only, assigned to m3 by the PM), `corestore/localrefs/**`
(WP76b). Granted by the PM for WP76b only: `internal/git/updateref` (the deferred
durability option), `internal/git/gitcmd` (the fsync and lock-timeout constants),
`internal/gitaly/hook/storage_voter.go` (and its test double) and the updater call
in `internal/gitaly/hook/updateref/update_with_hooks.go`. Shared, my section only:
`internal/cli/gitaly/serve.go`, `doc/graft/CONFIGURATION.md`. No change to the land
path: the voter is m3's; the land paths call it unchanged.

## RULES (binding, in addition to PLAN6 RULES 1–15)

16. **Packs only.** Compaction deletes packs, never changeset, changelog or review rows.
17. **Ownership.** Every upload uses a fresh owned key; `Put` never succeeds on an
    existing key; no code produces a legacy key.
18. **Enforced deadline.** A writer uploads and commits only before `t_r + W`.
19. **Observed age on the database clock.** An unreferenced pack is deleted only after
    the collector observed it unreferenced at least `gc_grace` earlier, decided in one
    snapshot; bucket times are never read.
20. **Lagging boundaries.** The collector uses `C_gc`; a sync decides with `C_sync`.
21. **Checkpoints are authoritative-mode only**; GC of orphans and removed repositories
    also runs in shadow mode.
22. **Untrusted data** (RULE 10 extended): checkpoint packs, snapshots and review
    commit IDs read from the database or the bucket are validated before they touch a
    local repository.
23. **Crash-safe order**: upload before record; row before pack deletion; prefix before
    tombstone deletion; `Delete` of a missing key is success.
24. **No fallback paths**: a pack the invariants say exists and does not is reported as
    corruption, never routed around.
25. **Review roots are references**: a review's head and base are kept by
    keep-around references written through the ordinary write path before the
    review is recorded; in authoritative mode the database requires them for a
    review write and refuses to delete one an open review uses, both under the
    repository row lock.
26. **One upload path**: every production upload goes through `Core.UploadPack`, and
    every row that references an upload is committed under its `CommitContext`.
    Enforced by `TestUploadPackIsTheOnlyUploader` (`internal/graft/corestore`):
    the module's non-test packages that depend on corestore are type-checked
    (`go list -export` and `go/types`, no new dependency), and the calls of
    `Put` on a value whose static type implements `corestore.BlobStore`,
    whatever its name, must be exactly one, in `Core.UploadPack`. It is
    invariant U of the reconciliation ("Removed repositories").
27. **Local references are a cache of the database**: in authoritative mode a local
    reference write without fsync happens only after its repository has a durable
    dirty marker; a marker is removed only after the stack is durable, under git's
    lock with no transaction in flight; a repository dirty at a kernel restart, or
    whose durable step failed, is moved aside and hydrated, never served.
