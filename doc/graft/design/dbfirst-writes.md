# DB-first writes (design for approval)

Status: approved by Federico on 2026-11-05 for the RPC path only (pushes keep today's order and
get only the shorter hold). Decisions: a DB-first write answers after the local apply; stage 1b's
whole-push refusal is accepted, with no upstream git proposal for now; per-branch admission is
dropped; stage 3 has the gate below plus Federico's decision. Stage 1b is implemented (branch
`p10-stale-old`, see its section); the other stages are not started.
Spec: [dbfirst.tla](dbfirst.tla) with [dbfirst.cfg](dbfirst.cfg) and the configurations
`dbfirst_*.cfg` beside it.

**Verdict: fits partly.** The idea fits the first-version design for the writes that the
server drives itself (every `OperationService` writer, `UserLand` and its batches included,
and `ResolveConflicts`), which is all of the agent SDK's write traffic and all of
`graft-bench`. It does not fit pushes: with stock git, receive-pack runs its own reference
transaction, and nothing can commit to the database ahead of it without telling the client the
wrong outcome. Pushes keep git's lock on their path; they get only the shorter hold of stage 1.

The smallest design that captures most of the gain is two changes, in this order:

1. **Shorter hold** (stage 1): take the local reference read and the repository read out of the
   locked Prepared phase for the server's own writes. Cheap, no new mechanism, also helps pushes
   a little.
2. **DB-first for server-driven writes** (stage 2, behind a flag that is off by default): such a
   write commits its changeset to the database without git's lock and without running git's
   reference transaction, and the syncer's existing catch-up, which already applies ranges of
   changesets in one checked `update-ref`, brings the local references up to the database. No
   new admission, no new log, no schema change.

Per-branch admission, the third part of the idea as stated, is dropped: the database's
per-reference compare-and-swap already orders writers of one branch, and the land batcher
already serializes the lands of a target branch (see [Per-branch admission](#per-branch-admission)).

## 1. Problem (measured on `0fccd7c`, Postgres, authoritative mode)

| Setup | Result |
|---|---|
| 1 repository, 50 agents on 50 branches | about 48 changesets/s, about 1,400 lands/min |
| same | admission wait (`graft_storage_ref_admission_wait_seconds`) mean 0.8-1.0 s |
| same | repository-wide write path held about 21 ms per transaction |
| 50 repositories, 1 agent each | 1.7-1.9x the lands/min, `UserCommitFiles` p50 about 500 ms lower |
| Postgres alone, across repositories | more than 3,100 commits/s |
| Postgres alone, within one repository | about 250-300 commits/s (the repository's sequence counter) |

What the numbers say:

- **The repository-wide git write path is saturated.** 48 changesets/s x 21 ms = 1.0 s per
  second: the path is busy all the time, and a writer waits for the writers ahead of it. 0.8-1.0
  s of wait is 40-50 holds, about the number of agents.
- **The database is not the bound.** Within one repository it takes 250-300 commits/s, 5-6
  times what the git path lets through.
- **The ceiling of removing the shared path on this machine is the 50-repository run, 1.7-1.9x.**
  That run does the same work without the shared repository path; its own bounds (CPU, the
  disk's fsyncs, the other stages of a land) apply to any design that removes the shared path.
  So no design here can promise more than about 1.8x lands/min on this machine, whatever it
  does to the lock.

What the hold is made of today (voter, `Prepared` phase, `internal/graft/corestore/voter/voter.go`,
under git's `tables.list.lock` and the FIFO `RefAdmission` in front of it):

1. git's prepare: lock `tables.list`, reload the stack, check the old values;
2. the voter's `GetRepository` (one database read, 1.1-2.2 ms on Postgres when measured in the
   Preparing phase);
3. `expectations`: one `for-each-ref` git process to read the local values;
4. `CommitChangeset`: one database transaction (row lock of the repository, compare-and-swap,
   changeset rows, changelog, land identities);
5. the dirty marker of the deferred durability (first write after a durable step only);
6. git's commit: the new table, inline compaction, the rename of `tables.list` (no fsync);
7. the `Committed` hook releases the admission and notes the sequence.

The split of the 21 ms between these steps has not been measured; the p8 work adds the metric
(stage 0 below). Everything in this design that depends on that split is marked as an estimate.

## 2. The design

### 2.1 What changes for a server-driven write

Every `OperationService` writer and `ResolveConflicts` update a reference through
`UpdaterWithHooks.UpdateReference` (`internal/gitaly/hook/updateref/update_with_hooks.go`), which
drives the hooks itself and runs `update-ref` with git's hooks disabled. That function is the only
place that changes. Today:

```plaintext
reservation check -> pre-receive -> quarantine migration -> update hook
  -> update-ref start -> Preparing (pack build + upload; admission at the end)
  -> git prepare (lock) -> Prepared (policy, record read, for-each-ref, DB commit)
  -> git commit (write, unlock) -> Committed (release, NoteCommitted, observer) -> post-receive
```

With `db_first_writes` on:

```plaintext
reservation check -> pre-receive -> quarantine migration -> update hook
  -> validate the new value as git's prepare would (section 2.3)
  -> Preparing (pack build + upload; no admission)
  -> policy (the same enforceGraftPolicy, on the policy in force now)
  -> DB commit (the same CommitChangeset: CAS old -> new, changelog, land identities)
  -> note "objects of seq are local" -> wait for the local apply of seq (bounded)
  -> observer -> post-receive -> answer
```

- **No git reference transaction runs for the write.** git's lock is not taken by the writer at
  all. The database transaction is the commit point (invariant 3), as today.
- **The expected old value is the caller's.** `UpdaterWithHooks` always has an explicit old
  value (zero means the reference must not exist), so the compare-and-swap is `ExpectValue old`,
  with no `for-each-ref`. The database decides, under the repository's row lock, against the
  truth instead of against this node's copy.
- **The local apply is the syncer's catch-up**, unchanged in its rules: it reads the range of
  changesets after the watermark from the database, accepts every local value the range explains,
  extends the range over newer writes of this node, and moves every reference of the range in one
  checked `update-ref -z --stdin` (removals first when needed), without hooks, then writes the
  watermark. One catch-up round applies every changeset committed before it read the head, so
  the writes of many agents share one git transaction and one hold of git's lock. Concurrent
  callers already share a round (`singleflight` in `Syncer.EnsureFresh`).
- **The pack is not downloaded again.** The changeset's objects are in the local repository
  already (the quarantine migration put them there). The writer records, in memory, that the
  objects of its sequence are local; the catch-up's fetch skips the download and `index-pack` of
  those sequences. After a restart the record is gone and the pack is downloaded as for any other
  node's changeset: correct, a little slower, and housekeeping drops the duplicates. If the apply
  finds an object missing (git refuses the update), the round downloads the pack and retries.
- **The writer waits for the apply of its own sequence, bounded by `sync_timeout`, and answers
  success once the database committed, whether or not the apply finished in time.** Waiting keeps
  what a client and the hooks see today: when the RPC returns, this node's references show the
  write, custom post-receive hooks that read references see it, and the land batcher's next batch
  reads the new tip. A timeout or failure of the apply is logged and counted; the write is durable
  and the next request's gate catches the copy up (and fails with `Unavailable` if it cannot, as
  a catch-up does today).

### 2.2 What stays on the hook path

- **Pushes** (`SSHReceivePack`, `PostReceivePack`), atomic or not.
- **Writers whose reference transaction git runs**: `WriteRef`, `UpdateReferences`, `DeleteRefs`,
  the review keep-around references, `CreateRepository`'s initial references.
- **Every write when the flag is off**, and every write in shadow mode.

They keep today's order (DB commit in `Prepared`, under git's lock), with the shorter hold of
stage 1. They share git's lock with the applier, in the FIFO `RefAdmission` order: a push now
waits behind one catch-up transaction per round instead of behind every RPC write.

### 2.3 The checks git's prepare made before the commit

Today git's `update-ref` prepare runs before the database commit and refuses some values. Without
it, such a value would reach the database and no node could apply it: a poison changeset. The
DB-first path must therefore refuse, before the commit, everything git's prepare refuses for a
single-reference update with an explicit old value:

| git refuses | DB-first check before the commit | Today's error, kept |
|---|---|---|
| a malformed reference name | `gitops.ValidateName` (git's rules) and the name length limit | `InvalidReferenceFormatError` |
| a new object that does not exist | the RPC's object reader (`cat-file --batch-check`, already open) | as `update-ref`'s error |
| a non-commit object on `refs/heads/*` | the same read, object type | `NonCommitObjectError` |
| an old value that does not match | the database's compare-and-swap (stronger: against the truth) | `MismatchingStateError` / `landConflict` |
| a name git cannot hold next to another (D/F) | the database's `refuseRefNameConflicts`, in the commit | `FileDirectoryConflictError` |
| a lock it cannot get | nothing to check: no lock is taken | none |

Stage 2 adds a test per row that feeds the invalid input to the DB-first path and requires the
refusal before anything is committed. Values that git accepts are applied by the catch-up with
the same `update-ref`, so a value that passes these checks is one the apply can write.

### 2.4 Configuration

```toml
[graft.storage]
mode = "authoritative"
db_first_writes = false   # default; true: server-driven writes commit DB-first
```

Startup refuses `db_first_writes = true` outside authoritative mode. The flag is per node and
changes nothing in the database: nodes with and without it work on the same repositories.

### 2.5 SQLite

SQLite has one write lock per database, taken in arrival order by the store's write queue. A
DB-first commit goes through it like any other. What DB-first removes is git's work and the
second database read from the critical section; what is left is SQLite's own commit, which on
SQLite is shared by every repository of the node anyway. Expect a smaller gain than on Postgres;
the gate for SQLite is no regression.

### 2.6 Shadow mode

Nothing changes, and the flag is refused there. In shadow mode git is the truth: a write must
happen in git first, and the database mirrors it. A DB-first write would invert that.

### Per-branch admission

The idea as stated replaces the repository's admission by one per reference name. In this design
DB-first writers take no admission at all: the database's compare-and-swap, under the
repository's row lock, decides between two writers of one branch, and the loser gets the lost-race
error it gets today (the land path retries it). The land batcher already serializes the lands of
a target branch. A node-local queue per reference would only save database round trips of
same-branch writers that race, so it is left to stage 4, if the conflict counts call for it. The
repository's `RefAdmission` stays, in front of git's lock, for the applier and the hook path.

## 3. The invariants, one by one

1. **Stock git clients and protocol.** No client changes. Pushes keep git's own transaction and
   today's semantics. The bundled server git is not patched.
2. **One dense, ordered changeset log per repository.** The sequence is assigned where it is
   today, `head_seq + 1` inside the commit transaction that holds the repository's row lock
   (Postgres `SELECT ... FOR UPDATE`, SQLite `BEGIN IMMEDIATE` behind the write queue). DB-first
   writers to different branches commit concurrently in the sense that none of them waits for
   git, but their commits still pass the row lock one at a time, about 3.6 ms each (250-300/s),
   so the log stays one sequence with no gap and no reordering. `ListChangesets`,
   `WatchChangesets`, sync, GC and the SDK read the same log. No per-branch log, no per-branch
   causal order. The local copy applies the log in order (see 2.1); it may hold a later write
   of this node's hook path before an earlier DB-first write, which the catch-up already allows
   (it never reverts a newer local write, and the watermark only claims applied prefixes).
3. **The database commit is the point of truth; local references are a copy.** More so than
   today: a DB-first write never touches a local reference before its commit, and the copy is
   brought up by the same catch-up that applies other nodes' changesets. TLC checks that a local
   value is always a database value (`LocalNotAhead`) and that the watermark never claims a
   changeset the copy does not hold (`WatermarkTruthful`).
4. **The server controls agents.** The same checks run in the same order: the reservation of
   `refs/graft/` before the hooks, pre-receive, the update hook (custom hooks and the database
   name check), then `enforceGraftPolicy` on the policy in force at that moment, then the commit.
   Today the policy is evaluated while git holds the locks; with DB-first it is evaluated right
   before the commit, which is the same point in the write's order (nothing changes the
   reference between the two except a concurrent write, which the compare-and-swap refuses).
   Exactly-once land identity is unchanged: the land records are written in the commit's
   transaction, `decide` reads them from the database (`LookupLand`), and the branch's
   compare-and-swap refuses a land built on a tip that moved (TLC: `ExactlyOnceLand`, with a
   client retry after a crash).
5. **Landing and conflict resolution are the core.** `UserLand`, the batcher and the conflict
   paths use `updateReferenceWithHooks` as today; only what happens inside it changes. Their error
   classification (lost race, conflict, D/F) keeps its types.
6. **MIT, one installable server, SQLite or Postgres, file or object-store buckets.** No new
   process, dependency or service; the code is the existing syncer and voter. Both databases work
   (2.5); buckets are untouched.

## 4. Questions the design must answer

**What does git do with a reference update when the model is inverted, and is git's write off the
critical path?** For server-driven writes there is no per-write git transaction: the catch-up
writes the references of a whole round in one `update-ref` under `tables.list.lock`, so git's
lock is held once per round instead of once per write, and no writer waits for it. For pushes,
git's own transaction still takes the lock, and the voter still commits inside it; stock git
gives no point before its transaction where the outcome of each update is known (section 6).

**What does a client see, and when?** An RPC client gets its answer after the database commit
and, normally, the local apply: the same answer as today, a few milliseconds of apply later
instead of 0.8-1.0 s of admission wait earlier. Lost races, D/F conflicts and policy denials are
the same errors. A database outage fails the write with `Unavailable` before anything is
durable. A push client sees exactly what it sees today.

**Read-your-writes.** For the writer: its RPC returns after the apply (or, after a timeout, its
next request's gate applies it). For every later request on this node: the gate's `EnsureFresh`
only answers with a sync that read the database head after the request arrived, so it applies
every write acknowledged before (TLC: `ReadYourWrites`). On other nodes: unchanged. Readers that
bypass the gate (the `HookService` inside a running write, the `StorageCoreService`, background
jobs) see the copy as it is, as they see other nodes' writes today.

**Crash between the database commit and the local apply.** Nothing is lost: the database has the
write, the copy is behind, and the next request catches it up, downloading the pack because the
in-memory note is gone. The client of the interrupted RPC sees a broken connection; a retried land
is answered `already_landed` from the land record (TLC, with a crash in every configuration:
`AckedDurable`, `ExactlyOnceLand`). This is today's `after_commit` fault point, made wider by the
apply wait; stage 2 adds a crash-injection point `after_dbfirst_commit` to the e2e crash test.
Other failure cases are in section 5.

**Ordering of applies against the log.** The catch-up applies ranges of the log in sequence
order and writes the watermark after git committed the range; the watermark moves through a
noted sequence of the hook path only when it is contiguous. A local reference can be ahead of
the watermark (a hook write of this node) but never ahead of the database.

**D/F conflicts and HEAD across concurrent branches.** The database refuses a pair in the commit
transaction (`refuseRefNameConflicts`, `edbedea`), under the row lock, so two DB-first writes that
create `refs/heads/a` and `refs/heads/a/b` are ordered and the second is refused with
`FileDirectoryConflictError`, the error callers handle today. The apply of a range that deletes
`a` and creates `a/b` runs in two steps (`applyChecked`), and names that conflict only at
different times are handled (`e738eba`, `9bc9c53`). `UpdaterWithHooks` writes the resolved target
of a symbolic reference (`WrittenReference`), never `HEAD` itself; `HEAD` is written by
`WriteRef` on the hook path and moved alone by the catch-up (`applyMoves`). Nothing new.

**The open bug: rejected updates of non-atomic pushes.** git feeds every update of a push's
transaction to the reference-transaction hook, also the ones it rejected
(`transaction_hook_feed_stdin` in the bundled git's `refs.c` writes every update). The `02fbb52`
stop-gap refuses the whole push when a create (zero old value) finds the reference existing. TLC
finds the wider case, and it is reachable on `main` today with two nodes
(`dbfirst_bug_multinode.cfg`): a push whose old value for a reference is the database's current
value while this node's copy is behind is rejected by git, yet the voter's compare-and-swap
succeeds and commits it. With stock git the old value of a command is always the value the
server advertised (`--force-with-lease` aborts on the client when they differ), so the way there
is an ABA on another node between the advertisement and the commands: the branch moves from v0
to v1, this node catches up to v1, the branch moves back to v0. The client's v0 is then the
database's value and not the copy's (`internal/graft/e2e/storage_stale_old_test.go`). The client is told the update was
rejected, the database has it, and the `Committed` phase notes its sequence, so the watermark
moves over a change the copy does not have: this node's copy diverges until the reference is
written again (TLC: `NoPhantom` and the liveness property `EventuallyConsistent` both fail).
DB-first makes this reachable on one node, because a DB-first write is in the database before it
is in the copy (`dbfirst_bug_staleupdate.cfg`). The fix is cheap and comes first (stage 1b): the
voter already reads the local value of every pushed reference under git's lock; an update whose
explicit old value differs from that value is one git rejected, and the whole push is refused,
as the stop-gap does for creates. The root fix (git not feeding rejected updates to the hook)
belongs upstream in git and is out of scope.

**Policy and exactly-once land identity.** See invariant 4 above.

**What changes in shadow mode.** Nothing (2.6).

## 5. Failure and crash cases

| Case | Today | With DB-first (server-driven writes) |
|---|---|---|
| process dies after the upload, before the commit | orphan pack, write fails | same |
| process dies after the commit, before the local write | copy behind, next request catches up | same, with the pack downloaded again |
| process dies during the apply's `update-ref` | (the syncer's case today) the transaction is all or nothing; the watermark is behind | same |
| power loss | dirty repositories moved aside and hydrated | same (no new durability state) |
| database unavailable | write fails `Unavailable` under git's lock | write fails `Unavailable` before any git work |
| the apply keeps failing (disk full, a bug) | catch-ups fail, requests get `Unavailable` | same: the gate fails the next requests, so this node stops taking writes for the repository |
| compare-and-swap lost | `MismatchingStateError`, land retries | same |
| bucket unavailable | the Preparing phase fails, Prepared retries the upload under the lock | the upload fails before the commit, the write fails `Unavailable` |
| a value git would refuse | git's prepare refuses it | refused by the checks of 2.3 before the commit |

## 6. Why pushes stay on git's lock

To take a push's write off git's lock, something would have to commit it to the database before
receive-pack's transaction and then have git agree. With stock server git:

- **Commit in pre-receive or in the Preparing phase.** git checks the old values, the D/F rules
  and the locks after that point, and in a non-atomic push it rejects updates one by one. The
  database would then hold updates the client is told were rejected, or the client would be told
  a durable write failed. Either breaks what a stock client relies on.
- **Atomic pushes only.** All-or-nothing makes the first case go away, but git can still fail
  after the database committed (a lock timeout, an I/O error), and the client is then told the
  push failed while it landed. Today that only happens on a crash.
- **A patched server git** that commits through the hook would be a second git to maintain
  against every upstream release.

So pushes keep the database commit inside git's transaction. They benefit from stage 1 (a shorter
hold) and from no longer queueing behind every RPC write. A push-only workload on one repository
stays bounded by one push transaction per hold, about 50-80 per second after stage 1 (estimate).

## 7. Expected gain

- **Server-driven writes (stage 2).** The repository-wide serial section of a write becomes the
  database's row lock, about 3.6 ms, instead of about 21 ms of git plus database. At the
  50-repository rate (about 85-90 changesets/s on one repository) that is a utilization of about
  0.3, so the queue nearly vanishes; the applier holds git's lock for one `update-ref` per round
  (a few ms per round, shared by every write committed before it). Expected at 1 repository and
  50 agents: lands/min 1.5-1.8x today (about 2,100-2,500), admission wait from 0.8-1.0 s to tens
  of ms, `UserCommitFiles` p50 300-500 ms lower. Upper bound: the 50-repository run, 1.7-1.9x,
  because it is the same work without the shared path. These are estimates from the measured
  numbers, to be confirmed by the stage gates.
- **Single agent.** A DB-first write trades the hold (about 21 ms, mostly uncontended at one
  agent) for the commit plus one catch-up round (estimate 10-20 ms). Expect no gain and at most a
  small loss.
- **Pushes.** Only the stage 1 savings and less queueing.
- **graft-bench and the SDK.** 100% of their reference writes are `UserCommitFiles` and `UserLand`,
  so all of the gain applies to the benchmark of record. A fleet that pushes with stock git gets
  only stage 1.

Stage 1 alone (estimate, to be confirmed by stage 0): removing the `for-each-ref` process and the
repository read from the hold saves 3-8 ms of the 21 ms under load, so 15-40% more changesets
per second on one repository. If stage 0 shows that most of the hold is these two reads, stage 1
may capture a large part of the gain alone, and stage 2 should be re-decided on its numbers.

## 8. Staged plan

Each stage merges alone, with its gate declared here, before it runs. Benchmarks follow the
project's protocol: interleaved arms (A B A B), at least four series per arm, every series started
with CPU pressure below 5 and the CPU at least 90% idle, Postgres, file bucket, batching on,
`--mode land`, 50 agents x 20 changes on 50 branches of one repository, conflict rate 0.2, seed 1,
unless a stage says otherwise.

### Stage 0: measure the hold (0.5 day, depends on p8)

Use the Prepared-phase metric of p8 to split the hold into its steps (git prepare, record read,
`for-each-ref`, commit, marker, git commit) at 1 and 50 agents.

- Gate: a table in BENCH.md with the split; the sum of the steps within 10% of the measured
  hold. No code beyond p8.

### Stage 1a: shorter hold for server-driven writes (1.5 days)

- `UpdaterWithHooks` marks its hook calls (in-process, so the mark cannot be forged by a hook
  caller); for those, `expectations` takes the old values from the input (`ExpectValue`, zero is
  absent) without `for-each-ref`. git's prepare has already refused a stale or existing old
  value before `Prepared` runs for these writes.
- The repository record read in Preparing is reused in Prepared for the same transaction (same
  repository ID, deadline still valid); the head-sequence-0 check moves into the commit
  transaction, which reads the row under its lock anyway.
- Gate: the Prepared-phase mean at 50 agents drops by at least 4 ms (or by the share stage 0
  attributes to the two reads, if smaller, then reported as such), lands/min not lower and p99
  not more than 10% higher than the base; the voter's unit tests and the e2e storage suites pass
  on SQLite and Postgres.

### Stage 1b: refuse rejected stale updates of non-atomic pushes (1 day)

- In `expectations`, for receive-pack (old value always sent), an update whose explicit old
  value differs from the local value read under git's lock is refused with the whole push, like
  the `02fbb52` stop-gap for creates. The message does not name the reference: as for the
  stale create, the client gets `fatal: in 'prepared' phase, update aborted by the
  reference-transaction hook` (over SSH) and the connection ends, with no status per
  reference; relaying the reason on stderr is not part of this stage. git runs the deletions
  of a non-atomic push in a transaction of their own first, so the deletions it accepted land
  when the refused update is a create or an update (STORAGE.md, Failures).
- Gate: an e2e test with two nodes reproduces the TLC trace of `dbfirst_bug_multinode.cfg`
  (an ABA of the branch on another node between the push's advertisement and its commands, so
  the client's old value is the database's value while the serving node's copy is behind): it
  fails on `main` (the database records the rejected update) and passes with the change; no
  benchmark change (pushes only, a comparison already read).
- **Done** (branch `p10-stale-old`). `TestStorageRejectedStaleUpdateIsNotRecorded` failed on
  `main` on SQLite and Postgres: the client got `! [remote rejected] HEAD -> topic (incorrect old
  value provided)` and the database held the rejected v2. The fix is one check in the voter,
  `rejectedByGit`, which now covers the stale create too: an old value of a receive-pack that is
  not the local value under the lock refuses the whole push (`MismatchingStateError`, counted as a
  `conflict`); shadow mode logs and counts it and does not refuse.

### Stage 2: DB-first server-driven writes, flag off by default (5-8 days)

- `[graft.storage] db_first_writes`, refused outside authoritative mode.
- `UpdaterWithHooks`: the DB-first branch of 2.1 and the checks of 2.3. The voter gets one entry
  point that runs the pack preparation (as `preparePack`) and the commit (the commit half of
  `prepare`, with `ExpectValue` from the input), without admission.
- Syncer: the in-memory note of sequences whose objects are local; the fetch skips their packs;
  a missing object makes the round download the pack and retry. A bounded wait for the
  watermark to reach a sequence (`EnsureFresh` already provides it).
- Metrics: `graft_storage_dbfirst_writes_total{result}`, `graft_storage_dbfirst_apply_wait_seconds`,
  `graft_storage_apply_lag_changesets` (head minus watermark, per scrape).
- Tests: one per row of 2.3; the e2e storage suites and the conformance harness with the flag on,
  on SQLite and Postgres; crash injection at `after_dbfirst_commit` (the client retries the land
  and gets `already_landed`, the copy catches up); concurrent DB-first writes and pushes on one
  repository (no lost update, every write in the changelog once, the copy equal to the database
  at the end).
- Gate (flag on against flag off, same tree): at 1 repository and 50 agents, lands/min at least
  1.4x and admission wait mean at most 100 ms, `UserCommitFiles` p50 at least 200 ms lower; at 50
  repositories with 1 agent each, lands/min within 10% of flag off; at 1 agent, p50 at most 10%
  higher; on SQLite, lands/min not lower than flag off. If the first line fails, the flag stays
  off and the stage is reported as not met.

### Stage 3: default on (0.5 day, after a release with the flag)

- Gate: the stage 2 gate repeated on the release tree, no new failure in the e2e and conformance
  suites over the release cycle, Federico's decision.

### Stage 4 (only if needed): per-reference admission (2 days)

- Only if `graft_storage_changesets_total{result="conflict"}` from DB-first writes shows retries
  that cost throughput (non-batched writers on one branch). A node-local FIFO per (repository,
  reference) in front of the DB-first commit, so that same-branch writers of one node do not
  race in the database.
- Gate: the conflict count of the targeted benchmark halves, lands/min not lower.

## 9. Migration and rollback

- No database migration, no change to the changeset format or the bucket. A node with the flag
  and a node without it write the same rows.
- Turning it on: set `db_first_writes = true` and restart the node.
- Turning it off: set it to false and restart. DB-first changesets not yet applied when the node
  stopped are caught up by the gate on the next request, as any other node's changesets.
- Downgrade to a release without the flag: nothing to undo.

## 10. Out of scope

- Pushes off git's lock (section 6), and a patch of the bundled server git.
- The upstream git fix of rejected updates in the reference-transaction hook's input.
- Repository backends other than reftable (authoritative mode serves reftable only).
- Transactions over several repositories; per-branch logs or per-branch causal order (Pando's
  model; invariant 2).
- Praefect (authoritative mode requires `standalone = true`).
- Making the database's per-repository sequence faster (250-300 commits/s); it is not the bound
  until about 5 times today's write rate.

## 11. Risks

- **Poison changesets.** A value that passes the checks of 2.3 but that git's `update-ref`
  refuses would stop every node's catch-up of the repository. Mitigation: the table of 2.3 is
  derived from what `update-ref` checks for a plain object update, each row has a test, and the
  write path keeps git's own `update-ref` (in the catch-up) as the only writer of references, so
  any gap shows up as a failed apply in tests, not as a silent difference.
- **The apply round costs more than a fresh check.** Today most requests of a single node find
  the copy fresh (under 2 ms). With DB-first, requests that arrive during writes run a catch-up
  round (a few database reads, one `for-each-ref`, one `update-ref`), shared by every caller of the
  round. The 1-agent and 50-repository gates of stage 2 catch a regression.
- **The phantom push update becomes reachable on one node** (section 4). Stage 1b is a
  prerequisite of stage 2.
- **Housekeeping and unreachable objects.** Between the quarantine migration and the apply, a
  write's objects are unreachable in the local repository. Pruning only removes unreachable
  objects older than its grace (far longer than an apply), and the bucket holds the pack: a
  missing object makes the round download it.
- **The estimates of section 7 may be wrong** because the split of the hold is unknown. Stage 0
  comes first, and stage 2 has its own gate.

## 12. Alternatives considered

| Alternative | Keeps the invariants | Expected gain | Cost | Decision |
|---|---|---|---|---|
| Shorter hold (stage 1a) | yes | 15-40% on one repository (estimate) | small | adopt first |
| Group commit at git's lock: the admission holder writes the waiters' updates in one `update-ref` and one database transaction | yes, for server-driven writes only | similar to DB-first | as large as DB-first, and a failing member fails the group's git transaction unless every member is checked first; the lock is held for the whole group's database work | rejected: same scope as DB-first with worse failure isolation |
| Group commit in the database (several changesets per transaction) | yes | none now: the database is not the bound (250-300/s against 48/s) | medium | rejected now; revisit if DB-first makes the sequence the bound |
| Per-branch admission without DB-first | yes | none: reftable has one lock per repository, admission only orders writers in front of it | small | rejected |
| Files reference backend (one lock per reference) | no: authoritative mode requires reftable (REFTABLE RULE) | unknown | large | rejected |
| Several git repositories per repository (by branch) | no: stock clients see one repository, one log | large | very large | rejected |
| Pushes DB-first (pre-receive or Preparing commit) | no: clients told the wrong outcome (section 6) | pushes only | medium | rejected |
| Per-branch logs, Pando-like coordination | no: invariant 2 | large | very large | rejected |
| Compaction out of the write path | yes | measured at 1.8 ms at 10 writers, worse at 50 (STORAGE.md) | small | rejected before |
| Answer on the database commit without waiting for the apply | yes (the gate gives read-your-writes, TLC checks it) | slightly lower latency than 2.1 | none | not chosen: post-receive hooks, the observer and the batcher's next batch would see the old tip; may be revisited with numbers |

DB-first is the only option that removes git's lock from the server's own writes while keeping
one log, the database as truth and stock git; stage 1a is the cheap part of the same gain, and
goes first.

## 13. The TLA+ model

[dbfirst.tla](dbfirst.tla) models one repository on one node: the database log with its
compare-and-swap and land identities, the local copy with its watermark and noted sequences,
git's lock (the reftable lock with the admission in front of it), three kinds of writers
(server-driven, non-atomic push, another node), the request gate, the catch-up applier, a
process crash and a power loss. Names, objects, policy and compaction are abstracted (see the
module's header). Values are unique (a value is the writer that wrote it), so a lost update or a
reverted reference is visible in the state.

Invariants: `LogDense`, `LocalNotAhead`, `WatermarkTruthful`, `AckedDurable`, `NoLostUpdate`,
`NoPhantom`, `ExactlyOnceLand`, `ReadYourWrites`; liveness: `EventuallyConsistent` (once the
writers are done, the copy equals the database), under weak fairness.

Run (TLC 2.19 from `tla2tools.jar` v1.7.4, Java 25, `-deadlock` because finished runs stop):

```shell
java -cp tla2tools.jar tlc2.TLC -deadlock -workers 4 -config dbfirst.cfg dbfirst.tla
```

Results on 2026-11-05 (one crash allowed unless said otherwise):

| Configuration | Model | Expected | Result |
|---|---|---|---|
| `dbfirst.cfg` | DB-first, every guard, scenario S1 | no violation | none; 56,629 states, depth 21, liveness holds |
| same, `MaxCrashes = 2` | | no violation | none; 97,839 states |
| `dbfirst_current.cfg` | `main`, scenario S1 | no violation | none; 43,751 states, liveness holds (76,286 states with two crashes) |
| `dbfirst_creates.cfg` | DB-first, a push and an RPC create one branch | no violation | none; 286 states |
| `dbfirst_bug_staleupdate.cfg` | DB-first without stage 1b | `NoPhantom` | violated after 9 steps (and `EventuallyConsistent` fails) |
| `dbfirst_bug_multinode.cfg` | `main`, two nodes, no stage 1b | `NoPhantom` | violated after 12 steps: the open bug exists today |
| `dbfirst_bug_revert.cfg` | catch-up without the never-revert rule | `WatermarkTruthful` | violated |
| `dbfirst_bug_stalecreate.cfg` | `main` before `02fbb52` | `NoLostUpdate` | violated: the bug the stop-gap fixed |
| `dbfirst_bug_cas.cfg` | DB-first without the database compare-and-swap | `NoLostUpdate` | violated (without that invariant, `ExactlyOnceLand` is) |
| `dbfirst_bug_localfirst.cfg` | git writes before the database commit | `LocalNotAhead` | violated |

Scenarios: S1, two server-driven writers land the same change on `b1` (a client retry), a
non-atomic push on `b1`, another node writing `b2`; S2, two pushes on `b1` while another node
writes `b2`; S3, a push and a server-driven write both create `b2`; S4, a push on `b1` while
another node writes `b1`. The model is small (2 branches, up to 4 writers, 1-2 crashes): it finds
interleaving bugs of the write order, not performance problems, and it says nothing about the
abstracted parts (names, objects, policy).

## 14. Open questions for Federico

1. Answer DB-first writes after the local apply (proposed, keeps today's behaviour for hooks
   and the batcher), or on the database commit (a few ms faster, the gate still gives
   read-your-writes)?
2. Stage 1b refuses a whole non-atomic push in one more case (an explicit old value that this
   node's copy does not have). Acceptable, as for the stale create, until git stops feeding
   rejected updates to the hook? And should we propose that fix upstream?
3. Pushes stay on git's lock. Is a fleet of stock-git agents pushing to one repository a case
   we must serve faster, or is the RPC path the one that matters?
4. Per-branch admission is dropped unless stage 2's conflict counts call for it. Agreed?
5. The criteria to turn the flag on by default (stage 3).
