# Automatic conflict resolution during land

`UserLand` can resolve merge conflicts itself. When a change cannot be
rebased or merged cleanly onto the branch tip, the server asks a **resolver**
for the merged content of the conflicting files. The resolver is a separate
program or HTTP service that the operator chooses. The Graft server never
talks to a language model and never imports a vendor SDK. AI is disabled by
default.

The reference resolver, `graft-resolve`, lives in the separate module
`tools/graft-agents`. It runs deterministic strategies out of the box. Its
optional `llm` strategy is covered in [AI resolvers](#ai-resolvers).

## Enabling it

Resolution is off unless it is configured and requested.

```toml
[graft.land.resolver]
enabled = true
command = ["/var/lib/graft/bin/graft-resolve", "--config", "/etc/graft/resolve.toml"]
# or, instead of command:
# url = "https://resolver.internal.example/v1/resolve"
# token_file = "/etc/graft/tokens/resolver"
timeout = "30s"
max_files = 20
max_file_bytes = 1048576
paths_allow = ["**"]
paths_deny = ["deploy/**", "*.lock"]
actors = ["builder", "planner"]
```

- `enabled`: master switch. When false, `resolve_conflicts` is ignored.
- `command` or `url`: exactly one. `command` runs the program with the request
  on stdin. `url` receives an HTTP POST with a bearer token read from
  `token_file`.
- `timeout`, `max_files`, `max_file_bytes`: bounds for each resolver call.
  `max_files` bounds a request for an AI resolution only: the conflict package
  lists every conflicting file, and the check of an agent's resolution covers
  all its files.
- `check_concurrency` (default 2): the checks of agents' resolutions that run
  at once in the Gitaly process, whatever listener the lands arrive on (see
  the check below).
- `resolve_concurrency` (default 2): the resolutions that run at once in the
  Gitaly process, in a pool apart from the checks', so that long resolutions
  never delay the short checks. `resolve_rounds` (default 32) is a safety cap
  on the rounds of a land whose files keep changing (see "Out of the lane"
  below); the rounds are bounded by `timeout`, and the cap is no contract.
- `verify_concurrency` (default 2): the verifications of a model's resolution
  that run at once in the Gitaly process, in a pool of their own, and
  `verify_per_repository` (default 1) the ones of one repository, so that a busy
  repository cannot take the whole pool (see "Optional verification of a
  resolution").
- A configuration the server could not use is a startup error (see
  [CONFIGURATION](CONFIGURATION.md)). A configured resolver is never treated
  as no resolver: if it cannot be built later, every land that needs it fails
  closed, and the package says why.
- `paths_allow`, `paths_deny`: glob lists with the same semantics as policy
  refs (`**` as a suffix matches any depth). A conflict in a denied path, or
  in a path that is not allowed, is not sent to the resolver. Apart from the
  `**` suffix, globs follow Go's `path.Match`: `*` never crosses a `/`. So
  `*.lock` denies `go.lock` but not `sub/go.lock`, and `*.env` does not deny
  `a/.env`. To deny a name at several depths, list one glob per depth
  (`*.env`, `*/.env`, `*/*/.env`) or deny whole directories (`secrets/**`).
- `actors`: globs of the actors that may request an AI resolution. Other
  actors get the normal `merge_conflict` error, with the conflict package. An
  empty list allows every caller that may land. `actors` gates only the
  requests for an AI resolution (`resolve_conflicts`): an agent's re-land with
  its own resolution is allowed for every actor that may land, and the
  resolver's `check` runs on it whatever the actor. The actor is the authenticated identity of the caller, which only
  agent authentication sets (see [AGENT-SECURITY](AGENT-SECURITY.md)):
  - with `[[graft.agent]]` configured, the agent name of a caller with an
    agent token, or `shared` for a caller with the shared `auth.token` (for
    example GitLab);
  - without `[[graft.agent]]`, no caller has an identity, so a non-empty list
    admits nobody. The server warns about this at startup.

  The user of the request (`gl_username`, `gl_id`) is chosen by the caller
  and never decides the actor, so listing GitLab user names has no effect.

A client asks for resolution per call:

- SDK: `LandOptions.ResolveConflicts`, and `LandResult` reports the
  resolved paths and the resolver name. On a conflict the server did not
  resolve, `sdk.LandConflict(err)` returns the conflict package,
  `Repo.ConflictSides` reads the sides (a side that cannot be read is nil,
  with its reason in `Unreadable`) and the proposal of a file that has one,
  and `LandOptions.Resolution` (start from `sdk.ResolutionFor`, or from the
  proposals with `sdk.ResolutionFromProposals`) lands the agent's resolution. `Land`
  refuses a resolution whose request exceeds `sdk.MaxLandRequestBytes`
  (4 MiB) before sending it.
- CLI: `graft land --resolve`, `graft commit --land BRANCH --resolve`.
  `graft land` prints the package when the land fails on conflicts, whether or
  not `--resolve` was set. `--conflicts-dir DIR` writes it without requiring
  automatic resolution: `DIR/files/<path>` is the merge with git's
  diff3 markers (the clean changes of both sides applied), the sides are in
  `DIR/sides/base/<path>`, `DIR/sides/ours/<path>` and
  `DIR/sides/theirs/<path>`, a proposal in `DIR/proposals/<path>` (copy it
  over the file to land it as yours), the package in `DIR/conflict.json`. Each
  directory mirrors the tree of the merge, so no repository path can collide
  with a side, another path's side or the package. A side the package gives no
  object ID (see `unavailable` below), or that the server cannot return, is
  left out and reported; it fails neither its file nor the directory, and it is
  never dropped silently. When the CLI can merge the file (ours and theirs
  text, the base text or absent, and a diff within bound), the file to edit is
  the merge with diff3 markers. Otherwise (a binary side, a side that is deleted
  or unavailable, a diff too large) it is one conflict between diff3 markers
  that holds every side: the text of each text side, and for a binary side a
  `graft-conflict-pointer:` line naming its file under `sides/`, with a note on
  the marker line of a side that is binary, deleted (absent, with a base),
  absent (without a base) or unavailable, and on the base's marker line `none`,
  with "both sides added the file" only when both sides are there. The marker
  rule of the server refuses it until the agent resolves it, and
  `graft land --resolved` refuses a file that still holds a pointer line, so
  that a pointer can never land in place of a binary side; the server refuses
  such a line too, so a client of the SDK gets the same rule. Both accept the
  line when a side of the conflict has one of its own (a file that documents
  the line, say). `DIR` must not
  hold a conflicts directory yet: one being edited is never overwritten. A
  path that cannot be a file under `DIR` on this system (a backslash, on
  Windows a colon or a name the system reserves) is an error, and so are two paths that
  the file system takes for one file (names that differ only in case on a
  case-insensitive file system). Edit the files under `DIR/files`, then
  `graft land --resolved DIR` lands them: a present file is written, with the
  file mode of the tip's side, else of the landed commit's, else of the base's;
  an absent one is deleted. A path that is a file on none of its sides (a
  submodule) can only be deleted, by removing its file, or resolved outside
  graft.
  For a manual resolution without calling the server's resolver:

  ```shell
  graft land --conflicts-dir /tmp/graft-conflicts
  # Edit /tmp/graft-conflicts/files/<path> to resolve every conflict.
  graft land --resolved /tmp/graft-conflicts
  ```

  Against an older server that returns no conflict package for an ordinary
  land, upgrade the server or opt into `--resolve` to obtain one; the latter
  may invoke its configured resolver.
- RPC: `UserLandRequest.resolve_conflicts` (field 10). The response carries
  `resolved_paths` (field 7) and `resolver` (field 8). The re-land is
  `UserLandRequest.resolution` (field 11).

## Flow

1. `UserLand` attempts the land as usual (rebase or merge onto the tip).
2. On a merge conflict the server lists the conflicting files with their base,
   ours and theirs sides (merge base of the tip and the landed commit; blobs
   and modes read through the object reader). The whole request is refused
   when resolution is not enabled, the actor may not resolve, there are more
   than `max_files` files, the history is criss-cross, or the merge is unclean
   without a conflicting file (a directory rename split, say), which the
   package then describes by the merge's messages.
3. It decides for each file whether it may be sent (see
   [What is never sent](#what-is-never-sent)), sends the others to the
   resolver in one request (protocol below), and validates the answer (see
   [Validation](#validation)).
4. Only when every conflicting file is resolved and the decision lands it (a rule's
   resolution does; a model's resolution only after `verify.command` passed on the exact
   tree that lands, see [Proposals](#proposals-and-when-a-models-resolution-lands)), it
   builds the merged tree from the merge-tree result, writes the resolved
   files as new blobs or deletes them, and creates a merge commit with parents
   `[tip, commit]`. The message is `merge_message` or
   `Merge <short commit> into <branch>`. Trailers:

   ```
   Graft-Resolved-By: graft-resolve/trivial,union
   Graft-Resolved-Path: docs/CHANGELOG.md
   Graft-Resolved-Path: pkg/config.go
   ```

   The committer is the requesting user.
5. The compare-and-swap update runs as in a normal land. If the branch moved
   in the meantime, the server retries from the top and resolves again
   against the new tip.

Otherwise (resolution disabled or refused, a file that is never sent, an
invalid answer, a timeout, an unresolvable file, or a file a model resolved
that `verify.command` did not pass, or that no `verify.command` checks)
the land fails with the normal `merge_conflict` error, the reason is logged,
and the error carries the conflict package as a second detail (see
[The conflict package and the re-land](#the-conflict-package-and-the-re-land)).
Nothing lands partially: the branch is never left in a partial state.

### Proposals, and when a model's resolution lands

A model's resolution lands only through the operator's verification
(PLAN10 14 and 15.9): when `verify.command` is configured and passes, out of
the lane, on the exact tree that would land (see "Optional verification of a
resolution"). Without a `verify.command` the server never lands what a model
resolved: the agent decides. Every resolved file says what made it, in `origin`
(see "Response"): `rule` or `model`. The check is fail-safe: a file is a rule's
only when it says so, and anything else is a model's, whatever the name of the
resolver. graft-resolve is fail-safe in the same way, one layer before the
server: it marks the files of trivial, union, rerere and the command strategies
(unless a command is configured as a model, see below) as `rule`, and every other
strategy's files as `model`, with the older `"proposal": true`: the `llm`
strategy, and every strategy that is registered from another package. A
strategy is a rule only when it says so, by implementing the exported
`resolve.RuleStrategy`; that claim is the operator's trust, and is never to be
made for a strategy that asks a model or reads what a model wrote. A land of an answer that holds a model's file and
does not land (no
`verify.command`, a verification that failed, or none finished in time) fails
with the package: the model's files are
`PROPOSED`, the files the rules resolved keep `RESOLVED`, and every file the
answer resolved carries its `proposal` (action, blob OID, mode, resolver, and
the explanation cut to 512 bytes). An answer that does not land for another
reason (a file left to the agent, a mode no side has) is kept the same way, so
that the agent can land the rules' files as they are.

The proposals are kept under a hidden reference,
`refs/graft/proposals/<first 16 hex of sha256(branch)>/<commit>`, named in the
package's `proposal_ref`: a commit whose tree holds the proposed files by
path, with parents `[tip, commit]`, whose message names the resolver and
`Requested-By:` the actor of the land, and whose date is the server's clock
(never the request's timestamp). One reference per branch and landed
commit, replaced by every failed land of that commit. The server writes it
itself, through the reference transaction like any update (and so the storage
core in authoritative mode), and refuses to write any other reference this
way. The agent reads the proposals by OID with `GetBlobs`. Nothing under
`refs/graft/` is visible to an agent: no ref RPC lists it or accepts a name
under it, upload-pack does not advertise it, and no agent may update it,
whatever the policy says (see [AGENT-SECURITY](AGENT-SECURITY.md)). An agent
the policy does not let update the branch gets `PermissionDenied` before
anything is resolved, and no proposal is kept for it. Every land that asks for
resolution or brings one prunes the branch's proposals older than
`proposal_ttl` (default 7 days) and keeps at most 20, and the housekeeping
pass prunes those of every branch, idle or deleted ones included; the agent's
land of the change drops that change's proposal. The proposals are an aid and
the package is the contract: when they cannot be stored, the package still
comes, each file of the answer saying `proposal not stored: <cause>`, and the
failure is logged and counted (`graft_land_proposal_write_errors_total`).

Every resolution is logged at info level as `graft: conflicts of land
resolved`, with the branch, the tip, the landed commit, the merge commit, the
actor, the resolver name, the resolved paths and the `explanation` of the
answer (cut to 4 KiB). The log records the resolution, not the landing: if the
branch update then fails or is retried, a later entry follows.

## Protocol v1

Transport: the request is JSON on stdin (command) or the body of an HTTP POST
(`url`). The response is JSON on stdout or the HTTP response body.

Request (server to resolver):

```json
{"version": 1,
 "repository": {"storage": "default", "relative_path": "x.git"},
 "branch": "main", "tip": "", "commit": "<oid>", "merge_base": "",
 "messages": {"tip": "", "commit": "<landed commit message>"},
 "actor": "agent-7",
 "files": [{"path": "a/b.go", "kind": "content",
            "base":   {"oid": "<oid>", "mode": "100644", "content": "<base64>"},
            "ours":   {"oid": "<oid>", "mode": "100644", "content": "<base64>"},
            "theirs": {"oid": "<oid>", "mode": "100644", "content": "<base64>"}}],
 "limits": {"max_file_bytes": 1048576},
 "capabilities": ["origin", "proposal", "hold", "verify"]}
```

- `capabilities` says what the server does with the answers (an additive field of protocol v1): `origin` it reads the
  origin of a file, `proposal` a file that is not a rule's is a proposal that lands only on a verified tree, `hold` it
  never lands a file with a `hold`, `verify` it verifies the exact tree that lands. A request without it comes from an
  older server, and `graft-resolve` gives it no model's resolution (see "Version skew" below).
- `rules_only` (additive, optional) asks the resolver to run only its rules, the strategies whose answers are a rule's, and
  never to call a model: the lane of the server sends it when it resolves a member inline (see "Out of the lane"). A
  resolver of this release that honours it says so with `"rules_only": true` in its response; the request then
  announces no capability but `origin`, so that a `graft-resolve` that gates the models on the capabilities refuses a
  model even if it ignored the field; what the rules do not
  resolve is left unresolved, as a failure of the chain. A server that gets no echo is talking to an older resolver,
  whose answer it drops, and it never asks again for the life of the process.
- `ours` is the current branch tip side. `theirs` is the change being landed.
- `tip`, `merge_base` and `messages.tip` are always empty since this version;
  they stay in the protocol for compatibility. The server reuses a file's
  answer at any tip where the file's sides are the same (see "Out of the lane"
  below), so an answer for a file must depend only on the file: its path, its
  sides and their modes, the change's message and the resolver's own
  configuration, never on the tip.
- Any side may be `null` when it is absent (for example in an add-add
  conflict the base is `null`).
- `kind` is one of `content`, `modify-delete`, `add-add`, `mode`.
- `path` is a JSON string, so it carries Unicode text, not bytes. Git paths
  can hold any byte, and a JSON encoder replaces invalid UTF-8 with U+FFFD,
  which would hand the resolver another name. The server therefore never
  sends a path that is not valid UTF-8: a conflict in such a path (for
  example a Latin-1 file name) is not resolved and keeps the normal
  `merge_conflict` error.

Response (resolver to server):

```json
{"version": 1, "resolver": "graft-resolve/trivial",
 "files": [{"path": "a/b.go", "action": "write", "mode": "100644", "content": "<base64>", "origin": "rule"}],
 "explanation": "free text"}
```

- `action` is `write` (with `mode` and `content`) or `delete`.
- `origin` says what made the file: `rule` (an operator's rule: trivial, union,
  rerere, command) or `model` (a model). **It is fail-safe, and a change for
  resolvers that do not set it: a file without `origin`, or with any other
  value, is a model's.** Only a file marked `rule` lands without the
  operator's verification of the tree (see "Optional verification of a
  resolution"); a model's file lands only if `verify.command` passed on the
  tree that lands, and is otherwise proposed. `"proposal": true` is the older
  mark of a model's file, still read: it means `model` whatever `origin` says.
  The name in `resolver` decides nothing: it may be a list joined over
  rounds, or cut to `,+N`, and a name that says `llm` makes no rule's file a
  model's.
- `report` (per file, optional) is the composition report of a model's
  resolution: lines of text that say what it keeps, drops and repeats of the
  two sides' changes. It is a report: nothing in it decides whether anything
  lands. The server shows it to the agent as the start of the explanation of
  the file's proposal (cut to 512 bytes). At most 16 lines are produced by
  graft-resolve.
- `resolver` names the resolver. It goes into the `Graft-Resolved-By`
  trailer, so it must be 1 to 128 bytes of printable, single-line UTF-8.
  The server answers with one `resolver` for a land it resolved in rounds (the
  files of a round that were resolved earlier keep their resolver): the
  distinct names of the rounds, joined by commas in the order they were made
  (`graft-resolve/trivial,graft-resolve/llm`), and, when the list would exceed
  128 bytes, the names that fit followed by `,+N` for the N that do not. It is
  always one line, and one `Graft-Resolved-By` trailer carries it.
- `explanation` is free text. It is logged with the resolution (cut to
  4 KiB) and never interpreted.

Failure signals:

- The command exits non-zero. Its stderr is the reason, unless its stdout is
  an error response (below).
- HTTP `422` with an error response. The conflict is unresolvable.
- Any other HTTP status, or invalid JSON, is a failure.

The error response may give the outcome of every file of the request:

```json
{"error": "unresolved conflict: ...",
 "files": [{"path": "a/b.go", "outcome": "rejected", "reason": "syntax check: ..."},
           {"path": "c.go", "outcome": "resolved"}]}
```

`outcome` is `resolved` (resolved, but the request was not resolved as a
whole), `rejected` (a result failed a rule or a check) or `failed`. The server
uses the list only when it has exactly one entry per requested path, each with
a known outcome; otherwise every sent file fails with the overall reason.
`graft-resolve` writes this response on a refusal, on stdout in command mode
and as the 422 body in HTTP mode. A resolver without `files` keeps working.

A second operation, `check`, asks the resolver to check an agent's resolution
with the checks of its own answers (see
[The conflict package and the re-land](#the-conflict-package-and-the-re-land)).
The request is a resolve request with `"operation": "check"` and the agent's
files in `resolution` (the shape of the response's `files`). The answer is:

```json
{"version": 1, "operation": "check", "resolver": "graft-resolve",
 "files": [{"path": "a/b.go", "ok": false, "reason": "syntax check: ...", "syntax": "checked"}]}
```

with one verdict per file of `resolution`, in its order. `syntax` is
`checked`, `unchecked` (a side does not parse with the grammar) or `none` (no
grammar, or a deletion). An absent `operation` means `resolve`, so existing
resolvers keep working; one that answers a `check` with anything but a check
response makes the server refuse the resolution.

## Validation

The server accepts an answer only if all of these hold:

- `version` is 1.
- `resolver` is 1 to 128 bytes of printable, single-line UTF-8.
- Every conflicted path is answered exactly once, and no other path appears.
- Modes are one of `100644`, `100755`, `120000`, and a written mode is one of
  the modes of the file's sides: a merge does not turn a file into a symbolic
  link or an executable that no side asked for. A file that breaks this is
  `REJECTED`, and the land fails with the package.
- No symbolic link is named `.gitmodules`, `.gitattributes`, `.gitignore` or
  `.mailmap` in any directory, as git fsck and checkout require, nor any name
  a file system takes for one of them, by git's rules: other case and the
  code points HFS+ ignores; on NTFS trailing dots and spaces, the 8.3 short
  name (`GITMOD~1` to `~4`) and the hashed short names (such as `gi7eb~12`).
  A name with an NTFS stream (`:stream`) is cut there first, which refuses
  more than git does.
- Every content is at most `max_file_bytes` after decoding.
- Text files contain no conflict marker lines. A marker line is one that
  starts with `<<<<<<< ` or `>>>>>>> `, or one that is `=======` followed by
  nothing but whitespace (a trailing CR is ignored). This is the rule git uses.
  Lines such as `========` or `=======x`, for example Markdown or RST heading
  underlines, are accepted by the server. Files containing a NUL byte are
  binary and are not checked for markers.
  `graft-resolve` applies the same rule to every strategy's result (including
  the `llm` strategy), so it neither sends an answer the server would reject
  nor refuses a file whose heading underlines are made of `=`.

A rejected answer is a failure, not a partial merge.

## What is never sent

The server decides for each conflicting file, before the request, whether it
may be sent. A file that may not goes to the agent, and the conflict package
gives the reason. The other files are still sent. In order:

1. The path: a line break or NUL byte (a commit trailer cannot carry it), not
   valid UTF-8 (the protocol cannot carry it), or denied by `paths_allow` /
   `paths_deny`, for the path and for every path a side comes from (a rename
   brings a side from another path).
2. The sides: no side under the name (a file/directory conflict), no base and
   a missing side, or a side that is not a file (a submodule).
3. large: a side over `max_file_bytes`, decided from its size before it is
   read.
4. binary: a NUL byte in a side.
5. lockfile: the base name is `package-lock.json`, `yarn.lock`,
   `pnpm-lock.yaml`, `poetry.lock`, `Cargo.lock`, `go.sum`, or one of
   `lockfiles`.
6. generated: `linguist-generated` is set for the path by the tip's
   attributes (one `git check-attr --source <tip>` for all paths), or a side's
   first 10 lines match `Code generated .* DO NOT EDIT\.`, `@generated`,
   `DO NOT EDIT`, `(?i)auto-generated` or one of `generated_markers`.
7. minified: the base name matches `*.min.js`, `*.min.mjs`, `*.min.css`,
   `*.bundle.js` or one of `minified.globs`, or a side larger than 2 KiB has an
   average line longer than `minified.average_line_bytes` (300) or a line
   longer than `minified.longest_line_bytes` (5000).

Secrets are not a reason: a file's content, secrets included, goes to the
resolver when the rules above let it. Use `paths_deny` for paths that must
never leave the server. `graft-resolve bench` applies the same rules (3 to 7);
both are tested against `internal/graft/resolver/testdata/never-send.json`.

## Optional verification of a resolution

`[graft.land.resolver.verify]`, off by default, runs a command on the merged
tree of every resolution before it lands, a resolver's answer or an agent's
resolution alike:

```toml
[graft.land.resolver.verify]
enabled = true
command = ["/usr/local/bin/graft-check-build"]
timeout = "5m"
```

The server writes the merged tree (the resolution included) into a
temporary directory byte for byte, from the entries of `git ls-tree -r` and the
contents of `git cat-file --batch`, with their modes, the symbolic links as
links and the submodules as empty directories. No attribute is applied: not
`export-ignore`, which would hide a file from the command (`git archive`
honours it), nor `export-subst`, `eol`, a filter or a smudge. The directory is
read back and compared with the listing, the content of every file included (it
is hashed as a git blob and must be the object ID of the tree), and a difference
fails the checkout closed. The listing and the blobs are streamed, not held in
memory, and the checkout refuses, with the reason in the package and without
running the command: a tree over `verify.max_tree_entries` entries and
directories made, `verify.max_tree_depth` components in a path, or
`verify.max_tree_bytes` bytes of files and link targets (checked per entry,
before that entry's content is read, so the earlier entries are already
written), or paths that take more than 128 MiB of memory, a path component that names `.git` on any file
system (the command may run git there, and a `.git` of the tree would be its
repository), and an entry under a symbolic link, also when a case-insensitive
file system would see the same name. The checkout and the command run inside
`verify.timeout` together. It then runs `command` there, with a minimal environment
(`PATH`, and `HOME` and `TMPDIR` in a directory of their own). A non-zero exit or a
timeout rejects the resolution: the land fails, every file of the package is
`REJECTED`, and the reason is `verification failed:` with the exit status and
the first 4 KiB of the output. That is the verification of a resolution made by
rules and of an agent's, which runs in the branch's lane, on the merged tree of
the lane's tip.

**For a model's resolution the verification is the gate that lets it land**
(PLAN10 15.9), and it runs out of the lane:

- The server builds the tree that would land (the merged tree of the tip and
  the change, the model's files written in) and runs `command` on it with a slot
  of the verify pool (`verify_concurrency`, default 2;
  `graft_resolver_verify_slot_wait_seconds`). The lane never runs it.
- In the lane, the model's resolution lands only if the tree it would write is
  a tree that passed (compared by tree object ID, not by tip). It is signed
  `Graft-Resolved-By: <resolver>` and `Graft-Checked-By: verify:<command>`.
  The trailer names what ran: the syntax and operator checks run inside the
  resolver and are not named, and the composition report is not a check.
- If the branch moved to another tree, the land goes back out: it rebuilds the
  tree from the same answers (the resolver is called again only for files whose
  sides changed), verifies the new tree, and tries the lane again.
- If the verification fails, the model's files stay `PROPOSED`, with the reason
  `verification failed:` and the output. If the deadline ends before a tree could
  be verified and landed, the land does not fail for the agent: the files stay
  `PROPOSED` with the reason `could not verify before the branch moved again`
  (or `before the deadline of <t>`), and the package carries the proposals. The
  deadline is `timeout` plus `verify.timeout`.
- Nothing is verified for a land that cannot land anyway (a file refused, a
  file left to the agent).

**A verify command such as a build or a test suite executes code from the
merged tree: code that agents and the AI wrote, run as the gitaly user on the
server.** The server only limits the environment of the command (`PATH`,
`HOME`, `TMPDIR`); that is not a sandbox. The command can read what the gitaly
user can read, reach the network, and leave processes behind if it forks
away from its process group. Run it inside an isolation boundary that you
control: a container, or a separate unprivileged user without network
access. For example, a wrapper that runs the build in a throwaway container
with the checkout mounted read-only and no network:

```sh
#!/bin/sh
# /usr/local/bin/graft-check-build: run from the checkout by the server.
exec docker run --rm --network=none --read-only --tmpfs /tmp \
  --user 65534:65534 --memory 2g --pids-limit 512 \
  -v "$PWD":/src:ro -w /src golang:1.26 go build ./...
```

**Since a model's resolution can land through it, the command now runs code
that a model wrote, from conflicts in repository content that agents control.**
A hostile agent can put text in a file (a comment, a string, a README) that is
written for the model to read, a prompt injection, and what the model returns
is then run by your tests. Treat the command as running untrusted code: for any
setup where the AI lands (a server AI and a `verify.command`), use the
container wrapper above as the default, not as an example: `--network=none`,
the checkout mounted read-only, no credentials in its environment or on its
file system, resource limits, and a separate unprivileged user. A command that
needs the network or a secret to pass is a command a model can steer into
leaking it.

It also costs time on every resolution, which is why it is off by default;
the trailers and `graft-verify` remain the safety net.

## Finding and reverting AI resolutions

Every resolution carries its trailers, so it can be found and undone:

- `Graft-Resolved-By` names the resolver (`graft-resolve/llm`, `agent/<actor>`)
  and `Graft-Resolved-Path` each resolved path; `Graft-Checked-By` says that a
  resolver checked an agent's resolution. In a clone, `git log
  --grep='^Graft-Resolved-By:'` lists them; `graft show --json REV` prints the
  trailers of one commit.
- The server logs every resolution at info level (`graft: conflicts of land
  resolved`, with the actor, the resolver, the paths and the explanation).
- `graft-verify` ([VERIFY.md](VERIFY.md)) tests the tip after landing,
  bisects a failure to the commit that broke it and reverts it with
  `UserRevert`, whether it was an AI resolution or not.

## Out of the lane

A resolution that takes minutes must not hold the branch's lane, so the server calls the resolver before a land
enters the lane, never while the lane is held for long. The one exception is the rules, which the lane resolves
itself within `inline_timeout` (default `1s`), as v0.2.0 did ("Rules in the lane" below). With it on (the default,
without `verify.enabled`) a land enters the lane first, and the steps below are for what the rules did not resolve:

1. The server merges the branch's current tip and the change in a quarantine it
   discards, and resolves the conflicting files there: the policy first, the
   classification, one request with a resolve slot, the validation and the
   decision. Each landable answer for a file is kept by the file's key: its
   path and its three sides, each by object ID and mode.
2. An answer that does not land (a model's proposal, a refusal, a file left)
   never enters the lane: the land fails with the package, and the proposals
   are kept.
3. A landable answer enters the lane, which merges its own tip and the change
   and writes the merge commit when every conflicting file has an answer with
   its key; the lane calls the resolver for no model, and for a rule only inline ("Rules in the lane" below). A tip
   that moved on other files costs nothing. When a file changed (or is new), the lane hands the land back, and
   the server resolves only that file, out of the lane, and enters the lane
   again.

A round is lost when another land changed one of the files. The rules do not wait for that:

* **Rules in the lane.** Without `verify.enabled`, a land enters the lane first, and the lane resolves the files that
  have no verdict itself, as the rules (trivial, union, rerere, a command that is a rule), within `inline_timeout`
  (default `1s`; `0s` turns it off). The policy is checked first, as out of the lane: an agent that may not update the
  branch gets nothing resolved. It sends the request with `"rules_only": true`, and `graft-resolve` never calls a
  model for it. What a rule cannot resolve, or does not resolve in time, is handed back and resolved out of the lane as
  described above. The lands on a hot file ride the same batches as the others, as they did in v0.2.0. The lane is
  bounded: it never waits for a slot (the calls have a pool of their own, so a model that holds the resolve slots does
  not stop the rules; with it taken the member goes out of the lane), and the time it spends in inline calls is charged
  to the batch attempt, all of it, a call that succeeds as well as one that fails or is cut at `inline_timeout`: once
  the batch has spent `inline_timeout` the other members go out of the lane without a call, so a batch stalls once,
  not once per member (fast rules, milliseconds, do not notice). A land the rules cannot resolve (the resolver
  refuses) is resolved out of the lane for the proposals and ends there, without a ticket (it gives back the one it
  took if it had queued for it: without a verification a model's resolution never lands). A resolver that does not
  echo `rules_only` is an older one: its answer is dropped and the lane never asks again for the life of the server
  process.
* **A queue for the rest.** A land that loses a round out of the lane (a verification, or a rule that takes more than
  `inline_timeout`, or a file that changed meanwhile) takes a ticket on its files, first in first out, and keeps it until
  it ends. It is the only land of the branch that resolves those files again; the others wait without calling the
  resolver, and the lane hands back any other land whose verdicts were made out of the lane. The holder that lost a
  round out of the lane holds its files with priority until it is done: the lane also hands back the inline resolutions
  of the others on them, which would change the files under its verdict at every round (a rule slower than
  `inline_timeout` on a hot file would never land); they wait behind it, and go back to the lane when it is done. Apart
  from that the rules in the lane do not wait for a ticket, and a land that waited for its turn goes back to the lane.
  The others wait behind the holder for as long as its resolution takes, at most up to its deadline (the resolver's
  `timeout`), after which they end with the package at their own. A holder whose set of files grows gives its tickets
  back and takes the larger set at the tail: it loses its place in the queues, and its priority until it is back in
  the lane.
  A land that waits past its
  deadline ends with the merge conflict and its package, its files `NOT_ATTEMPTED` with the reason `the time ran out
  waiting for the other lands on the same files`.
  The tickets are per file, or per branch with `verify.enabled` (the tree that is verified is the branch's). They are in
  the memory of the server process, like the lanes: with several nodes on one branch (authoritative storage) the lands
  go on in rounds across nodes, without this order.

The rounds are bounded by the resolver's `timeout` for the whole land, and by `resolve_rounds`, a safety cap that the
order above does not reach. The limits, stated: a land that holds the ticket of a file loses a round to a land that
changes the file without a conflict and lands first (once per such land); with a verification every resolution is
verified out of the lane on its own tree, in turn, and a land that moves the branch meanwhile makes the verified tree
stale and the verification is repeated (the cost is measured in PLAN10 15.8). In every case the end is bounded by the
deadline, with the merge conflict and its package: the files that changed under the land are `NOT_ATTEMPTED` with the
reason `the files changed again before they could be resolved`, the others keep their verdicts (a model's `PROPOSED`).

`verify.command` runs out of the lane, whatever made the resolution (a rule, an agent, a model): on
the exact tree that would land, with a slot of the process's verify pool, before the lane, which only
compares the tree it would write with the one that passed. A land whose tip moved after its tree was
verified is verified again; at the deadline the files of a rules' land are `NOT_ATTEMPTED` with the
reason, and an agent's resolution gets the package with "could not verify before the deadline". A
verification costs, per process, at most `verify_concurrency` times the inodes and bytes of
`verify.max_tree_entries` and `verify.max_tree_bytes` on disk and the paths of the tree in memory;
with `TMPDIR` on tmpfs that disk is memory.

## The conflict package and the re-land

When a land fails on conflicts that can be listed, including a land that did
not set `resolve_conflicts`, the `merge_conflict` error carries a second detail
after `UserLandError`: a `LandConflict`. Clients that know only `UserLandError` see
the error unchanged. Without `resolve_conflicts` or a caller's `resolution`,
the server calls no resolver: every file is `NOT_ATTEMPTED`. The package
reason is `conflict resolution was not requested`. It describes the exact tip and commit of the failed
attempt, even if the branch has since moved. A rebase can conflict on an
intermediate commit although the merge of the two endpoints is clean; that
case keeps the original merge conflict without a package. The package holds:

- `tip`, `commit` and `merge_base` (empty when there is none);
- both commit messages, cut to 4 KiB each, with `messages_truncated`;
- `reason`, when the server did not attempt the request at all;
- every conflicting file: the path, the kind, each side's mode and blob OID,
  the conflicting hunks, the outcome and its reason (cut to 512 bytes), and
  the resolver's `proposal`, when it made one;
- `proposal_ref`, the hidden reference that keeps the proposals;
- `messages`: git's messages about the merge (the paths, the type such as
  `CONFLICT (rename/delete)`, the text), which also describe a conflict that
  has no conflicting file.

A side has a blob OID only when the blob is in the tree of a merge base, the
tip or the landed commit, which the repository keeps. A side that the merge
wrote for the conflict alone (such as the content merge of a renamed file, or
the virtual base of a criss-cross history) is discarded with the failed land,
so it has no OID and `unavailable` says why, as does a submodule side (a
gitlink names a commit, not a blob). A file with an unavailable side has no
hunks (`hunks_omitted` says so): they would name lines the agent cannot read.

Each hunk gives line ranges of each side, 1-based and half-open (`[start,
end)`), each counting the lines of the side it names. An empty range is an
insertion point before line `start`. When a content or add/add conflict has
no hunks or only some, `hunks_omitted` says why: a side over
`max_file_bytes`, a binary side, a diff over the cost bound of one file (more
than about 2800 changed lines on a side), more than 256 conflicting regions
(the first 256 are listed), or a package bound below. The package is bounded:
at most 1000 files (a merge with more gets a package without files, whose
`reason` says it cannot be resolved through a land), 1000 messages, 64 MiB of
sides read and 64 Mi cells of diff work for its hunks, and 4 MiB encoded
(over it the hunks are left out, and then the files). The outcomes are:

| outcome | meaning |
|---|---|
| `NOT_SENT` | never sent (see above); the reason says why |
| `REJECTED` | the result failed a rule or a check, e.g. a syntax error at line:column |
| `FAILED` | the resolver failed on it: a model error, `AI budget exhausted`, no strategy applied |
| `RESOLVED` | resolved by rules, but the land failed because another file was not; see `proposal` |
| `PROPOSED` | a model resolved it and it did not land (nothing verified it, the verification failed, or none finished in time): proposed; see `proposal` and the reason |
| `NOT_ATTEMPTED` | the request was not attempted; see `reason` |

The agent reads the sides by OID with `GetBlobs`, resolves every file, and
lands again with `UserLandRequest.resolution` (field 11): the `tip` and
`commit` of the package and, for every conflicting path, `write` (mode and
content) or `delete`. The server accepts it only when:

- `resolution.commit` equals `commit_id`, and the strategy is not
  `STRATEGY_FF_ONLY`, since the resolution lands as a merge commit (otherwise
  `InvalidArgument`);
- `resolution.tip` is the branch's current tip (otherwise the land fails
  with a fresh package for the new tip);
- it resolves exactly the conflicting paths, each once;
- every file passes the acceptance rules of an answer (modes, size, conflict
  markers, no `graft-conflict-pointer:` line unless a side of the conflict has
  one of its own, no symbolic link named like a file git reads from the tree),
  and writes a mode one of the conflict's sides has;
- the merge has at most 1000 conflicting files, the bound of a package (a
  merge with more cannot be resolved through a land, and nothing is read or
  checked);
- every conflicting path, and the actor, can be written in a commit trailer:
  one line of printable UTF-8 (otherwise `FailedPrecondition`; such a path is
  never sent to a resolver either, so its conflict cannot be resolved through
  a land);
- when a resolver is configured, the files that may be sent to it pass its
  `check`: the syntax check and the configured checks, as an `llm` answer.
  A check that cannot run fails the land, with its cause in the package.
  The check runs before the land enters the branch's lane, against the tip
  the resolution was made against, so that no branch is held while an
  external process runs; the lane only applies its verdicts (if the branch
  moved meanwhile, the resolution is refused with a fresh package, as it would
  be anyway). It merges the tip and the change in an object quarantine that is
  then discarded, so it leaves no object in the repository, in every storage
  mode. When the branch is no longer at `resolution.tip`, or the change has
  already landed, nothing is checked: the lane refuses the resolution (tip
  moved) or answers at once (already landed), reading the tip again itself.
  At most `check_concurrency` checks run at once; a check waits for a slot
  within its deadline. The whole check has one deadline, the resolver's
  `timeout`: the wait for a slot and every check request use what remains of
  it, and its expiry fails the check closed. The files go to the check in requests of at most 48 MiB of file data
  each (sides and resolutions, within the 64 MiB a check request may take
  once encoded, and the 128 MiB graft-resolve reads by default), checked one
  after the other, so that neither side holds more than one such request at a
  time; the requests carry no commit message. One file, its three sides and
  its resolution, must fit one request, so the server refuses at startup a
  `max_file_bytes` over about 12 MiB. When the check stops (a request that
  fails, a git error while reading the next files, the deadline), the files
  checked before keep their verdict and the others are not attempted.
  A file with a `syntax: unchecked` verdict lands, and is counted in
  `graft_land_resolution_syntax_unchecked_total`, once, when the land
  succeeds, and in the server log.

The merge commit has parents `[tip, commit]` and the trailers
`Graft-Resolved-By: agent/<actor>` (`agent` without an identity) and, when the
resolver checked it, `Graft-Checked-By: <resolver>`. Without a resolver there
is no `Graft-Checked-By`: the resolution got only the conflict-marker rule and
the server's structural validation. The resolution travels in the request, so
the whole request is bounded by the 4 MiB gRPC message limit.

Only the last paragraph of a commit message is its trailer block (git's rule,
which `graft show`, `graft-verify` and superfan follow), and the server always
writes the `Graft-*` trailers as that paragraph. Lines of a caller's
`merge_message` that look like `Graft-*` trailers are part of an earlier
paragraph, so they never count.

## Strategies of graft-resolve

`graft-resolve` tries an ordered list of strategies per file. The first
strategy whose result passes the configured checks wins. If none succeeds the
file is unresolved: the request is refused as a whole, with the outcome of
every file in the error response (in command mode the program exits 1, writes
the error response on stdout and the reason on stderr).

| Strategy  | Behavior |
|-----------|----------|
| `trivial` | One side equals the base, both sides are identical, the sides differ only in whitespace, or both sides deleted the file. |
| `union`   | Concatenates the non-overlapping changes of both sides for configured globs, for example `CHANGELOG.md`. |
| `rerere`  | Reuses a stored resolution keyed by the SHA-256 of the base, ours and theirs contents. Every resolution of a rule that passes checks is recorded, with its version and origin; a model's resolution is never recorded. |
| `command` | Runs an external merge tool with `{base} {ours} {theirs} {out}` placeholders in temporary files. |
| `llm`     | Optional, see [AI resolvers](#ai-resolvers). |

**A `command` that calls a model is a model.** A command is a rule by default: it
is trusted as deterministic code, and its files land without the operator's
verification of the tree. A command that asks an AI (a wrapper around a model
API, a coding agent), or that reads what one wrote, must be configured with
`origin = "model"`:

```toml
[[strategy]]
name = "agent-merge"
type = "command"
command = ["/usr/local/bin/ai-merge", "{base}", "{ours}", "{theirs}", "{out}"]
origin = "model"   # its files are proposals, and land only if verify.command passes
```

`origin` is `"rule"` (the default) or `"model"`. A model command's answers are
not recorded by `rerere`, and the command is not run for a request for the rules only (`"rules_only": true`, "Out of
the lane"), which is the request of the lane's inline resolution.

**The agreement gate: `agree_with`.** The `llm` strategy alone produces a proposal that lands only if `verify.command`
passes on the exact tree. An operator who wants model resolutions to land without an agent in the loop gets the best
measured result (PLAN10 15.12) by adding a second model from another vendor to the strategy, in the `agree_with`
table (the same keys as `[strategy.llm]`, except `agree_with` itself):

```toml
[strategy.llm.agree_with]
type = "openai"
base_url = "https://openrouter.ai/api/v1"
api_key_env = "OPENROUTER_API_KEY"
model = "z-ai/glm-5.3-flash"
effort = "low"
max_tokens = 8000
timeout = "2400s"

[strategy.llm.agree_with.routing]
only = ["z-ai/fp8"]
```

Both models resolve each file at the same time with the same prompt of record. The first model's resolution is the
one that is proposed. It lands only if all three hold:

1. **the two results are the same file** after a normalization that erases exactly three things: a carriage return before a
   line feed (CRLF and LF are the same), the spaces, tabs and carriage returns at the end of a line, and the blank lines at
   the end of the file (so a final newline or its absence is not a difference). Nothing else is erased: indentation, blank
   lines elsewhere, the order of lines and every other character must be equal. It is the normalization that the
   measurements and the judges used, and it decides only whether the models agree: the bytes that land, and that the
   verification runs on, are the first model's, exactly as it wrote them;
2. **the composition check of both found nothing**: not one drop of a change of a side and not one duplicate, counted in
   full from the check of every region (`LLMResult.Findings`), not read from the text of the report, which is cut at 16
   lines;
3. **`verify.command` passes** on the exact tree, as for every model resolution: `verify.enabled = true` is mandatory
   (the gate does not replace it, it chooses the proposals worth verifying).

When 1 or 2 fails, or the second model gives no usable answer, the first model's resolution is **held**: the response
marks the file with a `hold` reason (`ResolvedFile.Hold`, "the second model disagreed" and why, also in the report),
the server never lands a held file, whatever `verify.command` says (it does not even run it for that land), and the
agent gets the conflict package with the proposal as usual. When the first model fails, the file fails as it does
without the gate. The gate is off by default (no `agree_with` table, no second call). Pick two models from different
vendors: two models of one family make the same mistakes. A `[strategy.llm.budget]` must have an
`[strategy.llm.agree_with.budget]` beside it (a cap that covers one of two models is not a cap), and the spend of both
is counted for the file. The bench (`graft-resolve bench run`) runs the same code, prices the two models from
OpenRouter and splits `--max-usd` between them; its report counts the held answers apart. Upgrade `graft-resolve` and the
server together (see "Version skew" above: a `graft-resolve` with `agree_with` gives no model's resolution to a server that
does not announce `hold`).

What it was worth. The operating point that the measurements support is: **two writers from different vendors that
give the same answer, and the project's own tests or build passing on the exact tree** (`verify.command`). It was
measured on conflicts of open-source repositories that no run had seen, with the gate's two writers (Luna and
glm-5.3-flash); "wrong" is the share of the landed answers that are not a correct merge of the two sides, with the
Wilson upper bound in parentheses (PLAN10 [15.15 to 15.24](PLAN10.md)):

* **Go**, with the tests: 7.7% wrong (upper bound 16.8%) on conflicts never seen before, replicated on a second set
  (8.2% pooled, upper bound 14.1%); with only a successful build of the package: 6.8% (14.9%).
* **Python**, under the PM label: with the tests 3.2% (7.2%); without tests, with the abstention on semantic conflicts
  (a side changes what the other side's code uses): 5.9% (9.2%).
* **TypeScript and JavaScript**, under the PM label: with the tests 4.1% (8.6%); with the project's type check 4.9%
  (9.7%). Without tests it is **not verified**: the label did not pass its calibration on TypeScript (78% against the 80% it
  required).

The limits, in one line: the reference of the Python and TypeScript figures is an AI reviewer (Claude) with a written
rubric, calibrated blind on Go but not a human; the first judges (three models that compare with the developer's
file) over-counted the errors (an audit found 18 of 25 flagged Python answers to be artifacts of that reference), so
the earlier, higher figures are not the ones to quote; and conflicts of merged agent PRs were not measured with the
tests. The figures combine two separate single-model runs, which is what the gate decides; the gate in the code is
covered by tests with fake providers, not re-measured through itself. Cost: two model calls per region.

**Version skew between the server and `graft-resolve`.** A resolution is a rule's only if its file says so (`origin`
`"rule"`, protocol v1); a file without an origin is a model's, which lands only through `verify.command`.

* *An older `graft-resolve` with a new server.* It answers without an origin, so every file it resolves, `trivial` and
  `union` included, is a proposal, and lands only if `verify.command` passes on the exact tree (without one, it never
  lands). Safe, and the rules stop landing by themselves until `graft-resolve` is upgraded.
* *A new `graft-resolve` with an older server.* An older server ignores the origin, the proposal mark and the hold, and
  lands every answer it gets without verification, a held one included. The request therefore says what the server does
  (`capabilities`, an additive field of protocol v1: `origin`, `proposal`, `hold`, `verify`; a server of this release
  sends all four), and `graft-resolve` gives a model's resolution (the `llm` strategy, a `command` with `origin =
  "model"`) only to a server that announces `proposal`, and with `agree_with` only to one that announces `hold` too. For
  any other server the model is not called and the file stays unresolved, with the reason "the server cannot hold or
  verify a model's resolution; upgrade the server". The rules are unaffected. A client of the protocol of your own that
  sends no capabilities gets no model's resolution either.
* *An older `graft-resolve` and the inline resolution of the rules.* A server of this release asks `graft-resolve` for
  its rules only (`"rules_only": true`, see "Out of the lane") when the lane resolves a member inline, and announces
  no capability but `origin`. A `graft-resolve` that gates the models on the capabilities (any since the capabilities
  were added) refuses to call a model for that request, even if it ignores the field. The `graft-resolve` of v0.2.0 has
  no gating: it ignores the field, so it may call a model, and the lane gives the call `inline_timeout` (default `1s`)
  and drops the answer, which is not a rule's, whatever it is. A batch pays at most `inline_timeout` for such calls
  (the lane's budget), and the first answer that does not echo `rules_only` makes the server stop asking, for the life
  of the process (`graft: the resolver did not honour a request for its rules only` in the log). After that the rules
  are resolved out of the lane, with the queue, as before this release. A `graft-resolve` of v0.2.0 that is cut at
  `inline_timeout` while it calls a model for a conflict, without ever answering, is never recognised as an older one,
  so every batch pays the lane's budget for it: set `inline_timeout = "0s"` with the `graft-resolve` of v0.2.0.
  (A resolver that crashes, with a non-zero exit and no refusal in its answer, is taken as a refusal: the land is
  resolved out of the lane for the proposals.)

Upgrade both in the same maintenance window anyway.

**The `rerere` store and the upgrade.** Entries are versioned and record their
origin. The stores of v0.1.0 and v0.2.0 recorded the `llm` answers as well, with
no origin: after an upgrade `graft-resolve` never replays an entry without
a version and an origin as a rule's answer (it skips it, and says so once on
stderr: the conflict goes on to the next strategy). Clear the store on upgrade
to drop the old entries: `rm -r <dir of the rerere strategy>`.

Checks are per-glob commands run on the resolved file in a temporary
directory, with `{file}` replaced by the file name. A failing check rejects
that strategy's result, and the chain continues with the next strategy.

- The file a check sees is named `check` plus the extension of the conflicted
  path when that extension is 1 to 15 letters or digits (`check.go`,
  `check.md`), and plain `check` otherwise. The repository path never reaches
  the command line, so `{file}` is safe inside a shell string.
- Every check whose `paths` match must pass. Checks do not run for deletions.
- A check passes on exit status 0. Tools such as `gofmt -l` exit 0 even when
  they report a problem, so test their output (see the example below).
- `timeout` bounds one check run, `env` lists extra environment variables to
  pass through (see [graft-resolve subprocesses](#graft-resolve-subprocesses)).

Options of the `command` strategy:

| Option | Meaning |
|--------|---------|
| `command` | argv with the placeholders `{base}`, `{ours}`, `{theirs}` and `{out}`. |
| `output` | `"file"` (default): the tool writes the result to `{out}` and must create it. `"stdout"`: the tool's stdout is the result, for tools such as `git merge-file -p`. |
| `timeout` | bound on one run. |
| `env` | extra environment variables passed to the tool. |

Glob syntax of `paths` in `resolve.toml`: `**` alone matches every path, a
trailing `/**` matches a directory and everything below it, and a leading
`**/` matches any number of leading directories, none included (`**/*.go`
matches `main.go` and `a/b/c.go`). Otherwise `*` does not cross `/`, so
`CHANGELOG.md` matches only the top-level file; use `**/CHANGELOG.md` for
every directory.

The response `resolver` names the strategies used:
`graft-resolve/trivial,union`. Strategy names are at most 64 bytes, so
several long names could exceed the 128 bytes the server accepts. The name is
then shortened: the names that fit are listed, followed by `+N` for the `N`
strategies left out, for example `graft-resolve/union-changelog,+2`. The
`explanation` still names the strategy of every file.

Example `resolve.toml`:

```toml
timeout = "5m"                      # whole request

[[strategy]]
type = "trivial"

[[strategy]]
type = "union"
paths = ["CHANGELOG.md"]

[[check]]
paths = ["**/*.go"]
# A non-zero exit rejects the strategy's result. gofmt -l exits 0 even when
# it lists files, so the check tests the output too.
command = ["sh", "-c", "out=$(gofmt -l \"$1\") && test -z \"$out\"", "sh", "{file}"]
timeout = "30s"
```

Unknown keys are errors, so a typo in the file fails at startup.

Run modes of `graft-resolve`:

- Command mode (default): read one request on stdin, write one response.
- HTTP mode: `graft-resolve serve --listen 127.0.0.1:9300 --config resolve.toml`
  with an optional bearer token (`--token-file`). HTTP mode does not limit
  concurrent requests. Listen on loopback or a private network, and set a
  token.
- Both modes take `--max-request-bytes N` (default 128 MiB,
  `protocol.DefaultMaxRequestBytes`). A larger request is refused.
- Both modes answer the `check` operation of the protocol as well as
  `resolve` (see [Protocol v1](#protocol-v1)).
- Without `--config`, only the `trivial` strategy runs.
- `graft-resolve bench ...` measures a configuration on real merges (see
  [Measuring a resolver](#measuring-a-resolver)).

## AI resolvers

The AI setup is the `llm` strategy alone. The deterministic strategies above
(`trivial`, `union`, `rerere`, `command`) are opt-in additions in front of it,
not part of the default AI chain:

```toml
[[strategy]]
type = "llm"

[strategy.llm]
type = "openai"
base_url = "https://openrouter.ai/api/v1"
api_key_env = "OPENROUTER_API_KEY"
model = "openai/gpt-6-luna"
effort = "low"
max_tokens = 8000
timeout = "2400s"   # the whole file: all its calls
```

`call_timeout` in `[[strategy]]` (default `"120s"`) bounds each model call,
one region; `timeout` in `[strategy.llm]` bounds the whole file, all its
calls, and stays the outer bound.

The `llm` strategy is registered by `tools/graft-agents` and uses an
`llm.Provider` configured in `resolve.toml`. Providers are anthropic, any
OpenAI-compatible endpoint, or any command.

**Choosing the model and who serves it.** The operator chooses both; the example above is one choice, not a
requirement. Set `model` for another model; for another endpoint set `base_url` and `api_key_env` too. Through
OpenRouter, `[strategy.llm.routing]` with `only = ["<provider tag>"]` (and `ignore`) pins the providers that may serve
the call, and with it set the call never falls back to another provider. The model that serves a call sees the
conflicting files (the three versions of each file that goes to it, and the commit message of the change), so the
provider you configure receives them: pick it for where you are willing to send your code. Effort and `max_tokens`
matter for a model that reasons: it needs `max_tokens` large enough to hold its reasoning and an answer, and
`call_timeout` long enough for it; a model that cannot finish within them is rejected for that file, never guessed. On
our benchmark `openai/gpt-6-luna` and `z-ai/glm-5.3-flash` (pinned to the provider `z-ai/fp8`) were equally accurate
on the same 100 files at the same price per file (PLAN10 15.12); other models need their own `max_tokens` and
`call_timeout` and should be tried with `graft-resolve bench run` on your own merges first.

The strategy asks the model to apply a patch, one conflicting region at a
time. Its prompt structure follows synthmerge's patch-application design
(Andrea Arcangeli, synthmerge): the structure only, written in our own words,
with our own worked example. The changes that only one side made, or both
the same way, are merged locally; for each region that both sides changed
differently there is one model call, whose request holds:

- the PATCH: THEIRS' change to the region, a unified diff from BASE, with
  `context_lines` (default 3) of context;
- the CODE: OURS' version of the region, with the same context lines (taken
  from the file with every other conflicting region as OURS);
- in the system message, in this order: one worked example of the task (its
  sections have tags of their own, so it never reads as the answer), the
  commit message of the change, THEIRS' (as the intent of the PATCH, at most
  4 KiB; the tip's message is not sent, as in synthmerge), the other
  conflicting regions of the file for reference, and the directives last,
  closest to the answer.

Two things more come with the strategy, with no option: both were measured against the alternatives
(PLAN10 15.10) and the alternatives were removed.

- The regions of the file are those of git's own merge (`git merge-file --diff3`
  on the three versions), so that "no conflict" means what it means to git and
  the model is asked what git calls a conflict. Our own line diff is the
  fallback when git cannot be run or a side lacks a final newline.
- One repair round, only for an answer that is broken: a rule of an answer that
  it breaks (the anchors, the newline, a patch, markers, size, no block), or the
  first syntax error of the assembled file, mapped to its region. The finding is
  quoted back in its exact words, with the previous answer as the block the
  model writes (its patched code between its tags; for a region that was valid,
  with the context lines of the CODE), and only if the request fits
  `max_prompt_bytes`. A region is asked again once at most; the repaired answer
  is kept if it passes the rules (and, for a syntax error, if the file parses).
  An answer that the rules and the syntax accept is never asked for again: what
  the composition check finds in it is only reported.

A proposal also carries the composition report of its answer (what it keeps,
drops and repeats of the two sides' changes, in exact words), as the start of
the explanation of the proposal in the package. It is a report: nothing in it
decides whether anything lands.

A file whose sides all break their lines with CRLF reaches the model with LF,
since a model answers with LF; its answer is read as LF (CRLF in it too) and
the resolved file is CRLF again. A file with mixed line endings is sent as it
is. The model answers in plain text, the patched code between a BEGIN and an END
line that carry the request's random token: no JSON, no response format, and
no explanation. The last such block of the answer is read, since a model may
draft or quote before it answers. The answer is accepted for a region only
when:

- it starts with the context lines before the region and ends with those
  after it, copied unchanged (a model that drops blank lines at a boundary is
  given back up to n-2 of them, n the context lines at that boundary);
- a `\ No newline at end of ...` note appears, if at all, only as its last
  line, where it means that the code ends without a newline;
- what is between them ends with a newline (unless the region ends a file
  that has no final newline) and does not read as a patch (lines starting
  with `+` or `-` when neither the CODE nor the PATCH has such lines);
- it holds no conflict marker, and is within `max_answer_bytes`.

A file is resolved only when every region is: the first region that is
rejected rejects the file, which goes to the agent. A file with more than
`max_regions` (default 20) conflicting regions, or a region whose request is
over `max_prompt_bytes`, is declined before any call. An answer equal to
OURS is kept (taking the tip's version is often right); the bench counts how
often it happens, per file and per region. The PATCH is our own line diff, not git's histogram diff
that synthmerge uses: on the benchmark sample of 300 cases the two differ in
18 of 403 regions.

- The model is not asked how sure it is, and nothing it says about its answer
  decides whether the answer is used: acceptance comes from the checks alone.
  The former `min_confidence` option is gone; a configuration that still sets
  it fails at startup.
- The answer must parse. graft-resolve parses it with the tree-sitter grammar
  of its language, chosen by extension: TypeScript (`.ts`, `.mts`, `.cts`),
  TSX (`.tsx`), JavaScript (`.js`, `.mjs`, `.cjs`, `.jsx`), Python (`.py`,
  `.pyi`), Go (`.go`), Rust (`.rs`) and Java (`.java`). The check asks
  whether the merge broke the syntax: when both sides parse, an answer with
  any ERROR or MISSING node is rejected, and the reason gives the line and
  column of the first one. When a side does not parse, the grammar cannot
  judge the file, and the answer gets the conflict-marker rule only. Examples:
  Flow annotations in a `.js` file; files that a repository declares to be
  another language; syntax newer than the grammar, such as a Java annotation
  on varargs, or Rust's `dyn 'a + Trait`; test files that are invalid on
  purpose. This is a parse, not a build: names, types and imports are not
  checked, and a few things a compiler refuses still parse (a JSX closing tag
  that does not match its opening tag, for one). Every other file gets the
  conflict-marker rule only. The check is always on for `llm` answers and
  does not apply to the other strategies.

  Measured on the human resolutions of the benchmark corpus (3183 cases),
  the check rejects none of the valid ones. 10 human resolutions fail it,
  each with both sides parsing, and all 10 are broken: 6 were committed with
  conflict markers, and 4 do not compile (confirmed with gofmt and by brace
  count). The check gives no verdict on 83 files where a side does not parse:
  76 JavaScript (Flow, and TypeScript test baselines), 3 Go (tests that are
  invalid on purpose), 2 Java and 2 Rust (syntax newer than the grammar).
- The configured checks still run afterwards.
- For large files the strategy sends only the conflict hunks with context and
  reassembles the file locally.

The grammars are C code compiled with cgo (the MIT-licensed
`github.com/tree-sitter/go-tree-sitter` bindings and the grammar modules of
those languages, in the `tools/graft-agents` module only; the gitai server
has no such dependency). Building graft-resolve therefore needs a C compiler
and `CGO_ENABLED=1`, the default when a compiler is installed. A
graft-resolve built without cgo refuses to start with an `llm` strategy
configured, with a message naming that requirement. The syntax check itself
fails closed in such a build (it rejects every file that has a grammar), and
`graft-resolve bench run` refuses to start.

Every setting of `[strategy.llm]` is either honoured by the provider or an
error at startup. None is dropped silently:

| setting | `openai` | `anthropic` | `command` |
|---|---|---|---|
| `model` | sent | sent (default `claude-opus-5-5`) | `GRAFT_LLM_MODEL`, and the reported model |
| `base_url` | the endpoint | the endpoint | error |
| `api_key_env` | the key's variable (default `OPENAI_API_KEY`) | the key's variable | error |
| `command` | error | error | the program (argv, no shell) |
| `timeout` | per call | per call | per call, the program is killed |
| `max_tokens` | `max_tokens` | `max_tokens` (default 16000) | `GRAFT_LLM_MAX_TOKENS` |
| `effort` | OpenRouter's `reasoning: {"effort": ...}`; `minimal`, `low`, `medium`, `high`, `xhigh` | `output_config.effort`; `low` to `max` (default `high`) | `GRAFT_LLM_EFFORT`, verbatim |
| `options` | error | error | error |
| `routing` | OpenRouter's `provider` object, with `allow_fallbacks: false` | error | error |

The `openai` provider's `reasoning` parameter is OpenRouter's. Set `effort`
only for an endpoint that takes it.

The `command` program inherits graft-resolve's environment, and gets three
extra variables for its settings: `GRAFT_LLM_MODEL`, `GRAFT_LLM_EFFORT` and
`GRAFT_LLM_MAX_TOKENS`. The token limit is the call's, else the configured
default. Each variable is set only when its setting is, and inherited
variables of those names are removed. The program reads its own endpoint and
credentials, which is why `base_url` and `api_key_env` are errors for it. It
is expected to honour the variables. In particular, the AI budget counts a
call's output as at most `GRAFT_LLM_MAX_TOKENS`, and a command reports no
usage, so each of its calls is booked at that worst case. An llm
strategy also refuses to start in a graft-resolve built without cgo (see
above).

The provider's API keys are withheld from the model: they are read from the
environment or a file by the provider, never put in the prompt. Everything
else is sent to the provider, so the three versions of a file and the commit
message of the change (the tip's message is not sent) are shared with it, and any secret that they contain goes with them. With
`agree_with` two providers receive them. Do not send
files you would not send to that provider. The server never calls the model.
Turn AI on only if you accept that the resolution is produced by a model.
Every AI resolution carries the `Graft-Resolved-By` trailer, so it can be
audited with `graft log` and reverted with `graft-verify` if it breaks tests.

### Pinning OpenRouter routing

OpenRouter serves a model from several endpoints, at different prices: for
example, a standard tier, a flex tier and priority tiers that cost up to six
times more. Without routing, a call can go to any of them. The
`[strategy.llm.routing]` table of the `openai` provider pins the endpoints a
call may use:

```toml
[strategy.llm.routing]
only = ["openai", "azure", "amazon-bedrock"]          # providers or endpoint tags allowed
ignore = ["openai/flex", "openai/fast", "openai/ultrafast"]  # endpoint tags excluded
```

The names are OpenRouter's provider slugs, or endpoint tags as the `tag`
field of `/api/v1/models/<model>/endpoints` lists them. The provider sends
them as the request's `provider` object, always with `allow_fallbacks:
false`: a call that the allowed endpoints cannot serve fails rather than going
to another endpoint. At startup the provider refuses a routing table that names
nothing, a malformed name, or a name listed twice or in both lists.

The budget is exact only when routing is pinned. Without it, the prices of the
budget must be those of the most expensive endpoint, or a call routed there is
undercounted. `graft-resolve bench run --pricing openrouter` prices each call
at the highest price of the endpoints the routing allows, read at run time. It
reads the names in the way that can only overstate the price: a slug in
`only` admits every endpoint of that provider, its tagged variants included,
while a name in `ignore` excludes only the endpoint whose tag is exactly that
name. It refuses to start when the allowed set cannot be determined: when an
endpoint has no tag, when a name matches no endpoint of the model (most likely
a typo), or when the routing allows none.

### Monthly AI spending cap

An `llm` strategy can carry a monthly cap:

```toml
[strategy.llm]
type = "openai"
base_url = "https://openrouter.ai/api/v1"
api_key_env = "OPENROUTER_API_KEY"
model = "openai/gpt-6.1-sol"
max_tokens = 8000                  # required with a budget: it bounds a call's output

[strategy.llm.budget]
monthly_usd = 50                   # the cap; 0 or absent: none, the spend is still tracked
input_usd_per_million = 2.0        # the model's prices; keep them current
output_usd_per_million = 10.0      # output tokens, reasoning included
request_usd = 0                    # optional: a fixed price per call
state = "/var/lib/graft/resolve-budget.json"
```

Before each model call graft-resolve books the call's worst case: its input
counted as one token per byte of the prompt (an upper bound), plus a margin,
and `max_tokens` of output. If that would take the month's spend past the
cap, the call is not made. The file then fails with the reason "AI budget
exhausted", which the server puts in the conflict package for the agent.
After the call the worst case is replaced by the cost of the usage the
provider reports. A call that the provider refused before generating anything,
with HTTP 400, 401, 402 or 403 (a bad request, no or a wrong key, no credit,
forbidden), is not billed: its worst case is released. A provider that reports
no usage, and every other failure (a 429 or 5xx answer, a timeout, a broken
connection, an error in the middle of a stream), is left at its worst case, as
it may have been billed. So the recorded spend is not below the real one, with
one rare exception: the provider's transport retries of a call share its one
booking, so a 5xx answer that was billed and then retried can be billed twice
while booked once.
A new calendar month (UTC) starts at zero.

The spend is kept in the `state` file (`{"month": "2026-10", "spent_usd":
...}`). Every update takes an exclusive `flock` on `<state>.lock` and
rewrites the file atomically (temporary file, fsync, rename), so the spend
survives restarts and is shared by every graft-resolve process that uses the
same file. The state file needs a platform with `flock` (Linux, macOS, the
BSDs). The prices are configuration values: graft-resolve does not look them
up, and they should be set from the provider's price page: with OpenRouter, the
highest price of the endpoints the routing allows (see above).

The spend is reported in three places:

- In HTTP mode, `GET /metrics` (with the same bearer token) serves
  `graft_resolve_ai_spend_usd` (gauge, the month's spend),
  `graft_resolve_ai_budget_usd` (gauge, the cap) and
  `graft_resolve_ai_budget_exhausted_total` (counter, the calls not made),
  labelled by strategy.
- In command mode, the spend is in the state file.
- In both modes, the answer's `explanation` gives the spend of each strategy
  that answered.

## Measuring a resolver

`graft-resolve bench` measures a `resolve.toml` on conflicts that humans
resolved in real repositories. It needs the grammars (a cgo build).

- `bench mine --corpus FILE --clones DIR --manifest OUT`: clones the
  repositories of the corpus (bare, complete clones), replays every two-parent
  merge with `git merge-tree --write-tree` on its parents, and keeps each
  conflicting file of a benchmark language whose human resolution differs from
  both sides. It leaves out a merge without a single merge base, and a human
  resolution that is not a valid reference: one committed with conflict
  markers, or one that does not parse although both sides do. Each
  repository is saved as soon as it is mined, so a stopped run resumes with
  the next one. The committed corpus
  (`tools/graft-agents/resolve/bench/corpus.json`) lists well-known
  repositories under permissive licenses at a pinned commit; the manifest
  holds object IDs, never third-party code.
- `bench filter`: applies the reference rule to a manifest mined before it.
- `bench syntax`: runs the syntax check on every human resolution and reports,
  per language, the files checked, those that fail, and those the grammar
  gives no verdict on, with the reason of each.
- `bench run --config resolve.toml --manifest M --clones DIR --sample N
  --seed S --max-usd USD --answers-dir A --json-out R`: replays a sample of the
  manifest through the configured chain, as the server would build the
  request (the never-send rules included), and judges each accepted result
  against the human file. The sample is stratified by family, with Rust and
  Java apart, so a language with few cases is taken whole. A chain with a
  model needs `--max-usd`: every call books its worst case first, and a call
  that would pass the cap is not made. The first HTTP 402 of the provider (no
  credit) stops the run: the remaining cases are reported as stopped, with the
  reason, apart from those the cap left out. The model's price is read at run time
  from OpenRouter's endpoints API (`--pricing openrouter`, the highest price
  of the model's endpoints, or of those the routing allows), or given as flags (`--pricing fixed`). The
  answers are kept in `--answers-dir`.
- `bench rejudge --report-in R --manifest M --clones DIR --answers-dir A`:
  judges the stored answers again with the current judge, with no model call.

The outcomes of a case are: correct (equal to the human file after
normalizing line endings and trailing whitespace; exact when byte-identical),
wrong (accepted by the checks, but different), rejected by checks, not sent,
model error, not run (cap) and abstained (no strategy applied, such as the
`llm` strategy on a modify/delete conflict). The error rate is wrong /
(correct + wrong): the share of the results that would land and differ from
the human resolution. The report counts apart the wrong results that differ
in blank lines only, and the accepted results that got no syntax verdict.

## Bounds and failure modes

| Limit | Default | Where |
|-------|---------|-------|
| files per land | 20 | `max_files` |
| bytes per file | 1 MiB | `max_file_bytes` |
| resolver time | 120 s | `timeout` |
| resolver answer (stdout or HTTP body), and a refusal's error response | 2 × `max_file_bytes` × files in the request + 64 KiB | server |
| reason of a refusal (stderr, or `error` of the error response) | 4 KiB | server |
| check answer | 64 KiB + 8 KiB per file | server |
| checks of agents' resolutions at once | 2 | `check_concurrency` |
| resolutions at once | 2 | `resolve_concurrency` |
| verifications of a model's resolution at once; of one repository | 2; 1 | `verify_concurrency`; `verify_per_repository` |
| rounds of a land whose files keep changing | 32 (a safety cap; `timeout` bounds the land) | `resolve_rounds` |
| verification run | 5 min, output kept 4 KiB | `verify.timeout` |
| request body read by `graft-resolve` | 128 MiB (`protocol.DefaultMaxRequestBytes`) | `--max-request-bytes` |
| land request with an agent's resolution | 4 MiB, the gRPC message limit | server; the SDK refuses larger ones |
| package: files, merge messages, encoded size | 1000; 1000; 4 MiB | server |
| package: reasons and merge messages; commit messages | 512 bytes; 4 KiB each | server |
| package hunks: per file; sides read; diff work | 256; 64 MiB; 64 Mi cells | server |
| land attempts | 20 | `max_attempts` of the land |

A resolver that hangs is killed at the timeout. Its stderr is kept in the
logged error, which is truncated. An answer larger than its bound kills the
resolver and is a failure.

Before calling the resolver, the server also checks its own limits. A request
with more than `max_files` conflicted files, or from an actor that `actors`
does not allow, is not attempted at all. A file that breaks a per-file rule
(a side larger than `max_file_bytes`, a path denied by `paths_allow` /
`paths_deny`, the other rules of [What is never sent](#what-is-never-sent))
is not sent, and the other files are. Either way the land fails with the
conflict package unless every file is resolved.

`graft-resolve` must be able to read the requests the server sends. The
default `--max-request-bytes` covers the default server limits (20 files of
1 MiB, three sides each, base64 encoded). Raise it when you raise `max_files`
or `max_file_bytes`.

### Command resolvers

- The command runs with a minimal environment: only `PATH`, `HOME`, `TMPDIR`,
  `LANG` and `LC_ALL` are copied from the Gitaly server. Secrets in the
  server's environment never reach the resolver. A resolver that needs other
  variables, for example an LLM provider key named by `api_key_env`
  (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`), proxy settings (`HTTPS_PROXY`) or
  `SSL_CERT_FILE`, must get them from a wrapper script or read them from a
  file, for example:

  ```sh
  #!/bin/sh
  ANTHROPIC_API_KEY=$(cat /etc/graft/secrets/anthropic) exec \
    /var/lib/graft/bin/graft-resolve --config /etc/graft/resolve.toml
  ```

- On timeout the server kills the resolver process. Processes the resolver
  started itself are not killed by the server. A resolver that starts
  children (merge tools, check commands, LLM CLIs) should put them in its own
  process group or bound them with their own timeouts. After the kill, the
  server waits at most 2 seconds for the output pipes to close.
- A resolver must not leave a background process holding its stdout after
  it exits. The server then waits for the pipe for 2 seconds and reports a
  failure, even when the answer was complete and the exit status was 0.

### HTTP resolvers

- The request is a `POST` with `Content-Type: application/json` and
  `Authorization: Bearer <token>` when `token_file` is set. The token is read
  once at startup. It must be a single line.
- Redirects are never followed, so the bearer token never travels to another
  URL. A `3xx` answer is a failure.
- Only `422` is a refusal (unresolvable). Every other non-`200` status is a
  failure. Both end in the normal `merge_conflict` error, with the conflict
  package.

### graft-resolve subprocesses

Checks and `command` strategies run with a minimal environment too: `PATH`,
`HOME`, `TMPDIR`, `TEMP`, `TMP`, `LANG`, `LC_ALL`, `LC_CTYPE` and `SYSTEMROOT`,
plus the variables named in their `env` option. The provider keys in the
environment of `graft-resolve` therefore do not reach programs that run on
agent-influenced content. On timeout only the direct child is killed; see the
note on process groups above.
