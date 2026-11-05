# Post-merge verification and auto-revert (graft-verify)

`graft-verify` watches a branch and checks that its tip passes a test command.
When a batch of landed changes breaks the tests, it finds the first breaking
commit by bisection and reverts it on the branch. It is a separate program
built on the public SDK (`sdk/`). It does not run inside the server and has no
special access to it: it is an ordinary gRPC client that uses the operator's
shared token (see [Permissions](#permissions)).

## Security note: the test command runs code from the tree

The test command runs code that is in the tip, written by agents, and, where
the server's AI lands a resolution because the land's own `verify.command`
passed (see [RESOLVE](RESOLVE.md)), written by a model from repository content
that agents control. Treat both commands as untrusted code: run them in a
container with `--network=none`, the work directory read-only where the tests
allow it, no credentials in the environment or on the file system, and resource
limits. A command that needs the network or a secret to pass can be steered
into leaking it (a prompt injection planted in the repository). See
[AGENT-SECURITY](AGENT-SECURITY.md).

## Loop

1. Read the changelog of the branch from the saved cursor with
   `WatchChangesets` (see [Changeset log](../../README.md)).
2. Collect the changesets that moved the branch. Wait for the batch window so
   that near-simultaneous lands are tested together.
3. Materialize the current tip into a work directory (the workspace of
   `internal/graft/workspace`, sparse prefixes allowed) and run the test
   command with its argv, environment and timeout.
4. On success, move the green ref (default `refs/graft/green/<branch>`) to the
   tested tip with a compare-and-swap. The green ref lets consumers ask for
   the newest tested tip.
5. On failure, re-run the test once to rule out flakiness. If it still fails
   and `[revert] enabled = true`, bisect (below) and revert the culprit (below).
   Reverts are off unless `enabled = true` is set. Then a failing tip is only
   reported: nothing is bisected and nothing is reverted, and the green ref
   stays where it is.
6. Persist the cursor after every processed batch. Shutdown is graceful: the
   current batch finishes or is abandoned without moving the cursor.

`--once` runs one batch, prints the outcome as one JSON object and exits. Use
it in CI. Exit status 0 means the batch passed or was skipped, 2 means the
test failed on the tip, and 1 means an error, including a configuration error.

The outcome object has the fields `branch`, `tip`, `passed`, `timed_out`,
`green`, `green_moved`, `skipped`, `output`, `bisect_error` and `cursor`. It
does not say whether a revert was made. A revert shows on the branch as a new
commit whose `Graft-Auto-Revert` trailer names the culprit (see Auto-revert).

## Configuration

```toml
address = "unix:/var/lib/graft/run/gitaly.socket"
token_file = "/etc/graft/tokens/verifier"
storage = "default"
repository = "monorepo.git"
branch = "main"
sparse = ["services/api/", "pkg/"]
batch_window = "30s"
green_ref = "refs/graft/green/main"
work_dir = "/var/lib/graft/verify/main"
state_file = "/var/lib/graft/verify/main.cursor"

[test]
command = ["go", "test", "./services/api/..."]   # runs in the materialized tree
env = ["GOFLAGS=-mod=readonly"]                  # KEY=VALUE, added to the environment
timeout = "15m"

[revert]
enabled = true
dry_run = false
max_per_hour = 3
webhook_url = "https://hooks.internal.example/graft"
```

| Key | Default | Meaning |
|-----|---------|---------|
| `address` | required | `unix:<path>` or `tcp://host:port` of Gitaly (or a cache tier). |
| `token`, `token_file` | none | The shared token, inline or in a file (trimmed). At most one of them. |
| `storage` | `default` | Storage of the repository. |
| `repository` | required | Relative path of the repository. |
| `branch` | `main` | Branch to verify, without `refs/heads/`. |
| `green_ref` | `refs/graft/green/<branch>` | Must be under `refs/graft/green/`. |
| `work_dir` | required | Directory for the materialized trees. Created when missing; otherwise it must be empty or a previous `graft-verify` work directory. |
| `state_file` | required | JSON file holding the changeset cursor. |
| `sparse` | whole repository | Path prefixes to materialize. |
| `batch_window` | `30s` | How long lands are collected before the tip is tested. |
| `test.command` | required | argv of the test command, run in the materialized tree. `GRAFT_VERIFY_COMMIT` is set to the commit under test. |
| `test.env` | none | Extra `KEY=VALUE` entries. The rest of the environment is inherited. |
| `test.timeout` | `10m` | Bound on one test run. |
| `revert.enabled` | `false` | Bisect and revert culprits. |
| `revert.dry_run` | `false` | Report the planned revert without making it. |
| `revert.max_per_hour` | `0` | Reverts allowed per hour. `0` allows none, so set it with `enabled`. |
| `revert.webhook_url` | none | Receives a JSON notification for every revert decision. |

Unknown keys are an error.

## Bisection

- Candidates are the first-parent commits between the last green tip and the
  failing tip.
- The search runs the test command at each midpoint on a materialized tree.
- The culprit is the first failing commit.
- A commit that is itself an auto-revert (carries `Graft-Auto-Revert`) is
  never reverted again. A failing revert is reported for a human.
- Auto-reverts already in the range are taken into account. When culprit C
  was reverted by R but another culprit D landed between C and R, every
  commit from C up to R fails because of C, so a plain search would blame C
  again. Instead, each commit from C up to R is tested with C undone: the
  server builds a scratch revert commit (`UserRevert`) on a temporary branch
  under `refs/heads/graft-verify/scratch/`, the commit is tested, and the
  branch is deleted (`UserDeleteBranch`). The verified branch is never
  touched. A scratch branch left behind by a killed verifier can be deleted
  by hand. A dry run creates no scratch branches.
- A culprit that is already undone on the branch by other means (a revert by
  hand) while the tip still fails is reported as `already-reverted`: the
  branch needs a fix by hand.
- The green ref and the branch are read by their exact reference names, never
  through git's DWIM revision rules, so that a branch named like them
  (`refs/heads/refs/graft/green/main`, `refs/heads/refs/heads/main`) is never
  taken for them. A consumer has to read the green ref the same way (`ListRefs`
  with the full name, `git show-ref --verify`): until the green ref exists, a
  revision lookup such as `FindCommit` or `git rev-parse` answers with such a
  branch. Agents can no longer create or update one
  ([AGENT-SECURITY](AGENT-SECURITY.md)), but one from before that rule, or one
  an operator made, still answers.

## Auto-revert

The revert is created with `UserRevert` onto the current branch tip. If the
branch moved, the revert is re-applied onto the new tip, using the same
retry rules as `UserLand`. The commit message is:

```
Revert "<culprit subject>"

This reverts commit <culprit oid>, which failed the verification test command.

Graft-Auto-Revert: <culprit oid>
```

A reverted culprit is not landed again from the same commit: a `UserLand` of
its `commit_id` on the branch, for example the retry of a land whose response
was lost, answers `already_landed` with the culprit as `landed_commit_id` and
this revert as `reverted_by_commit_id` (the land identity of
[PLAN11](PLAN11.md)). To land the change again, the author makes a new commit:
the fixed change, or a revert of the revert.

Safeguards:

- `dry_run = true` reports the culprit and the planned revert, and changes
  nothing.
- `max_per_hour` caps automatic reverts. Beyond the cap the verifier only
  notifies.
- Every action is written to the log (`bisect decision`, with the status,
  culprit, revert commit and reason) and to the notifiers. When a
  notification fails, for example a webhook that answers 500, the decision
  and the failure are also recorded in the cycle's `bisect_error`, so that
  they reach the log and the `--once` JSON.

## Notifications

`Notifier` implementations are stdout (one line per event) and webhook (JSON
POST). Slack is covered by the superfan notifier, see [SUPERFAN](SUPERFAN.md).

## Permissions

The verifier writes the green ref, the revert and the scratch branches of a
bisection with the operator credential
(shared token), because agent identities may not call reference mutators (see
[AGENT-SECURITY](AGENT-SECURITY.md)). Run it with a dedicated token that only
the operator holds.

## Status

This document describes the phase-2 contract (work packages WP16 for the core
loop, green ref and `--once`, and WP17 for bisection, auto-revert, caps and
notifiers). See `doc/graft/PLAN2.md` for what each package delivers.
