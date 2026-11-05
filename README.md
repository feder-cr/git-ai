<div align="center">
<picture>
  <source media="(max-width: 1239px) and (prefers-color-scheme: dark)" srcset="doc/images/banner-phone-dark.gif">
  <source media="(max-width: 1239px)" srcset="doc/images/banner-phone-light.gif">
  <source media="(prefers-color-scheme: dark)" srcset="doc/images/banner-dark.gif">
  <img alt="gitai, Git designed for AI agents. An animation: the gitai logo; a board of 50 agents racing on one branch, where git lands 100 of 250 changes and gitai lands 200; a merge conflict between two agents (one renamed get to fetch, the other added an argument) resolved by AI, tested and merged; and every change carrying the name of the agent that made it." src="doc/images/banner-light.gif" width="100%">
</picture>
<h3 align="center">Git designed for AI agents.</h3>
</div>

Anthropic built **Scion**, a faster version control system for coding agents,
and described it in
[Redesigning source control](https://www.anthropic.com/engineering/redesigning-source-control):
a server-centric system built for hundreds of agents that read, write and land
code at the same time. Scion is internal to Anthropic and is not available to
the public.

So I built **gitai**: an open-source version of that design that anyone can
run, and that the community can improve and maintain together.

MIT licensed. Vendor neutral: the server never calls a language model, and every
AI feature works with any provider, or none.

<sub>gitai is an independent implementation of the publicly described design. It is not affiliated with or endorsed by Anthropic.</sub>

---

## Why

Classic Git hosting was built for humans who clone a repository, work for hours
and push once. Coding agents work differently: thousands of short-lived tasks,
each touching a few files, each wanting to land in seconds, all on the same
branches. Full clones, merge queues and client-side history walks become the
bottleneck.

Scion's answer is to move the work to the server: clients stay online and fetch
only what they need, history questions are answered (and cached) by the server,
changes land through a compare-and-swap instead of a queue, every update is an
entry in a changeset log, and the repository state lives in a consistent
database plus object storage so servers scale out. gitai implements these
ideas, with normal Git repositories underneath.

## The Scion ideas, implemented

| Scion idea | In gitai |
|---|---|
| **Always-online clients** that fetch exactly what they need | `ObjectService` serves single objects and sparse trees; `graft clone` and `graft sparse` materialize only the paths you ask for. |
| **Source control as an API**: history runs on the server | Blame, path history and ancestry run server-side and are memoized as derived data, computed once per commit. Code search runs server-side too, without an index. |
| **Landing without a merge queue** | `UserLand` rebases a change onto the current tip and lands it with a compare-and-swap; concurrent lands on one branch are batched into a single reference update. |
| **Automatic conflict resolution** | A conflicting land is resolved by a resolver you choose (any LLM provider, a deterministic tool, or none) through a versioned, vendor-neutral protocol. A model's resolution is only a proposal: it lands by itself only if the `verify.command` you configure passes on the exact tree that would land, and otherwise the agent gets the conflict package with the proposal. The deterministic rules you configure need no such condition of their own, and are verified on the exact tree as well when you turn the verification on (`verify.enabled`). |
| **Changeset log** | Every accepted reference update is recorded with its operation and actor in a dense, ordered log that clients list or watch live. |
| **Storage core: a database and object storage, stateless servers** | Each change is a changeset in a write-ahead log in SQLite (one node) or PostgreSQL, with a compare-and-swap per reference; packs live in object storage (file, S3, GCS, Azure; only file buckets are tested). Server nodes are caches that any node can rebuild, so you add nodes to scale. |
| **Test after landing, revert the culprit** | `graft-verify` tests the tip, bisects a failure and reverts the change that broke it. |
| **Monitoring agents ("superfans")** | Agents follow the changeset stream and report what matters to each component, also through Slack. |
| **Agent-friendly review** | Reviews with semantic approval rules: a comment-only change can need fewer approvals than a logic change. |
| **Agents as first-class users** | Per-agent tokens, an RPC allowlist and branch policies decide what each agent may do. |
| **Strict cache tiers** | `graft-cache` caches immutable objects in daemon and regional tiers, verifying every object. |

## Architecture

```
        agents, CI jobs, sparse workspaces          graft CLI / Go SDK
                            |
                            v   gRPC
        +-------------------------------------------------+
        |  graft-cache (optional daemon and cache tiers)  |
        +------------------------+------------------------+
                                 v
   +---------------------------------------------------------------+
   |  gitai server (any number of nodes)                           |
   |  land - objects - changesets - derived data - search - review |
   |  write path: changeset + compare-and-swap per reference       |
   +-----------+--------------------------------+------------------+
               |                                |
               v                                v
   +-----------------------+       +-----------------------------+
   |  SQLite / PostgreSQL  |       |  object storage             |
   |  changeset WAL, refs, |       |  content-addressed packs    |
   |  changelog, reviews   |       |  (file, S3, GCS, Azure)     |
   +-----------------------+       +-----------------------------+
               \                              /
                local Git repositories = caches any node can rebuild
```

Details: [ARCHITECTURE.md](doc/graft/ARCHITECTURE.md) and [STORAGE.md](doc/graft/STORAGE.md).

## Quick start

Requirements: Linux or macOS, Go 1.26, a C toolchain, Meson and Ninja (the
build compiles a bundled Git).

```shell
git clone https://github.com/feder-cr/git-ai.git
cd git-ai
make build                         # server, CLI and tools into _build/bin
examples/graft/quickstart.sh       # a server, a cache daemon, two agents and a land, in a temporary directory
./_build/bin/graft                 # the CLI and its commands
```

To run your own server, adapt [`examples/graft/config.toml`](examples/graft/config.toml)
and start it with `_build/bin/gitaly serve config.toml`. To put the repository
state in a database and object storage, add a `[graft.storage]` section: start in
`shadow` mode, import your repositories, then switch to `authoritative`
([STORAGE.md](doc/graft/STORAGE.md)).

Inside the code the extensions are called **Graft**: the `[graft]`
configuration section, the `graft` CLI, `internal/graft` and `doc/graft`.

## Documentation

- [Quickstart](doc/graft/QUICKSTART.md) and [configuration reference](doc/graft/CONFIGURATION.md)
- [Architecture](doc/graft/ARCHITECTURE.md) and the [storage core](doc/graft/STORAGE.md)
- [Landing and batching](doc/graft/LAND-BATCHING.md), [conflict resolution](doc/graft/RESOLVE.md), [derived data](doc/graft/DERIVED.md)
- [Review](doc/graft/REVIEW.md), [search](doc/graft/SEARCH.md), [verification](doc/graft/VERIFY.md), [superfans](doc/graft/SUPERFAN.md) and [Slack chat](doc/graft/SUPERFAN-CHAT.md)
- [Agent security](doc/graft/AGENT-SECURITY.md), [benchmarks](doc/graft/BENCH.md), [fuzzing](doc/graft/FUZZING.md)
- [Limitations and roadmap](doc/graft/LIMITATIONS.md)
- Design documents of each phase: [PLAN.md](doc/graft/PLAN.md) to [PLAN6.md](doc/graft/PLAN6.md), and [PLAN8.md](doc/graft/PLAN8.md) (faster landing)

## Benchmark

50 agents land 5 changes each on one branch; 20% of the changes conflict on
purpose. "Classic" is the same server with batching off: every agent rebases
and retries its own compare-and-swap, up to 20 attempts. Measured with
[`acceptance-phase4.sh`](examples/graft/demo/acceptance-phase4.sh) on one
16-core machine, two runs on 2026-10-20 (commit `bf95897`, the 0.3.0 tree; not
repeated for 0.4.0):

| 50 agents, 250 changes, no conflict resolver | classic | gitai |
|---|---|---|
| changes landed | 100-106 | 200 |
| attempts per landed change | 26.5-28.7 | 1.0 |
| lands per minute | 264-385 | 1,487-1,944 |
| p99 latency | 4.4-5.2 s | 1.9-2.7 s |

The 50 changes gitai does not land are the planned conflicts, returned to their
agents: these runs use no conflict resolver. With the deterministic resolver of
[`resolve.toml`](examples/graft/demo/resolve.toml) every conflict is resolved
and all 250 changes land (1,245-1,806 lands per minute, p99 2.0-3.4 s). Details and more sizes in [BENCH.md](doc/graft/BENCH.md).

## Status

Version 0.4.2. Everything above is implemented and covered by unit,
end-to-end, multi-node, crash and fuzz tests. All of them run on one machine:
several nodes share one database and one bucket, nodes on separate machines are
not tested. The tests and benchmarks use file and in-memory buckets; S3, GCS
and Azure are untested. Garbage collection of packs exists but is off by
default (`gc = "auto"` or `graft storage gc`, see
[STORAGE.md](doc/graft/STORAGE.md)). Interfaces may still change before 1.0.
Known gaps are listed in [LIMITATIONS.md](doc/graft/LIMITATIONS.md).

The access policy reloads on SIGUSR1 without a
restart, and `graft policy check` validates a policy file before a reload
([CONFIGURATION.md](doc/graft/CONFIGURATION.md#reloading-the-policy)).

## Community

gitai is meant to be maintained by its community. Open an issue to report a
bug or propose an idea, start a discussion before a large design change, and send
pull requests: tests and a short description of the reasoning make them easy to
review. The design documents in `doc/graft/PLAN*.md` explain why things are the
way they are.

## Credits and license

gitai is MIT licensed, see [LICENSE](LICENSE).

It started as a fork of [Gitaly](https://gitlab.com/gitlab-org/gitaly), GitLab's
Git RPC server (MIT, Copyright GitLab Inc.), which provides the Git plumbing
underneath; every upstream Gitaly RPC keeps working, except that a few mutators
are refused when you switch the storage core to authoritative mode
([STORAGE.md](doc/graft/STORAGE.md#rpcs-in-authoritative-mode)). The non-MIT `ee/` directory
of upstream Gitaly is not part of this repository. Gitaly's own README is kept in
[doc/upstream/README-GITALY.md](doc/upstream/README-GITALY.md), and third-party notices are in
[NOTICE](NOTICE).
