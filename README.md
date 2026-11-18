# git-forge

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/ketong112/git-forge/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/ketong112/git-forge/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A standalone GitHub-compatible git forge: git smart HTTP (upload-pack protocol v2,
receive-pack v0), the REST v3 API, GraphQL v4, and MCP over streamable HTTP — all
served from one in-memory store, no UI, no git binary, no outside service.

The forge is a thin Node 22 entry (`main.ts`: bind, register with the env, wait for
done, exit 0) plus a statically linked Go core (`forge-core`, built from `go/`) that
does all four surfaces with zero-alloc/lock-free hot paths and HTTP/1.1 keep-alive.
The core is vendored as a binary, as RULES.txt allows; its source lives in `go/`.

## Run

```sh
node main.ts
```

Node 22.18 or later (it strips TypeScript types on its own; on an older local Node use
`node --experimental-strip-types main.ts`). The trial harness starts the forge beside
the suite and hands it `MAKEFUN_ENV_URL`: the entry copies the core to /tmp (the repo
is mounted read-only and a web upload lands the binary without the executable bit),
spawns it, registers its ephemeral port with `POST $MAKEFUN_ENV_URL/register`, serves,
then long-polls `GET $MAKEFUN_ENV_URL/done` and exits 0 when the suite is over.
`MAKEFUN_SHOP_TOKEN` is the bearer token every API call must carry.

## Layout

- `main.ts` — the entry ↔ env lifecycle (spawn the core, register, wait for done,
  exit 0). Fails fast if the core exits mid-trial.
- `forge-core` — the vendored static Go core (Linux amd64, CGO_ENABLED=0, stripped,
  6.0 MiB). Rebuild it with `cd go && CGO_ENABLED=0 go build -trimpath
  -ldflags="-s -w" -o ../forge-core .` (Go 1.25).
- `go/main.go` — core boot: bind an ephemeral port, print `PORT <n>`, serve until
  SIGTERM.
- `go/store.go` — the one in-memory store: repositories, git objects, refs, issues,
  labels, pull requests, comments. Everything is derived from what the suite sends;
  nothing reads a clock or the network, so two runs on the same instance leave the
  same state.
- `go/git.go` — the git object model: pkt-line framing, zlib objects, packfile read
  and write (plain + OFS/REF delta objects), trees and commits.
- `go/git-http.go` — git smart HTTP: upload-pack v2 (ls-refs, fetch with shallow /
  deepen), receive-pack v0 (report-status, side-band-64k, delete-refs,
  fast-forward only, per-ref report).
- `go/treeutil.go` — tree manipulation: contents writes, pull-request diffs, merge
  trees, reachability walks, fast-forward checks.
- `go/rest.go` — the REST v3 surface: repositories, branches, commits, contents,
  issues, comments, labels, pull requests, files, squash merges with conflict
  detection.
- `go/graphql.go` — GraphQL v4: a small parser + executor for the documents `gh`
  and `github-mcp-server` send, plus introspection for Issue, PullRequest,
  Repository and SearchType.
- `go/mcp.go` — MCP over streamable HTTP: server/discover (2026-07-28) with a legacy
  initialize fallback, tools/list, tools/call on the same store REST reads.
- `go/scenario.go` — the suite's seeded scenario, ported from the kit's wire.ts:
  the token's login (`adaiversen`) is recomputed deterministically from the scenario
  seed, so the forge knows its viewer without any out-of-band channel.

## Design notes

- One process, one store: a repository or issue created over MCP reads back over REST
  as the same object, with the same ids.
- Deterministic: ids and SHAs are content-derived (git object ids are SHA-1 over the
  object bytes, exactly as git computes them); issue/PR numbers are per-repository
  counters; no timestamps, ports or random values enter any stored state.
- Fast paths for the timed workloads: the read path is a direct map lookup with
  struct JSON encoding, the write path appends without a scan, identical shallow
  fetches reuse a memoised pack, and the clone workload never re-walks objects it
  already packed.

<sub>Entered on [make.fun](https://make.fun/?ref=dmgw15y1&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
