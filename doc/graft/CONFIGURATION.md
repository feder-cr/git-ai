# Graft configuration reference

Graft settings live in the `[graft]` section of Gitaly's TOML configuration
file, next to the usual Gitaly keys. The section is defined in
`internal/gitaly/config/config_graft.go`. Every key is optional. An empty
section keeps upstream Gitaly behaviour, except that the defaults below apply.

The annotated example is [`examples/graft/config.toml`](../../examples/graft/config.toml).
The end-to-end tests in `internal/graft/e2e` load that file and validate it, so
the example stays a working configuration.

## Gitaly keys that standalone mode uses

| Key | Notes |
|---|---|
| `socket_path` | Unix socket to serve on, for example `/var/lib/graft/run/gitaly.sock`. Use `listen_addr` for TCP. Clients address a unix socket as `unix:<path>`. |
| `bin_dir` | Directory with `gitaly-hooks` and `gitaly-ssh`. Required, as in upstream Gitaly. |
| `runtime_dir` | Directory for runtime files. |
| `[auth] token` | Shared token. It is also what the test helpers use for setup calls when agents are configured. |
| `[[storage]]` | Repository storage. Repositories are found below `path` by their relative path. |
| `[gitlab]`, `[gitlab-shell]` | Not required when `[graft] standalone = true`. |

## `[graft]`

| Key | Type | Default | Description |
|---|---|---|---|
| `standalone` | bool | `false` | Run without GitLab. No GitLab secret or URL is required, GitLab is never contacted, and access checks and hook callbacks are answered by the standalone client from `[[graft.policy]]`. Request defaults are applied on the external server. |
| `policy` | array of tables | empty | Ordered access rules, see below. |
| `agent` | array of tables | empty | Per-agent credentials, see below. With at least one agent, every request must present an agent token or the shared `auth.token`. |
| `agent_rpcs` | array of strings | empty | Extra full gRPC method names (for example `/gitaly.RepositoryService/RepositorySize`) that agents may call on top of the built-in allowlist. Startup fails on an unregistered method or on a method that agents may never call. See [AGENT-SECURITY.md](AGENT-SECURITY.md). |
| `land` | table | | `[graft.land.batch]`: batching of concurrent lands, and `[graft.land.resolver]`: the conflict resolver of `UserLand`, see below. |
| `storage` | table | | `[graft.storage]`: the storage core (database plus object storage), see below. |

### Request defaults (standalone mode)

On the external server, the request defaults fill the GitLab-specific identity
fields of every request, unary or streaming (each received message of a stream
is handled).

- Every `gitaly.User` that the request contains, at any depth (for example the
  user in the header of the first `UserCommitFiles` message, or a user inside a
  oneof), gets `gl_id` and `gl_username` set to the caller's identity name, or
  to `anonymous` when there is no identity. A request without a `gitaly.User` is
  not given one, so the handler rejects it as it does without Graft.
- Every string field named `gl_id` or `gl_username`, at any depth, gets the same
  value.
- The top-level `repository` field gets `gl_repository` set to
  `graft:<storage>/<relative path>` when it is empty. This value is never
  overwritten, and no other repository field is touched.

For a request made with an agent token, the identity fields are always set to
the agent name, even when the client sent another value, so an agent cannot
claim another actor's user ID. For the shared token and for unauthenticated
requests, they are filled only when empty and a value the client sent is kept.
The user's name and email, which become the committer of new commits, are never
changed. See [LIMITATIONS.md](LIMITATIONS.md), Security limitations.

### `[[graft.policy]]`

The policy answers the access check for a reference update. For each changed
reference, the first rule that applies decides:

- A rule applies when its `actor` matches the actor of the request and one of
  its `refs` globs matches the reference. The actor is the agent name for a
  request made with an agent token. Otherwise (shared token, no
  authentication) it is the acting user ID of the request (`gl_id`), see
  [LIMITATIONS.md](LIMITATIONS.md). An empty `actor` or `"*"` matches every
  caller. Empty `refs` match every reference.
- A reference that no rule applies to is denied. This includes tags and any other
  reference that the rules do not name. Add a final rule with `allow = true` to
  open everything that is left.
- An update is allowed only if every changed reference is allowed. The denial
  message names the rule that decided, or says that no rule matched.
- When the policy is empty, every update is allowed.
- For requests made with an agent token, the policy holds for every reference
  update under `refs/`, whichever RPC performs it (for example `WriteRef` and
  `DeleteRefs`, not only `UserLand` and pushes). See
  [LIMITATIONS.md](LIMITATIONS.md), "Which updates the policy covers", which
  also lists the administrative RPCs that agent tokens cannot call.
- A batched land is decided per request, with the actor of each request, before
  the request joins the batch. The combined update of the batch, made as the
  synthetic user `graft-batch`, is not checked again, so a rule for
  `graft-batch` has no effect. See
  [LAND-BATCHING.md](LAND-BATCHING.md#hooks-and-identity-of-the-combined-update).

| Key | Type | Default | Description |
|---|---|---|---|
| `actor` | string | `""` (everybody) | Glob (`path.Match` syntax) matched against the actor of the request: the agent of the token, or the acting user ID for the shared token. |
| `refs` | array of strings | `[]` (every reference) | Globs matched against fully qualified references such as `refs/heads/main`. Syntax is `path.Match`, plus a trailing `/**` that matches any depth, so `refs/heads/**` matches `refs/heads/feature/x`, and any other trailing `**` that matches any remainder, so `refs/heads/release**` matches `refs/heads/release/1.0`. Invalid globs and empty entries are rejected at startup, and by a reload, which keeps the policy in force. |
| `allow` | bool | `false` | Whether a matching update is allowed. |

Example: protect `main` for everybody except the `planner` agent, and allow
everything else:

```toml
[[graft.policy]]
actor = "planner"
refs = ["refs/heads/main"]
allow = true

[[graft.policy]]
refs = ["refs/heads/main"]
allow = false

[[graft.policy]]
allow = true
```

#### Reloading the policy

Send SIGUSR1 to the gitaly process (or to gitaly-wrapper, which forwards it):
it reads its configuration file again and puts the new `[[graft.policy]]` in
force. SIGHUP is not a reload: it stops the server, or upgrades it in place
with the graceful-upgrade bootstrap.

- Only the policy is applied. Other changes of the file, `[[graft.agent]]`
  included, are logged at WARN as needing a restart, and not applied.
- A file that cannot be read, or whose policy is invalid (a bad glob, an empty
  `refs` entry), is logged at ERROR and changes nothing: the policy in force
  stays. Actors of the new policy that name no configured agent are logged at
  WARN; they are not an error (an actor can be the user ID of a caller with
  the shared token).
- An update in flight is decided by the policy in force when the
  reference-transaction hook runs, while git holds the references' locks: a
  tightening applies to it even if its earlier checks allowed it. A land batch
  whose members were admitted under another policy is refused there, and its
  requests land alone, each decided by the policy in force. A batch attempt
  judges each of its members with the policy in force when the attempt
  started: a loosening reaches the members of the next attempt.
- Each node reloads its own file: reload every node, then compare their
  `graft_policy_info`.

Metrics: `graft_policy_reloads_total{result}` (`applied`, `unchanged`,
`rejected`), `graft_policy_generation` (1 at startup, one more for every
applied reload) and `graft_policy_info{sha256}` (1 for the hash of the
policy in force). A denied agent update is logged at INFO with the
`policy_generation` and `policy_sha256` that denied it; an allowed one at
DEBUG.

Before a reload, `graft policy check FILE` validates the file's policy as the
reload will (exit 2 when the policy is invalid) and prints its hash; with
`-repo REPO` it lists the recent updates of the repository that the new policy
would deny, and exits 1 when there are any. It checks the policy only: the reload also validates the rest of
the file, against the server's own paths and settings, and rejects a file that
fails there.

### `[[graft.agent]]`

| Key | Type | Default | Description |
|---|---|---|---|
| `name` | string | required | Identity of the agent. It is the identity the token maps to, the actor the policy decides on for the token's requests, the actor that the changeset log records for the token's updates and, in standalone mode, the user ID (`gl_id`, `gl_username`) of every request made with the token, whatever the client sent. The name `shared` is reserved for the shared token, and `proposals` and `green` for the server's references under `refs/graft/`: the workspace's temporary branches of an agent are `graft/<name>/...`, which for those names an agent may not use. |
| `token` | string | — | The secret. Set either `token` or `token_file`. |
| `token_file` | string | — | Path to a file that holds the secret, surrounding whitespace removed. Read at startup. |

Clients send the token as a plain bearer token in the standard `authorization`
metadata. The v2 HMAC token format, computed with the agent token as the secret,
is accepted too. Comparison is constant time. The shared `auth.token` is accepted
only in the v2 HMAC format, never as a plain bearer token. Health checks never
require a token.

Agent names and tokens must be unique. Startup fails when a name is empty or
reserved, a name repeats, two agents share a token, an agent token equals the
shared `auth.token`, both `token` and `token_file` are set, or a token file is
missing or empty.

### `[graft.object_service]`

| Key | Type | Default | Description |
|---|---|---|---|
| `max_objects` | int | `1000` | Maximum number of object IDs per `GetObjects` request. A larger request fails with `InvalidArgument`. The SDK splits large reads automatically and adapts to a lower limit. |
| `chunk_size` | int | `1048576` (1 MiB) | Maximum data bytes per `GetObjects` message. |

`graft-cache` has its own `--max-objects` and `--chunk-size` flags. Set them to
the same values as the upstream, so that a cache tier returns the same stream.

### `[graft.changelog]`

| Key | Type | Default | Description |
|---|---|---|---|
| `disabled` | bool | `false` | Turn off recording of reference updates in `<git dir>/graft/changesets.jsonl`. New reference updates are not recorded. Entries recorded earlier stay in the log, and the changeset RPCs still return them. |

### `[graft.derived]`

| Key | Type | Default | Description |
|---|---|---|---|
| `disabled` | bool | `false` | Turn off the derived-data store and the commit-graph writes after updates. `Blame`, `PathHistory` and `LastCommitForPath` compute their results from scratch on every call. |
| `eager` | bool | `false` | Compute the `graph` and `changed-paths` kinds in the background right after a reference update commits, so that the first read finds them. Lazy otherwise. Has no effect when `disabled` is true. See [DERIVED.md](DERIVED.md). |
| `workers` | int | `2` | Background workers that compute derived data eagerly. Zero means the default. |
| `queue_size` | int | `10000` | Batches (one per committed transaction) waiting for the workers. When full, a batch is dropped and its commits are computed on first read. Zero means the default. |
| `commit_graph` | bool | `false` | Write the commit-graph incrementally after committed updates (debounced, through housekeeping). Off by default since phase 4a; with `false` no commit-graph is written outside housekeeping. A phase 3 server that relied on the old default must set `true`. Has no effect when `disabled` is true. See [DERIVED.md](DERIVED.md#commit-graph-writes). |

### `[graft.land]`

| Key | Type | Default | Description |
|---|---|---|---|
| `graph_ancestry` | bool | `false` | Answer the history questions of `UserLand` from the stored `graph` index (see [DERIVED.md](DERIVED.md)) instead of running `git`, for every land, batched or not: the listing of the commits that a request adds to the branch, and the ancestry and merge-base checks that make that listing exact when committer dates are skewed (see [LAND-BATCHING.md](LAND-BATCHING.md#how-a-request-is-decided-and-rebased)). The index lists the exact commits in one walk; both ways return the same answers. If the index cannot be built, or the repository has shallow commits, grafts or replace refs, the server falls back to `git` and logs it. With `[graft.derived] disabled = true` the index is computed in memory for each landing attempt and nothing is written under `graft/derived/`. |

### `[graft.land.batch]`

Batching of concurrent `UserLand` requests on one branch: requests are queued per
branch, and a batch is published with one reference update. Standalone mode only.
See [LAND-BATCHING.md](LAND-BATCHING.md).

| Key | Type | Default | Description |
|---|---|---|---|
| `disabled` | bool | `false` | Turn batching off. Every request then runs its own compare-and-swap loop, as in phase 2. |
| `window` | string (Go duration) | `"10ms"` | How long a lane waits after its first queued request before it runs the batch. `"0s"` means no wait: a batch is whatever queued while the previous batch ran. Must not be negative. |
| `max_size` | int | `50` | Largest number of requests in one batch, at most `100`: the update of a batch records the identity of every member ([PLAN11](PLAN11.md)) in the environment of its git processes. Zero means the default; negative values and values above `100` are rejected. |
| `target_latency` | string (Go duration) | `"500ms"` | Batch duration the lane aims for. A lane halves its effective maximum batch size after a batch slower than this, and grows it back (up to `max_size`) after batches faster than half of it. `"0s"` disables the adaptation, and the lane always uses `max_size`. Must be a valid, non-negative Go duration. See [LAND-BATCHING.md](LAND-BATCHING.md#adaptive-sizing). |

### `[graft.land.resolver]`

Automatic conflict resolution during `UserLand`. Off by default. The server
never calls a language model: it calls the program or HTTP endpoint configured
here. The protocol, the validation rules and `graft-resolve` are described in
[RESOLVE.md](RESOLVE.md).

A resolver configuration the server could not use is a startup error: an empty
`command`, a `url` that is not an `http` or `https` URL with a host, a
`token_file` that cannot be read or is empty, a bad `timeout`, a negative
bound, or a `max_file_bytes` over the bound of a check request. A configured
resolver is never treated as no resolver: if it cannot be built later (its
token file removed, say), every land that needs it fails closed, with the
cause.

| Key | Type | Default | Description |
|---|---|---|---|
| `enabled` | bool | `false` | Master switch. When false, `resolve_conflicts` in a land request is ignored. |
| `command` | array of strings | empty | Program and arguments. It reads one request on stdin and writes the answer on stdout. |
| `url` | string | empty | `http://` or `https://` endpoint that receives the request as a JSON POST. Exactly one of `command` and `url` must be set when `enabled` is true. |
| `token_file` | string | empty | For `url`: file with the bearer token, read at startup. |
| `timeout` | string | `"120s"` | Bound on one resolver call (Go duration), and on the whole land that the server resolves for `resolve_conflicts`: its rounds (the resolutions, the verifications of a model's resolution, the waits for slots, and the passes through the branch's lane) end at this deadline, which is `timeout` plus `verify.timeout` when `verify.enabled`. A land that is still unresolved at this deadline does not fail with a deadline: it ends with the merge conflict and its package, the proposals of what is known and the files that changed under it as not attempted, with the reason. |
| `max_files` | int | `20` | Conflicted files per land. A land with more is not sent to the resolver. |
| `max_file_bytes` | int | `1048576` (1 MiB) | Bound on every side of every file sent and on every resolved file accepted. At most 12566528 (about 12 MiB): one file, its three sides and its resolution, must fit one check request. |
| `proposal_ttl` | string | `"168h"` | How long the proposals of a failed land are kept under `refs/graft/proposals/` (Go duration, by the server's clock); at most 20 are kept per branch. Lands with a resolution and the housekeeping pass prune them. |
| `check_concurrency` | int | `2` | Checks of agents' resolutions that run at once in the Gitaly process, across all its listeners (one resolver client is shared by them); each holds a request of up to 64 MiB. A check waits for a free slot within its deadline (`timeout`), and fails closed when it expires. The wait is in `graft_resolver_check_slot_wait_seconds`. |
| `verify_per_repository` | int | `1` | Verifications that run at once for one repository (of any resolution: a rule's, an agent's, a model's), inside `verify_concurrency`: every land of a busy branch builds a tree of its own to verify, and one repository must not take the whole pool. A land waits for its repository's turn first, holding nothing of the pool, within the land's deadline. **The cost, stated:** a resolution lands only on the tree that was verified, so a land whose branch moved after its tree was verified is verified again, and on a busy branch the lands take their turns one repository at a time; at the deadline a land ends with the package (a rule's files `NOT_ATTEMPTED` with the reason, a model's `PROPOSED`) and the agent lands again. Lands of rules are batched only when `verify.enabled` is not set. |
| `verify_concurrency` | int | `2` | Verifications that run at once in the Gitaly process (of any resolution: a rule's, an agent's, a model's), all out of the lane, in a pool of their own that exists whenever `verify.enabled` is set, with or without an AI resolver, as `verify.command` takes minutes. A verification waits for a free slot within the land's deadline. The wait is in `graft_resolver_verify_slot_wait_seconds`. |
| `resolve_concurrency` | int | `2` | Resolutions that run at once in the Gitaly process, in a pool apart from the checks', so that long resolutions never delay a check. A resolution waits for a free slot within the land's deadline (`timeout`), and fails closed when it expires. The wait is in `graft_resolver_resolve_slot_wait_seconds`. The lane's inline calls (`inline_timeout`) have a pool of their own, of at least 8 slots, which the lane never waits for. |
| `resolve_rounds` | int | `32` | Safety cap on the rounds of a land whose conflicting files keep changing while it is resolved out of the lane. The rounds are bounded by `timeout`; a land that loses a round waits for its turn on its files in a queue, so that it does not lose them one after the other (RESOLVE.md, "Out of the lane"), and the cap is not reached in practice. A round that enters the lane without a pre-resolution (the first, and the ones after a wait for a turn; at most 4) does not count. It is no contract. |
| `inline_timeout` | string (Go duration) | `"1s"` | Bound of the call with which the lane resolves a member's conflicting files itself, as the rules (trivial, union, rerere, a command that is a rule) and never a model: the request carries `rules_only` and announces no capability but `origin`. The lane never waits for a slot (the calls have a pool of their own; with it taken the member goes out of the lane), and the time it spends in inline calls is bounded once per batch attempt: all the time of every call, a successful one too, is charged to the batch, and once the batch has spent `inline_timeout` the other members go out of the lane without a call (fast rules, milliseconds, do not notice; a resolver that leaves child processes holding its output may keep the lane up to 2 s more, the kill delay). A member whose files the rules cannot resolve within it is resolved out of the lane, as any other. `"0s"` turns the inline resolution off; a negative or invalid value is a configuration error. Never used with `verify.enabled`: every tree is then verified out of the lane. |
| `paths_allow` | array of strings | empty (every path) | Globs of paths that may be sent to the resolver, with the syntax of policy refs. |
| `paths_deny` | array of strings | empty | Globs of paths that are never sent. Deny wins over allow. |
| `actors` | array of strings | empty (every caller that may land) | Globs of the identities that may request resolution: agent names, or `shared` for the shared token. Without `[[graft.agent]]` no caller has an identity, so a non-empty list admits nobody (the server warns at startup). |
| `lockfiles` | array of strings | empty | Base names of lockfiles, besides the built-in `package-lock.json`, `yarn.lock`, `pnpm-lock.yaml`, `poetry.lock`, `Cargo.lock` and `go.sum`. A conflicting lockfile is never sent to the resolver: it goes to the agent. |
| `generated_markers` | array of strings | empty | Go regular expressions, besides the built-in `Code generated .* DO NOT EDIT\.`, `@generated`, `DO NOT EDIT` and `(?i)auto-generated`, searched in the first 10 lines of every side. A match marks the file generated, and it is never sent. A path that the tip's `.gitattributes` marks `linguist-generated` is generated as well. |
| `minified.globs` | array of strings | empty | Base-name globs of minified files, besides the built-in `*.min.js`, `*.min.mjs`, `*.min.css` and `*.bundle.js`. Such files are never sent. |
| `minified.average_line_bytes` | int | `300` | A side larger than 2 KiB whose average line is longer is minified. |
| `minified.longest_line_bytes` | int | `5000` | A side larger than 2 KiB with a longer line is minified. |
| `verify.enabled` | bool | `false` | Run `verify.command` on the merged tree of every resolution (a resolver's or an agent's) before it lands; it applies even when `enabled` is false. It is also what lets a model's resolution land: only a tree it passed on lands, and without it a model's resolution is only ever a proposal. |
| `verify.command` | array of strings | empty | Program and arguments, run in a temporary checkout of the merged tree with `PATH`, `HOME` and `TMPDIR` only. A non-zero exit rejects the resolution. |
| `verify.timeout` | string | `"5m"` | Bound on one verification, the checkout of the tree and the command together (Go duration); one that does not finish rejects the resolution. When `verify.enabled`, it is added to `timeout` for the deadline of a resolved land. |
| `verify.max_tree_entries` | int | `200000` | Files, links and submodules in the tree that is checked out for the command, plus the directories made for them; a bigger tree is not verified, and the package says why. |
| `verify.max_tree_bytes` | int | `2147483648` | Bytes of the files of that tree and of the targets of its links, checked per entry from the listing, before that entry's content is read. |
| `verify.max_tree_depth` | int | `64` | Components of a path of that tree. A tree can name one deep subtree many times; the expansion is what this and the entry count bound. |

A file that is never sent goes to the agent with its reason in the conflict
package: large (a side over `max_file_bytes`), binary (a NUL byte), lockfile,
generated or minified, checked in that order (see [RESOLVE.md](RESOLVE.md)).

Startup fails when both or neither of `command` and `url` are set, the URL has
another scheme, `timeout` is not a duration, a bound or a threshold is
negative, a glob is empty or invalid, a lockfile is not a base name, or a
generated marker is not a valid regular expression.

### `[graft.storage]`

The storage core: the state of every repository in a database plus object
storage, with the local repositories as caches that any node can rebuild. Off
by default. Defined in `internal/gitaly/config/config_graft_storage.go`. The
architecture, the modes and the operations (import, verify, switching modes)
are in [STORAGE.md](STORAGE.md).

| Key | Type | Default | Description |
|---|---|---|---|
| `mode` | string | `"off"` | `"off"` (also the empty string): nothing is opened and nothing changes. `"shadow"`: every reference transaction of an imported repository is also committed to the database and its pack uploaded; the database never rejects a write, and every storage core error is logged and counted while the git write proceeds. `"authoritative"`: the database is the source of truth; a write commits only if its database compare-and-swap succeeds, every request first catches the local repository up, and the changelog and the reviews are served from the database. Authoritative mode requires `[graft] standalone = true` and is not supported behind Praefect. |
| `database` | string | required when on | `sqlite:///absolute/path.db`, or `postgres://user@host:port/db?sslmode=...` (`postgresql://` too). All nodes use the same database. Credentials are not written here: Postgres reads the `PG*` environment variables and `.pgpass`. The schema is created at startup. |
| `bucket` | string | required when on | gocloud URL of the object storage: `file:///absolute/dir`, `s3://bucket?region=...`, `gs://bucket` or `azblob://container`. All nodes use the same bucket. Credentials come from the cloud SDK's environment (`AWS_*`, `GOOGLE_APPLICATION_CREDENTIALS`, `AZURE_*`). |
| `node_id` | string | the host name | Name of this node in the database (changesets record it, and `graft storage state` shows it). Must not contain spaces or slashes. Give every node its own. |
| `sync_timeout` | string (Go duration) | `"10s"` | Budget of one sync round before a request in authoritative mode. A round that cannot reach the head in time keeps what it did: the local repository stays at the changeset it reached, the request fails with `Unavailable`, and the next request's round goes on from there. A round that cannot finish at all (for example because the database is unreachable or a local writer holds a reference lock) fails the request with `Unavailable` too. Concurrent requests share a round; a request that arrives during a round that read the head before it then runs one of its own, so it waits at most two rounds (and stops waiting with `Unavailable` after twice `sync_timeout` plus a second per round, should a round not end within its budget). Clients retry `Unavailable`: the SDK does it for every land (`LandOptions.RetryUnavailable`, see [LAND-BATCHING](LAND-BATCHING.md#limits)): the retry of a land whose response was lost answers `already_landed` from the server's record of the first land, rebase lands included (that record needs a server released after v0.2.0 with its changeset log on, `[graft.changelog]` not disabled; against another server the retry of a lost rebase land replays the change, and `SkipRebaseLands` turns the retry of rebase lands off). A client without the SDK must follow the same rules. Must be positive. |
| `shadow_timeout` | string (Go duration) | `"5s"` | Budget of the shadow-mode work of one reference transaction, for its Preparing phase (pack build and upload, before git locks the references) and its Prepared phase together. When it runs out, the work is abandoned, logged and counted, and the git write proceeds. Must be positive. |
| `gc_grace` | string (Go duration) | `"1h"` | How long garbage collection waits before it deletes a pack it observed unreferenced, and how old a checkpoint must be before the packs it covers are deleted. Must be greater than `sync_timeout + gc_margin`. |
| `gc_margin` | string (Go duration) | `"5m"` | Safety margin of the writers' deadline: a write uploads its pack and commits its changeset within `gc_grace - gc_margin`, else it fails. It also covers the difference between a node's clock and the database's. Must be positive. |
| `checkpoints` | string | `"auto"` in authoritative mode, `"off"` otherwise | `"auto"` runs the background checkpoints: every `maintenance_interval`, every repository this node has a current copy of is checkpointed when the thresholds below say so, under a per-repository lease so that two nodes do not do the same work. On by default: a checkpoint deletes nothing (it adds a pack and database rows), and without checkpoints a node that joins replays every changeset of a repository. `"off"` turns them off. `"auto"` requires authoritative mode. |
| `gc` | string | `"off"` | `"auto"` runs the background garbage collection: every `maintenance_interval`, the packs of removed repositories are deleted, and so are the dead packs of every repository this node has a current copy of, under the same lease. Off by default because it is the operation that deletes data. Authoritative mode only. `graft storage gc` does the same on demand in either case. |
| `maintenance_interval` | string (Go duration) | `"1h"` | Time between two runs of the background checkpoints and garbage collection. Must be positive. |
| `checkpoint_changesets` | integer | `1000` | Checkpoint a repository once this many changesets follow its newest checkpoint. `0` never checkpoints by count. |
| `checkpoint_pack_bytes` | integer | `268435456` (256 MiB) | Checkpoint a repository once the packs of the changesets after its newest checkpoint hold this many bytes. `0` never checkpoints by size. |
| `lease_ttl` | string (Go duration) | `"10m"` | How long a node holds a repository's maintenance lease; renewed every third of it while the work runs. Must be positive. |
| `database_max_open_conns` | integer | `32` | Postgres only: the most connections this node opens to the database. A request that finds them all in use waits for one, as long as its own deadline allows. Every node opens up to this many, and during a graceful upgrade the old and the new process of a node each have a pool, so one node can briefly use twice as many. Keep the sum, plus what other clients use, below the server's `max_connections` (100 by default in PostgreSQL, minus its `superuser_reserved_connections`): a connection the server refuses (error 53300) fails the operation and is not retried. With the default, the database-only benchmark on a 16-CPU machine commits 2,019 to 2,127 changesets per second for 16 writers in separate repositories and 2,586 to 2,630 for 64 writers queued on the 32 connections, more than twenty times what one node committed end to end in the land benchmarks. At least 1. |
| `database_max_idle_conns` | integer | `database_max_open_conns` | Postgres only: how many of those connections stay open while unused, so that steady load reuses them instead of opening a connection per transaction. Between 0 and `database_max_open_conns`. |
| `database_conn_max_idle_time` | string (Go duration) | `"5m"` | Postgres only: a connection unused for this long is closed, so that the pool shrinks after a burst. Must be positive. |
| `database_conn_max_lifetime` | string (Go duration) | `"30m"` | Postgres only: a connection is replaced this long after it was opened, so that the pool follows a failover or a changed DNS name. Must be positive. |

```toml
[graft.storage]
mode = "authoritative"
database = "sqlite:///var/opt/graft/meta.db"
bucket = "file:///var/opt/graft/objects"
node_id = "node-a"
gc = "auto"             # checkpoints are on by default in authoritative mode
```

Startup fails when the mode is unknown, authoritative mode is set without
`standalone = true`, the database URL is missing or has another scheme, the
bucket is missing or not a URL, `node_id` contains spaces or slashes, a
duration is not a positive Go duration, `gc_grace` is not greater than
`sync_timeout + gc_margin`, `checkpoints` or `gc` is neither `off` nor `auto`,
either is `"auto"` outside authoritative mode, `checkpoint_pack_bytes` is
negative, a `database_*` connection setting is given for a SQLite database,
`database_max_open_conns` is below 1, or `database_max_idle_conns` is outside 0
to `database_max_open_conns`. A SQLite database is opened as before: its writers
are queued by the storage core and its connections are local files.

## `graft-cache` flags

The cache is configured on its command line, not in `config.toml`.

| Flag | Default | Description |
|---|---|---|
| `--listen` | required | Where to serve: `unix:<path>` or `tcp://<host:port>`. |
| `--upstream` | required | Gitaly or another cache tier, in the same forms. |
| `--tier` | `cache` | `cache` (regional; may use `--disk-dir`) or `daemon` (per node; memory only). The tier is reported in the statistics. |
| `--max-bytes` | `268435456` (256 MiB) | Byte budget of the in-memory object and tree cache. Objects larger than the budget are not cached. |
| `--max-request-bytes` | `67108864` (64 MiB) | Bytes of objects that are not in memory (disk hits and misses) that one `GetObjects` request may buffer, and the largest `GetTree` listing buffered for caching. A larger request is streamed through the upstream without caching. |
| `--max-objects` | `1000` | Object IDs per `GetObjects` request. Match the upstream's `max_objects`. |
| `--chunk-size` | `1048576` | Data bytes per `GetObjects` message. Match the upstream's `chunk_size`. |
| `--disk-dir` | empty | On-disk object store. Only valid with `--tier cache`; the daemon tier refuses it. |
| `--disk-max-bytes` | `10737418240` (10 GiB) | Byte budget of the on-disk store. The oldest objects are evicted first. |
| `--metrics-addr` | empty | HTTP address that serves Prometheus metrics at `/metrics` and the statistics as JSON at `/stats`. Disabled when empty. |
| `--token` | empty | Sent upstream on requests that carry no `authorization` header of their own. Requests that carry one always use their own. Every client that can reach the tier without credentials gets this token's access, so set it only when all of them may use it. |
| `--seed` | empty | Comma-separated seed files (`FILE[,FILE]`), written by `graft-cache snapshot`, loaded in order into the cache before the tier serves. Every object is verified against its object ID; a damaged record is skipped and counted, and a file that cannot be opened or is not a seed file stops the start. Objects go into memory and, with `--disk-dir`, onto disk; an object larger than `--max-bytes` is skipped. Seeded objects are served without upstream object reads, but every read is still authorized by the upstream first. |

Chaining: point the `--upstream` of a daemon tier at a cache tier, and the cache
tier's `--upstream` at Gitaly. Each tier serves its own hits and forwards its
misses in one batched call.

### `graft-cache snapshot`

`graft-cache snapshot` writes a seed file with every tree and blob reachable
from the root tree of one revision, read from the upstream with a recursive
`GetTree` and batched `GetObjects` calls. Every object is verified; a mismatch
fails the snapshot. The file is written to a temporary file next to `--out` and
renamed into place only when the snapshot succeeded.

```shell
graft-cache snapshot --upstream unix:/run/gitaly.sock --repo demo.git --rev main --out demo.seed
graft-cache --listen unix:/run/graft-cache.sock --upstream unix:/run/gitaly.sock --seed demo.seed
```

| Flag | Default | Description |
|---|---|---|
| `--upstream` | required | Gitaly or a cache tier to read from: `unix:<path>` or `tcp://<host:port>`. |
| `--token` | empty | Token sent upstream as the caller's credentials. |
| `--storage` | `default` | Storage name of the repository. Must not be empty. |
| `--repo` | required | Relative path of the repository below the storage. |
| `--rev` | required | Revision whose root tree is snapshotted, for example `main`. |
| `--out` | required | Seed file to write. |

It prints `snapshot: <objects> objects, <bytes> bytes, <missing> missing -> <out>`.

### Seed statistics

`/stats` (JSON) and `/metrics` report the seeding of a tier:

| `/stats` field | Metric | Description |
|---|---|---|
| `seeded_objects` | `graft_cache_seeded_objects_total` | Objects loaded from seed files. |
| `seeded_bytes` | `graft_cache_seeded_bytes_total` | Data bytes loaded from seed files. |
| `seed_corrupt` | `graft_cache_seed_corrupt_total` | Seed records skipped because they were damaged. |

## `graft` CLI

The CLI reads these environment variables. Each one can also be passed as a flag
(`--address`, `--token`, `--storage`, `--user`, `--author`).

| Variable | Default | Description |
|---|---|---|
| `GRAFT_ADDRESS` | from the workspace | Gitaly or `graft-cache` address. |
| `GRAFT_TOKEN` | none | The agent token, or the shared token. |
| `GRAFT_STORAGE` | `default` | Storage name. |
| `GRAFT_USER` | empty | The acting user ID of writes. It names the temporary branches of `graft commit` (`graft/<user>/...`), verbatim when it is a valid reference name component and as a lowercase slug otherwise. Set it to the agent name of `GRAFT_TOKEN`. With an agent token the server replaces it with the token's agent (standalone mode) and the policy decides on that agent; with the shared token it decides on this ID. Without it, the CLI sends the user ID `anonymous` and commits on a temporary branch named after the author, which a policy that scopes temporary branches per agent may deny. |
| `GRAFT_AUTHOR` | `graft <graft@localhost>` | Author of `graft commit`, as `Name <email>`. |

The workspace flag `-C DIR` selects a workspace. Without it, the CLI looks for
the workspace above the current directory.

## Minimal example

```toml
[graft]
standalone = true

[[graft.agent]]
name = "planner"
token_file = "/etc/graft/tokens/planner"

[[graft.policy]]
actor = "planner"
allow = true
```

With this policy, `planner` may update any reference and every other caller is
denied, because no rule applies to them. Add a final `allow = true` rule to
change that. The annotated example in `examples/graft/config.toml` is the
starting point for a real deployment.
