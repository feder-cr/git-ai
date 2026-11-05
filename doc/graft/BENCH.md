# Benchmarking agent landings (graft-bench)

`graft-bench` measures how a Graft server copes with many agents landing
changes on the same branches. It uses the public SDK, so it exercises the same
RPCs as an agent.

## Current results

These are the results of the current code, newest first. Older results, which the
changes since replaced, are under [History](#history-superseded) at the end.

The headline of the [README](../../README.md#benchmark), 50 agents landing 5
changes each on one branch with 20% planned conflicts (seed 1), measured with
`acceptance-phase4.sh` on one machine. Measurement of record: two runs on
2026-10-20 on commit `bf95897` (the 0.3.0 tree), every start at load below 2.
It has not been repeated for 0.4.0.

| 50 agents, 250 changes | classic (batching off) | gitai (batching on) |
|---|---|---|
| changes landed | 100-106 | 200 |
| attempts per landed change | 26.5-28.7 | 1.0 |
| lands per minute | 264-385 | 1,487-1,944 |
| p99 latency | 4.4-5.2 s | 1.9-2.7 s |

An earlier measurement of the same script, two runs on 2026-10-19 on the 0.2.0
tree, was the README headline until the 0.3.0 release and is superseded by the
table above. It gave classic 100-101 landed at 27 attempts each, 240-272 lands
per minute and p99 6.3-7.0 s, and gitai 200 landed at 1.0 attempts, 1,862-2,544
lands per minute and p99 1.5-1.8 s. The two sets differ in both columns, so
part of the difference is the machine and its load, not only the code; the
cause was not investigated, and only the later set is current.

### Default bundled git 2.56.0 (2026-11-05)

The default bundled git (`GIT_VERSION_PREV`, the `-prev` build that serves every non-MVCC
repository unless the `git_master` feature flag is on) moved from 2.55.0-rc1 (`1ea786d`) to the
release 2.56.0 (`a018953`). Window of record: arm A `edbedea` (git 2.55.0-rc1), arm B `edbedea` with
the new pin (git 2.56.0, checked in the running server), one server, postgres, authoritative storage,
batching on, `--mode land`, 50 agents x 20 changes on one branch, conflict rate 0.2, seed 1, the
workload of the 0.4.1 release benchmark. Six series per arm in the order A B A B ..., each started
with CPU pressure (PSI `some avg10`) below 5 and the CPU at least 90% idle over 5 s; every series
was valid (the same gate held 60 s after it).

| series | A p99 (ms) | B p99 (ms) | A lands/min | B lands/min |
|---|---:|---:|---:|---:|
| 1 | 4073 | 4781 | 932 | 893 |
| 2 | 4963 | 4213 | 794 | 836 |
| 3 | 3748 | 4691 | 906 | 827 |
| 4 | 4518 | 3943 | 884 | 922 |
| 5 | 3756 | 4442 | 968 | 900 |
| 6 | 3989 | 3313 | 888 | 981 |

Median p99 4031 ms (A) and 4328 ms (B), +7.4%, inside the gate declared before the run (B at most
10% above A); means 4174 and 4230 ms (+1.3%). Throughput, admission wait (88 ms mean on both) and the
Prepared phase (21 ms mean on both) did not move. The run-to-run spread of p99 (3.3 to 5.0 s) is
larger than the difference between the arms.

### `UserCommitFiles` in fewer git processes (WP96)

WP96 ([PLAN9 section 15](PLAN9.md#15-wp96-the-git-processes-of-usercommitfiles-design-for-approval))
cut the git processes of a `UserCommitFiles` request with one file from 10 to 15 to
6. Window of record: before `d584aac`, after `9db0ac7`, 50 agents of 20 changes each
(1000 calls per run), runs ordered before, after, after, before, twice, each started
once the machine was idle (load below 2). Medians of the four runs on each side:

| | lands/min | p50 (ms) | p95 (ms) | p99 (ms) | server mean (ms) |
|---|---:|---:|---:|---:|---:|
| before | 2134 | 217 | 486 | 600 | 237 |
| after | 2185 | 177 | 358 | 444 | 186 |

Every before/after pair is lower after in p50, p95 and p99. The per-run table and
where the time went are in PLAN9 section 15.4.

### The write path (PLAN9)

PLAN9 ([PLAN9.md](PLAN9.md)) changed the write path that every reference update
shares: the changeset log records the transactions that commit together with one
fsync (group commit, in ticket order), and the quarantine migration fsyncs each
directory once instead of every file. The window of 2026-10-19 measured
`UserCommitFiles`, the RPC that pays for both, with the per-RPC latency of
`graft-bench` (`rpcs`), against a baseline built from the same history without
PLAN9 (`b6058e5` + `6aecff1`, the tree of v0.2.0's land code), runs interleaved
base, p5w, base, p5w, each one server with batching on, `--mode land`, 50 then 100
agents of 5 changes, conflict rate 0.2, seed 1, on the same machine (load 0.4 at the
start, 12.7 at the end). The p5w tree is `64b29bb`.

Server-side mean of `UserCommitFiles` (`grpc_server_handling_seconds`), and the
client-side percentiles:

| run | agents | mean (ms) | p50 (ms) | p95 (ms) | p99 (ms) | lands/min |
|---|---:|---:|---:|---:|---:|---:|
| base 1 | 50 | 594 | 466 | 1152 | 1259 | 1427 |
| p5w 1 | 50 | 433 | 379 | 1440 | 1517 | 1342 |
| base 2 | 50 | 775 | 589 | 1420 | 4268 | 833 |
| p5w 2 | 50 | 430 | 349 | 882 | 927 | 1877 |
| base 1 | 100 | 455 | 339 | 1273 | 1458 | 1499 |
| p5w 1 | 100 | 367 | 295 | 1061 | 1089 | 1876 |
| base 2 | 100 | 642 | 404 | 2116 | 2717 | 816 |
| p5w 2 | 100 | 426 | 322 | 1098 | 1146 | 1481 |

The committed phase of a transaction (`graft_changeset_ticket_wait_seconds`, from the
committed hook to its entries being durable) averaged 22 to 27 ms, with 2.5 to 3.1
transactions per group write and 13 to 18 ms per write; before PLAN9 the profile
measured a ticket wait of 92 to 99 ms plus a 7 ms append.

Against the acceptance of PLAN9 (section 11):

| criterion | result |
|---|---|
| mean at least 150 ms lower at 50 agents | met: 684 -> 432 ms on average (-252); per pair -161 and -344 |
| mean at least 100 ms lower at 100 agents | met on average: 548 -> 396 ms (-152); per pair -88 and -216 |
| p99 not higher than the baseline | not met in every pair: p5w 1 at 50 agents 1517 ms against base 1 1259 ms; lower in the other three pairs |
| committed phase at most 20 ms mean | not met: 22 to 27 ms |
| `acceptance-phase4.sh` twice on the p5w tree | A1, A2, A3 and the stretch goal pass in both runs (p99 on-100 3397 and 2338 ms against 0.5 x off of 4527 and 3817 ms) |
| lands/min not below phase 4b | 1377 / 1184 at 20 agents, 2628 / 2856 at 50, 2066 / 2785 at 100; above the phase 4b window 3 runs at 50 and 100 agents, below them at 20 agents in run 2 (1184 against 1366 and 1476) |

The runs are short (250 and 500 calls per point, 8 to 30 s each) and noisy: base 2
ran at half the throughput of base 1 (833 against 1427 lands/min at 50 agents) as
the load rose, so the per-pair numbers vary more than the effect at 100 agents. The
committed phase misses its target by a few milliseconds: a group write still costs
one fsync of the log (13 to 18 ms under this load) after the transaction's own wait
for the head of the queue. Outputs: `/home/feder/p5w-work/window` (scripts
`window.sh`, `summary.py`, `means.py`).

### Batched landing (phase 4b)

Phase 4b ([PLAN8.md](PLAN8.md)) fixed the two causes of the latency that the
profile of the phase 4a tree found: about 12 fsyncs per batched request (each new
object fsynced when written and again by the quarantine migration) and nine short
git processes per request where one listing of the change's commits answers every
question. It also removed a second git process that the command factory started
for most git commands (`gitcmd` attr.tree detection), which speeds up every
command of the server, so the "off" column below is measured on that corrected
base too.

Runs of record: two runs of `acceptance-phase4.sh` in one benchmark window, on
the tree of commit `1899a0d`, with the defaults (`AGENT_COUNTS="20 50 100"`,
`CHANGES=5`, `RATE=0.2`, `SEED=1`, `window = "10ms"`, `max_size = 50`,
`target_latency = "500ms"`, `graph_ancestry = false`). Same machine as above
(Intel Core Ultra 7 255H, 16 logical CPUs, WSL2, ext4 on a virtual disk, an
fsync of a small file takes about 6 ms), with no other workload: the load average
was 4.2 when the first run started and rose to 38 and 48 during the runs, which is
the benchmark itself (both servers and up to 100 agents).

Both runs pass every criterion, the stretch goal included:

| criterion | 20 agents | 50 agents | 100 agents |
|---|---|---|---|
| attempts/landed (on), run 1 / run 2 | 1.00 / 1.00 | 1.00 / 1.00 | 1.00 / 1.00 |
| failed = planned conflicts (on) | 22 / 22 | 50 / 50 | 108 / 108 |
| p99 on vs off (ms), run 1 | 1347 vs 7437 | 2525 vs 12030 | 4854 vs 10077 |
| p99 on vs off (ms), run 2 | 1193 vs 6741 | 4839 vs 12746 | 2897 vs 11905 |
| stretch: p99 on-100 <= 0.5 x off-100 | | | 4854 <= 5038 (run 1), 2897 <= 5952 (run 2) |

The stretch goal passed by 3.7% in run 1 and by 51% in run 2.

| metric (batching on), run 1 / run 2 | 20 agents | 50 agents | 100 agents |
|---|---|---|---|
| lands/min | 1020 / 1070 | 1230 / 944 | 1616 / 2325 |
| lands/min, phase 4a run of record | 412 | 251 | 258 |
| p50 landed (ms) | 911 / 739 | 1700 / 1980 | 2653 / 1824 |
| batch size mean | 8.2 / 8.3 | 13.4 / 8.9 | 13.9 / 18.5 |
| duration | 4.6 s / 4.4 s | 9.8 s / 12.7 s | 14.6 s / 10.1 s |

With batching off the runs took 17 to 41 s and landed 61 to 116 changes, as before:
the off column still loses most lands to the 20-attempt limit at 50 and 100 agents.

Time per phase, mean per observation, run 1 / run 2:

| phase | 20 agents | 50 agents | 100 agents |
|---|---|---|---|
| land_member (per request) | 8.5 / 7.6 ms | 7.4 / 8.5 ms | 7.5 / 8.1 ms |
| plan (per request) | 3.3 / 3.1 ms | 3.1 / 3.3 ms | 3.1 / 3.6 ms |
| replay (per rebased request) | 4.8 / 4.2 ms | 3.9 / 4.8 ms | 4.1 / 4.2 ms |
| pack (per batch) | 55 / 47 ms | 75 / 69 ms | 53 / 45 ms |
| update (per batch) | 126 / 136 ms | 212 / 210 ms | 175 / 135 ms |
| total (per batch) | 238 / 233 ms | 351 / 335 ms | 321 / 324 ms |

Reading it:

- A batched request costs about 8 ms instead of 80 to 137 ms in phase 4a: one
  `git rev-list` for the decision and a replay through the long-lived merge-tree
  and hash-object processes, without fsync per object.
- The lane is still busy for most of a run, but each batch now spends most of its
  time publishing: packing the objects of the batch and the update (the
  compare-and-swap, the hooks and the changeset log). At 100 agents the publish
  is about 70% of a batch. The update took 135 to 212 ms here, against 20 to 30 ms
  (without the quarantine migration) in a profile at lower load; why it grew under
  load is being profiled.
- The variance between runs is large: p99 at 50 agents was 2525 ms in run 1 and
  4839 ms in run 2, p99 at 100 agents 4854 and 2897 ms. The benchmark saturates
  the machine (load average 38 to 48 during the runs), so single runs are not
  precise; compare runs of one window, and rerun before comparing across trees.
- The stretch goal passed with a margin of only 3.7% in run 1.
- The off column moved too. At 100 agents its p99 (10077 and 11905 ms) is lower
  than in the phase 4a run of record (13368 ms); at 50 agents it is higher (12030
  and 12746 ms against 9939 ms). One known change applies to it: every git command
  of the server now starts one process instead of two for most commands. Why the
  off p99 at 50 agents rose is not explained; its p99 covers only the 97 changes
  that landed, out of 250.

#### Final tree (window 3)

After window 1 the unbatched land was moved onto the same landing attempt as a
batch (PLAN8 WP86). A second window (tree `59d8fda`) passed every criterion but
showed a regression of the "off" column at 100 agents (p99 33.9 and 34.8 s, 60
lands/min): every unbatched attempt published its objects as a pack, and under
the contention of 100 agents about 98% of the ~6600 attempts lose the
compare-and-swap and each left a pack in the repository (120 packs at the end of
the run against 4 in window 1, `pack-objects` 1484 s and `update-ref` 840 s of
real time against 1 s and 56 s). Unbatched lands now publish their few objects
loose (PLAN8 section 13a). The runs of record of the final tree, commit
`9bf4742`, are window 3 (same workload and defaults as above; load average 10.7
at the start, 53 and 57 at the end of the runs):

| criterion | 20 agents | 50 agents | 100 agents |
|---|---|---|---|
| attempts/landed (on), run 1 / run 2 | 1.00 / 1.00 | 1.00 / 1.00 | 1.00 / 1.00 |
| failed = planned conflicts (on) | 22 / 22 | 50 / 50 | 108 / 108 |
| p99 on vs off (ms), run 1 | 819 vs 4707 | 1880 vs 8267 | 4640 vs 14641 |
| p99 on vs off (ms), run 2 | 748 vs 4813 | 9171 vs 17868 | 5390 vs 26692 |
| stretch: p99 on-100 <= 0.5 x off-100 | | | 4640 <= 7321 (run 1), 5390 <= 13346 (run 2) |

| metric, run 1 / run 2 | 20 agents | 50 agents | 100 agents |
|---|---|---|---|
| lands/min, batching on | 1366 / 1476 | 1690 / 620 | 1459 / 1678 |
| lands/min, batching off | 332 / 336 | 230 / 151 | 158 / 114 |
| p50 landed on (ms) | 599 / 639 | 1326 / 1794 | 2927 / 2537 |
| member (land_member, mean) | 10.8 / 11.1 ms | 14.9 / 21.1 ms | 14.2 / 13.1 ms |
| pack per batch (mean) | 39 / 34 ms | 51 / 210 ms | 62 / 50 ms |
| update per batch (mean) | 65 / 52 ms | 89 / 155 ms | 114 / 107 ms |

All criteria pass in both runs, the stretch goal included (by 37% and 60%). The 50
agents of run 2 are an outlier on both sides (pack 210 ms and update 155 ms per
batch, p99 on 9171 ms, lands/min 620): the load average was above 40 during that
part of the run. The off column at 100 agents recovered from window 2 (p99 14641 and
26692 ms, 158 and 114 lands/min; window 1: 10077 and 11905 ms, about 170 lands/min)
and its repository ended with 3 packs and 232 loose objects, but it still varies a
lot between runs and was slower than in window 1 in run 2, for a reason not
established.

## What it does

- Starts `--agents` concurrent agents (goroutines). Each agent commits
  `--changes` changes on private branches, then lands each one on its target
  branch.
- With `--branches N` the agents are spread round robin over N target
  branches (agent `i` lands on branch `i mod N`), so each branch has its own
  contention. The default is one branch shared by every agent.
- Each change adds its own file, so changes never conflict by path.
- A fraction of the changes (`--conflict-rate`) also rewrites a shared file
  by appending one line at the same place. Two such changes made from the same
  base conflict when the second one lands on the same branch.
- The change plan is deterministic for a given `--seed`, so runs are
  comparable.

## Modes

| Mode | What is measured |
|------|------------------|
| `land` | `UserLand` with rebase. A conflicting change fails with `merge_conflict`. |
| `land-resolve` | `UserLand` with `resolve_conflicts`. Conflicts are resolved by the server's resolver. Requires the resolver configuration of [RESOLVE](RESOLVE.md). |
| `land-batch` | The same requests as `land`, run against a server that batches concurrent lands of a branch (see [LAND-BATCHING](LAND-BATCHING.md)). The mode only names the run: whether lands are batched is decided by the server's `[graft.land.batch]`. The batch of every land is reported in any mode. |
| `queue` | A simulated serialized merge queue. Changes land one at a time, and each is held for `--test-duration` (a simulated test) before the next starts. Latency includes waiting in the queue. |

## Metrics

`--format json` and `--json-out` write these fields. The table output and the
Markdown report show the same values.

| Field | Meaning |
|-------|---------|
| `label` | The `--label` of the run. Omitted when the run has none. |
| `mode`, `agents`, `changes_per_agent`, `branches`, `conflict_rate`, `test_duration_ms` | The configuration of the run. |
| `changes` | Changes attempted (agents times changes per agent). |
| `landed` | Changes that reached the branch, including already landed ones. |
| `already_landed` | Changes that were already reachable from the branch. |
| `failed`, `conflicts` | Changes that did not land, and how many of them were merge conflicts. |
| `observed_conflict_rate` | `conflicts` divided by `changes`. |
| `resolutions` | Conflicting paths the server resolved. |
| `attempts` | Compare-and-swap attempts the server used, summed over every change, landed or failed. A land that gave up because the branch kept moving counts the attempts it made (the number the server reports in its "giving up after N attempts" error). Other failed lands count none, because the server does not report their attempts. Above `landed` means the branch moved during lands. |
| `attempts_per_land` | `attempts` divided by `landed`. The attempts wasted on failed changes raise it, so it is the server work per landed change. |
| `unavailable_retries` | Calls that the server answered with `Unavailable` and the SDK retried (`LandOptions.RetryUnavailable`), summed over the landed changes. Each is a whole land request again, the conflict resolver included. A failed land reports none. The benchmark lands with rebase, which the SDK does not retry by default, so it is 0 until rebase lands are retried. |
| `max_attempts` | The largest number of attempts of any change, landed or failed. |
| `attempts_histogram` | Landed changes by their number of attempts (`{"1": 95, "2": 5}`: 95 changes landed at the first attempt). Landed changes that report no attempts are left out. |
| `failed_attempts_histogram` | Failed changes that report attempts, by their number of attempts. A land that gave up after `max_attempts` rounds shows up here. |
| `batch_size_mean`, `batch_size_max`, `batch_size_histogram` | The size of the batch each landed change landed in, as the server reports it in `batch_size` of `UserLandResponse`. A server without batching reports 1 for every land. Landed changes without a reported batch (an older server) are left out; `batch_size_max` is then 0. Failed changes are not counted. |
| `duration_seconds` | Wall-clock duration of the run. |
| `lands_per_minute` | `landed` divided by the wall-clock duration of the run. |
| `p50_ms`, `p95_ms`, `p99_ms` | Latency of the LANDED changes, from the start of the commit to the end of the land (nearest rank). Failed changes are not included. |
| `failed_p95_ms` | p95 latency of the failed changes, measured the same way. 0 when none failed. |
| `rpcs` | The latency of each RPC as the client measures it around the call: `commit_files` (the `UserCommitFiles` of every change) and `land` (the `UserLand` of every change that was committed, without the wait of queue mode). Each has `calls`, `failed`, and `p50_ms`, `p95_ms`, `p99_ms` of the calls that succeeded (nearest rank). A land that fails on a conflict is a failed call. |
| `failure_classes` | Failed changes counted by the kind of their error message, with commit IDs removed, so the same conflict on different commits is one kind. |
| `errors` | Up to five distinct kinds of failure, each as the first full message of its kind. |

Latency includes the commit, so it measures what an agent waits for. It does
not include setup of the branch. The latency percentiles cover landed changes
only: the time a failed change spent before it failed is not the time a landed
change takes. Failed changes are reported separately by `failed`,
`failure_classes` and `failed_p95_ms`.

## Usage

Every run needs branches of its own: give each run a fresh `--branch`.

```sh
graft-bench --address unix:/var/lib/graft/run/gitaly.socket \
    --repo bench.git --branch bench-land-1 --agents 50 --changes 10 \
    --conflict-rate 0.2 --mode land --token "$GRAFT_TOKEN"

graft-bench --address unix:/var/lib/graft/run/gitaly.socket \
    --repo bench.git --branch bench-queue-1 --mode queue \
    --test-duration 200ms --format json --token "$GRAFT_TOKEN"

graft-bench --address unix:/var/lib/graft/run/gitaly.socket \
    --repo bench-new.git --create-repo --branch bench-land-1 --mode land \
    --token "$GRAFT_TOKEN"

graft-bench --address unix:/var/lib/graft/run/gitaly.socket \
    --repo bench.git --branch bench-batch-1 --branches 4 --agents 100 \
    --changes 5 --mode land-batch --label batching-on-100 \
    --json-out on-100.json --md-out on-100.md --token "$GRAFT_TOKEN"
```

Flags:

- `--address`: `unix:<path>`, `tcp://host:port` or `tls://host:port`. Required.
- `--repo`: relative path of the repository on the storage. Required.
- `--branch` (default `bench`): the branch the changes land on. It is not
  `main` by default, because a policy usually protects `main` (the example
  policy of `examples/graft/config.toml` lets only `planner` update it), and
  then every land is denied. The actor of the lands is the identity of the
  token (see [CONFIGURATION](CONFIGURATION.md)), so the policy must let it
  update this branch. With `--branches` above 1 it is the prefix of the branch
  names, see below.
- `--branches` (default 1): the number of target branches. With more than one,
  the branches are named `<branch>-00`, `<branch>-01`, and so on, and the
  agents are spread over them round robin. It must not exceed `--agents`.
- `--agents` (default 50), `--changes` (default 10 per agent),
  `--conflict-rate` (default 0, from 0 to 1).
- `--mode` (default `land`): `land`, `land-resolve`, `land-batch` or `queue`.
- `--test-duration`: the simulated test time per change, `queue` mode only.
- `--storage`, `--seed` (default 1), `--max-attempts` (default 0, the server
  default).
- `--token` (default `$GRAFT_TOKEN`) or `--agent-token` (default
  `$GRAFT_AGENT_TOKEN`). Use an agent token to measure what an agent sees. A
  flag overrides the other variable. With no flag, `$GRAFT_TOKEN` is used when
  both variables are set. Passing both flags is an error.
- `--create-repo`: creates the repository first, through the server's
  `RepositoryService` (never by writing into the storage directory). It needs
  the shared `--token` and fails if the repository exists.
- `--timeout`: bounds the whole run (default 10m).
- `--format table` (default) or `json`: the output on stdout.
- `--label NAME`: names the run in reports, such as `batching-on-50`. It is the
  `label` field, the title of the Markdown report and the column header of
  `graft-bench compare`. It must not contain `|` or newlines.
- `--json-out FILE`: also writes the result as JSON (the `--format json`
  object) to FILE.
- `--md-out FILE`: also writes the result as a Markdown report to FILE: a
  `## graft-bench: <label>` heading (the mode when there is no label), the
  metrics as a table, and the failed changes by kind.

Each target branch is created on first use with an initial commit that holds
the shared file. A run adds a file per change under `agents/`, so a second run
on the same branch would fail every commit: `graft-bench` refuses a branch
that holds `agents/` and exits 1 with "every run needs a fresh --branch". A
repository can be reused with a fresh branch name, which also keeps the
numbers comparable. A branch that exists without `agents/` (for example one
created beforehand) is used as it is.

A run cut short by `--timeout` or an interrupt prints the samples it has,
writes the `--json-out` and `--md-out` files, and exits 1 with the reason on
stderr. Its numbers are partial and must not be reported as a complete run.

## Comparing runs

`graft-bench compare` prints saved JSON results (from `--json-out` or
`--format json`) side by side, one column per file:

```sh
graft-bench compare --format markdown off-50.json on-50.json
```

- `--format table` (default) prints an aligned text table, `--format
  markdown` a Markdown pipe table.
- Each column is headed by the `label` of the result, or by the file name when
  the result has no label.
- The rows are the mode, agents, branches, changes, landed, failed, conflict
  rate, attempts (all changes), attempts per landed change, max attempts (all
  changes), the attempts histograms of landed and of failed changes, the batch
  size mean, max and histogram, duration, lands per minute, latency p50, p95
  and p99 of the landed changes, and latency p95 of the failed changes.

It exits 2 without a file or with an unknown format, and 1 when a file cannot
be read.

## Demo

`examples/graft/demo/demo.sh` runs the `land`, `land-resolve` and `queue`
modes with 50 agents and prints a comparison table. See the header of the
script for the setup it needs.

`land-resolve` only resolves conflicts when the server has a resolver for the
shared file. The demo ships `examples/graft/demo/resolve.toml`, which unions
the two sides of `shared/log.txt` and tries `trivial` first. The header of
`demo.sh` gives the `[graft.land.resolver]` snippet that enables it. Without
that configuration the `land-resolve` row matches the `land` row.

## Batching benchmark

`examples/graft/demo/bench-batch.sh [WORKDIR]` measures what batched landing
(see [LAND-BATCHING](LAND-BATCHING.md)) changes:

1. It builds `gitaly` (with `make build`) and `graft-bench`, and writes tokens.
2. It starts two standalone Gitaly servers from `examples/graft/config.toml`
   that differ only in `[graft.land.batch] disabled` (`true` for "off",
   `false` for "on"), each with its own repositories and unix socket under
   WORKDIR.
3. Against each server it runs `graft-bench --mode land` with 20, 50 and 100
   agents of 5 changes each, conflict rate 0.2 and seed 1, on a fresh branch
   per run (the first run creates the repository with `--create-repo`). Each
   run is labelled `batching-<off|on>-<agents>` and saved with `--json-out`
   and `--md-out`.
4. It prints the combined `graft-bench compare --format markdown` table of all
   runs and saves it as `combined.md`.
5. It stops both servers when it exits, also on failure.

WORKDIR defaults to a fresh temporary directory; the results are in
`WORKDIR/results`. The environment variables `AGENT_COUNTS` (`"20 50 100"`),
`CHANGES` (5), `RATE` (0.2) and `SEED` (1) change the workload. The script needs
make, go, openssl and the Gitaly build dependencies (see
`examples/graft/quickstart.sh`).

Read the combined table column by column: the off and on runs of one agent
count share the workload, so `attempts/landed change`, the attempts
histograms, `max attempts (all changes)` and the landed latency show what
batching saves, and the batch size rows show how many lands shared an update.
Because the attempts of lands that gave up count in both columns, the off
column is not flattered by its failures.

### Phase breakdown

Each server exposes its metrics on its own `prometheus_listen_addr` (set by
`bench-batch.sh`). After each run the script scrapes `/metrics` and appends a
"time per phase" table to the results. For each phase of a batch attempt
(`graft_land_batch_phase_seconds{phase}`: `quarantine`, `tip`, `land_member`,
`plan`, `replay`, `pack`, `update`, `total`) it shows the sum and the mean, and
the mean `land_member` time per batch. `plan` and `replay` are parts of
`land_member`; `pack` precedes `update`. The metric names are in [LAND-BATCHING](LAND-BATCHING.md#metrics).

### Acceptance run (`acceptance-phase4.sh`)

`examples/graft/demo/acceptance-phase4.sh [WORKDIR]` runs `bench-batch.sh` with
the same workload (20, 50 and 100 agents, 5 changes each, conflict rate 0.2,
seed 1) and checks the acceptance criteria of
[PLAN4](PLAN4.md#acceptance-phase-4a) against the JSON results:

| Criterion | Check on the batching-on runs (and the off runs for p99) |
|---|---|
| Attempts | `attempts_per_land` is 1.00 at every agent count |
| Failures | `failed` equals `conflicts`: only the planned conflicts fail |
| Latency | `p99_ms` on is at most `p99_ms` off, at every agent count |
| Stretch (reported, not required) | `p99_ms` on at 100 agents is at most half of off |

Each criterion prints `PASS` or `FAIL`. The script exits non-zero when one fails,
and it prints the stretch result as `STRETCH PASS` or `STRETCH MISS`. The
`AGENT_COUNTS`, `CHANGES`, `RATE` and `SEED` variables of `bench-batch.sh` are
passed through.

`acceptance-phase4.sh --dry-run WORKDIR` runs only the check, on the JSON files
that a previous run left in `WORKDIR/results`; it builds and starts nothing. Use
it to re-check saved results. The script keeps `WORKDIR/results` (the JSON, the
Markdown, the Prometheus scrapes and `combined.md`). On exit, also when a run is
interrupted, it stops any Gitaly server of `WORKDIR` that is still running and
deletes the agent tokens in `WORKDIR/tokens`.

## Reading the numbers

- Compare `land` and `land-resolve` at the same `--conflict-rate`. The first
  shows the cost of conflicts as failures, the second their cost as latency.
- `attempts` close to `landed` means little contention on the branch tip.
  High values mean many retries, so increase `--max-attempts`, reduce the
  number of agents landing on one branch (`--branches` shows the effect), or
  enable batching.
- A `failed_attempts_histogram` entry at the `max_attempts` of the server
  means lands gave up because the branch kept moving.
- `batch_size_mean` above 1 means concurrent lands shared one reference
  update. With batching disabled every land reports 1.
- In `queue` mode, `p95` and `p99` grow with the number of changes ahead in
  the queue, times `--test-duration`. That is the cost of serialization, which
  the rebase-based land avoids for changes that do not conflict.
- Results depend on the machine, the storage and whether hooks run. Report
  the full JSON output with any number you quote.

## Limits of the benchmark

- Each target branch is shared by the agents assigned to it. Changes on
  different branches never conflict with each other, so `--branches` measures
  independent contention, not cross-branch interaction.
- The simulated test in `queue` mode does not run the tests. Use
  `graft-verify` for real post-merge checks.
- The shared file conflicts by design. Real repositories conflict less often,
  so the conflict rate is a stress knob, not a forecast.
- The attempts of a failed land are known only when it gave up because the
  branch kept moving. Other failures (merge conflicts, hook rejections) report
  no attempts.


## History (superseded)

These results describe code that has since changed. Phase 4b (PLAN8) superseded
the phase 4a results: it removed the per-object fsyncs and the extra git processes
of a batched land, and its runs pass every acceptance criterion (see
[Batched landing (phase 4b)](#batched-landing-phase-4b)).

### Phase 3 baseline

The table from PLAN4, measured with batching on and off:

| metric | off-20 | on-20 | off-50 | on-50 | off-100 | on-100 |
|---|---|---|---|---|---|---|
| attempts/landed | 10.97 | 1.00 | 29.35 | 1.00 | 56.70 | 1.00 |
| p50 landed (ms) | 954 | 3320 | 5484 | 8152 | 5703 | 15950 |
| p99 landed (ms) | 5049 | 9868 | 17669 | 15317 | 16775 | 21808 |

### Measured phase 4a results

Run of record: `acceptance-phase4.sh /tmp/graft-acc4-final3` on the final phase
4a tree (all work packages and fixes merged), with the defaults
(`AGENT_COUNTS="20 50 100"`, `CHANGES=5`, `RATE=0.2`, `SEED=1`,
`window = "10ms"`, `max_size = 50`, `target_latency = "500ms"`,
`graph_ancestry = false`). Machine: Intel Core Ultra 7 255H, 16 logical CPUs,
WSL2 (kernel 6.6.87.2), ext4 on a virtual disk, git 2.53.0. Other builds and
tests ran on the same machine at the same time (load average 8 to 38 during the
run), so the numbers carry noise. Quote them with the machine, and rerun before
comparing.

Result:

```
PASS attempts/landed = 1.00 with batching on, 20 agents (landed 78)
PASS failures are planned conflicts only, batching on, 20 agents (failed 22, conflicts 22)
PASS p99 on 2789.0 ms <= off 10022.5 ms, 20 agents
PASS attempts/landed = 1.00 with batching on, 50 agents (landed 200)
PASS failures are planned conflicts only, batching on, 50 agents (failed 50, conflicts 50)
FAIL p99 on 12658.3 ms <= off 9939.0 ms, 50 agents
PASS attempts/landed = 1.00 with batching on, 100 agents (landed 392)
PASS failures are planned conflicts only, batching on, 100 agents (failed 108, conflicts 108)
FAIL p99 on 23252.6 ms <= off 13368.1 ms, 100 agents
STRETCH MISS p99 on-100 23252.6 ms, 0.5 x off-100 6684.1 ms
acceptance-phase4: FAIL
```

**The phase 4a acceptance is not met.** Contention stays solved at every size
(one attempt per landed change, only the planned conflicts fail), and p99 with
batching on is below off at 20 agents, but it is above off at 50 and 100 agents.

The p99 criterion is not stable between runs. All runs below use the same
workload:

| run | tree | p99 on/off 20 (ms) | p99 on/off 50 (ms) | p99 on/off 100 (ms) | result |
|---|---|---|---|---|---|
| `/tmp/graft-acc4` | phase 4a before the final fixes | 4443 / 5914 | 9607 / 9683 | 17635 / 19381 | PASS (50 agents by 76 ms) |
| `/tmp/graft-acc4-final2` | final | 3502 / 6303 | 13235 / 9703 | 24716 / 19848 | FAIL at 50 and 100 |
| `/tmp/graft-acc4-final3` (of record) | final | 2789 / 10023 | 12658 / 9939 | 23253 / 13368 | FAIL at 50 and 100 |

A further run on the final tree, made while other test suites loaded the
machine heavily (some of its runs took up to 8 times longer than usual), is left
out; it failed the p99 criterion at 50 agents. The off
column varies most, because its landed latency depends on random retries, and
its p99 covers only the changes that landed (115 of 500 at 100 agents in the
run of record; 277 gave up after 20 attempts and are not in it).

Combined table of the run of record (`results/combined.md`):

| metric | batching-off-20 | batching-on-20 | batching-off-50 | batching-on-50 | batching-off-100 | batching-on-100 |
| --- | --- | --- | --- | --- | --- | --- |
| mode | land | land | land | land | land | land |
| agents | 20 | 20 | 50 | 50 | 100 | 100 |
| branches | 1 | 1 | 1 | 1 | 1 | 1 |
| changes | 100 | 100 | 250 | 250 | 500 | 500 |
| landed | 62 | 78 | 99 | 200 | 115 | 392 |
| failed | 38 | 22 | 151 | 50 | 385 | 108 |
| conflict rate | 0.220 | 0.220 | 0.200 | 0.200 | 0.216 | 0.216 |
| attempts (all changes) | 706 | 78 | 2766 | 200 | 6548 | 392 |
| attempts/landed change | 11.39 | 1.00 | 27.94 | 1.00 | 56.94 | 1.00 |
| max attempts (all changes) | 20 | 1 | 20 | 1 | 20 | 1 |
| attempts histogram (landed) | 1: 7, 2: 4, 3: 10, 4: 10, 5: 9, 6: 1, 7: 2, 8: 6, 9: 2, 11: 1, 12: 1, 13: 1, 14: 2, 16: 2, 17: 1, 18: 1, 19: 1, 20: 1 | 1: 78 | 1: 8, 2: 15, 3: 9, 4: 9, 5: 6, 6: 5, 7: 9, 8: 3, 9: 2, 10: 3, 11: 1, 12: 6, 13: 6, 14: 2, 15: 3, 16: 3, 17: 4, 20: 5 | 1: 200 | 1: 10, 2: 10, 3: 11, 4: 7, 5: 10, 6: 8, 7: 4, 8: 2, 9: 5, 10: 3, 11: 2, 12: 4, 13: 6, 14: 5, 15: 8, 16: 5, 17: 2, 18: 4, 19: 4, 20: 5 | 1: 392 |
| attempts histogram (failed) | 20: 16 | - | 20: 101 | - | 20: 277 | - |
| batch size mean | 1.00 | 9.86 | 1.00 | 10.95 | 1.00 | 3.80 |
| batch size max | 1 | 18 | 1 | 43 | 1 | 25 |
| batch size histogram | 1: 62 | 2: 12, 3: 11, 4: 12, 6: 2, 12: 12, 17: 14, 18: 15 | 1: 99 | 1: 14, 2: 97, 3: 33, 6: 5, 12: 11, 43: 40 | 1: 115 | 1: 47, 2: 180, 3: 129, 6: 4, 12: 10, 25: 22 |
| duration | 17.51s | 11.36s | 28.02s | 47.82s | 54.49s | 91.30s |
| lands/min | 212.4 | 411.9 | 212.0 | 250.9 | 126.6 | 257.6 |
| latency p50 (landed) | 1518.2ms | 1989.4ms | 2097.0ms | 8958.5ms | 4073.0ms | 17292.1ms |
| latency p95 (landed) | 5786.8ms | 2670.6ms | 7324.4ms | 12475.3ms | 12293.4ms | 22915.5ms |
| latency p99 (landed) | 10022.5ms | 2789.0ms | 9939.0ms | 12658.3ms | 13368.1ms | 23252.6ms |
| latency p95 (failed) | 11338.5ms | 2706.3ms | 9644.9ms | 12466.1ms | 14038.5ms | 22404.3ms |

Time per phase of the run of record (the difference of
`graft_land_batch_phase_seconds` between the scrapes before and after each
run). The runs with batching off execute no batch, so all their rows are zero
and are left out here:

| run | phase | observations | sum (s) | mean (s) |
|---|---|---:|---:|---:|
| on 20 agents | quarantine | 21 | 0.012507 | 0.000596 |
| on 20 agents | tip | 21 | 0.078462 | 0.003736 |
| on 20 agents | land_member | 100 | 7.968227 | 0.079682 |
| on 20 agents | land_member per batch | 21 | 7.968227 | 0.379439 |
| on 20 agents | update | 19 | 3.002550 | 0.158029 |
| on 20 agents | total | 21 | 11.095477 | 0.528356 |
| on 50 agents | quarantine | 99 | 0.059037 | 0.000596 |
| on 50 agents | tip | 99 | 0.370074 | 0.003738 |
| on 50 agents | land_member | 250 | 28.369638 | 0.113479 |
| on 50 agents | land_member per batch | 99 | 28.369638 | 0.286562 |
| on 50 agents | update | 86 | 18.260168 | 0.212328 |
| on 50 agents | total | 99 | 47.216474 | 0.476934 |
| on 100 agents | quarantine | 228 | 0.140545 | 0.000616 |
| on 100 agents | tip | 228 | 0.877662 | 0.003849 |
| on 100 agents | land_member | 500 | 51.281139 | 0.102562 |
| on 100 agents | land_member per batch | 228 | 51.281139 | 0.224917 |
| on 100 agents | update | 215 | 38.124248 | 0.177322 |
| on 100 agents | total | 228 | 90.788043 | 0.398193 |

Reading it:

- Contention stays solved: every landed change took one attempt, and only the
  planned conflicts failed. With batching off, 16, 101 and 277 changes gave up
  after 20 attempts. Batching on lands 1.3 to 3.4 times as many changes, and
  its throughput (lands/min) is higher at every size.
- The lane is saturated: the `total` sum of the batches (11.1 s, 47.2 s, 90.8 s)
  is almost the whole run duration (11.4 s, 47.8 s, 91.3 s). Batches run one
  after another, so a request waits for every batch queued before it, and p50
  and p99 with batching on are mostly time in the queue.
- Landing the members is the dominating phase: `land_member` is 72%, 60% and
  56% of the batch time at 20, 50 and 100 agents, at about 80 to 115 ms per
  member with the streaming rebase engine, and it is serial. The reference
  update with its hooks (`update`, 160 to 210 ms per published batch) is 27%,
  39% and 42%. Opening the quarantine and reading the tip are under 1%.
- Adaptive sizing is not the cause. A diagnostic run of the batching-on server
  alone with `target_latency = "0s"` (no adaptation; mean batch sizes 32.7 and
  49.6 instead of 10.95 and 3.80) gave p99 12128 ms at 50 agents and 20081 ms at
  100 agents, with durations of 52.5 s and 79.5 s: larger batches save update
  phases, but the serial member work stays.
- What phase 4b (pipelined batches) must address: the serial lane. A batch must
  be able to land its members onto the unpublished tip of the batch before it
  while that batch runs its CAS and hooks, which hides the `update` share (about
  40% at 50 and 100 agents). The per-member cost of about 100 ms is the larger
  share and the bound that remains after pipelining, so phase 4b must also make
  it cheaper or parallel (for example, landing members that touch disjoint paths
  concurrently), and break `land_member` down in the metrics (ancestry, rebase,
  commit writes, policy) to find where the 100 ms go.
- Compared with phase 3 (table above), p99 with batching on dropped at 20 agents
  (9868 to 2789 ms) and 50 agents (15317 to 12658 ms), and rose slightly at 100
  agents (21808 to 23253 ms), on a noisy machine.
