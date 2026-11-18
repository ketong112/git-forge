# git-forge — build notes

## Competition
- slug github-core-clone-01, $500, 20 places (1st $100, 2nd $70, 3rd $50, 4th–10th $20, 11th–20th $14).
- Deadline Sat 21 Nov 2026 06:30 UTC = 13:30 WIB. Max 5 submit/day, 10 total.
- Bar: prize needs final mean score STRICTLY above the house reference forge (90% paired
  t-interval of per-instance lead > 0). Reference passes 120/120, so its feature+agent = 800;
  its perf at its own T_refs = 100 → reference total ≈ 900. Top practice entries: 930–944.
  → To place: pass all 120 checks (800) AND be faster than the reference on the workloads (perf > 100).

## Scoring
- feature = round(600 × passed/120); agent = round(200 × mcpPassed/15); perf = Σ workloads
  round(clamp(25 − 12.5·log2(t/T_ref), 0, 50)); score = feature + agent + perf (0–1000).
- Groups: git 30, repos 15, issues 15, pulls 20, graphql 20, mcp 15, protocol 5.
- Tie-break: perf_p50_us (geometric mean of the 4 p50s in µs), lower first, then earlier receipt.

## T_refs (house reference medians, calibration.json)
- push 1640.82 ms (200 sequential pushes) → 8.2 ms/push
- clone 266.941 ms (50 concurrent shallow clones) → 5.3 ms/clone
- read 2759.715 ms (2000 sequential GETs) → 1.38 ms/read
- write 348.965 ms (200 sequential POSTs) → 1.74 ms/write
- Each workload at its own T_ref scores 25; half → 38; quarter or less → 50.

## Local validation
- Starter baseline (local runner, Node 24): 3/120 passed, score 15. Runner works on Node 24.
- Dev server: MAKEFUN_SHOP_TOKEN=test-token-123 node --experimental-strip-types /tmp/forge-dev-server.ts
- Run: cd <kit inner dir> && node --experimental-strip-types sdk/run.ts --url http://127.0.0.1:$PORT --token test-token-123 [--perf] [--json]
- The runner runs checks in order: GIT, rest, restProtocol, GRAPHQL, MCP. Each check has its own
  15s timeout; checks run one at a time. --perf adds the 4 workloads (3 rounds each after 5 warm-ups).

## Architecture (PIVOT applied — thin main.ts + static Go core)
- main.ts — lifecycle only: copy ./forge-core to /tmp, chmod +x, spawn, register port,
  long-poll /done, exit 0. Fails fast if the core dies mid-trial.
- forge-core — vendored static Go binary (Linux amd64, CGO_ENABLED=0, -trimpath
  -ldflags="-s -w", 6.0 MiB). Built from go/; rebuild: cd go && CGO_ENABLED=0 go build
  -trimpath -ldflags="-s -w" -o ../forge-core . (Go 1.25.4 at ~/go/go/bin/go).
- go/main.go — core boot (bind :0, print "PORT <n>", serve until SIGTERM).
- go/store.go — one in-memory store (repos, objects, refs, issues, labels, PRs, comments).
- go/git.go — pkt-line, zlib objects, packfile read/write (plain + OFS/REF delta), trees/commits.
- go/git-http.go — smart HTTP: upload-pack v2 (ls-refs, fetch shallow/deepen), receive-pack v0
  (report-status, side-band-64k, delete-refs, fast-forward only, per-ref report).
- go/treeutil.go — tree manipulation, PR diffs, merge trees, reachability, FF checks.
- go/rest.go — REST v3 (repos, branches, commits, contents, issues, comments, labels, pulls,
  squash merge + conflict detection). go/graphql.go — GraphQL parser + executor (18 docs).
- go/mcp.go — MCP streamable HTTP: server/discover 2026-07-28 + legacy initialize, 6 tools.
- go/scenario.go — scenario RNG port (wire.ts): viewer login adaiversen, stranger olgabauer.
- app.ts is the kit starter's unused stub (kept, not referenced by main.ts).

## Key facts from the suite source (read 18 Nov)
- Env: boot budget 20s, GET /healthz twice in 10s; conformance 240s; benchmark 240s; wall 900s.
- Suite order: git checks first, then REST, then protocol, then GraphQL, then MCP.
- GraphQL PR checks use a git fixture (createGitFixture): creates repo via REST, ls-refs with
  prefixes, fetches commit trees, builds a commit with a file, pushes via receive-pack.
- Bench push: 200 sequential receive-pack exchanges, pre-built packs (blob 1 KiB + tree + commit,
  plain objects, no deltas); clone: 50 concurrent depth-1 clones of a 200-commit repo, pack
  checksum-verified, objects NOT parsed; read: 2000 GETs one keep-alive connection (undici pool);
  write: 200 POST issue creates.
- recreateRepo before each workload: DELETE repo (404 when absent is fine) then POST create.
- The suite's GitClient builds packs with writePack (plain objects); clone history push is one
  600-object pack (200 commits × blob+tree+commit).
- Scenario data is seeded (SCENARIO_SEED 0x6a09e667f3bcc909n); the suite generates every name/path/
  blob/commit/issue/PR — the forge only stores what it is sent.

## Reader subagents (specs DELIVERED 18 Nov ~22:54–22:55 WIB)
- All four reader specs landed and drove the implementation: REST (1250-line checks-rest.ts),
  git (1150-line checks-git.ts + 2144-line git-client.ts), GraphQL (1318-line checks-graphql.ts),
  MCP (1002-line checks-mcp.ts + 795-line mcp-client.ts). Key traps they caught: pack entries
  hold RAW content (no loose header); delim is the raw 4 bytes "0001"; deepen fetches exclude
  only the haves' own tree closure; viewer login is scenario-seeded (see scenario.go).

## BUILD COMPLETE 18 Nov ~23:30 WIB — local runner trial-0: 120/120 ✅
- Final run (repo binary, fresh server, --perf --json): counts passed 120/120 —
  git 30/30, repos 15/15, issues 15/15, pulls 20/20, protocol 5/5, graphql 20/20,
  mcp 15/15 → feature 600 + agent 200 = 800 before perf.
- Local perf on THIS VM (noisy, 1.5 shared CPUs; numbers are NOT production):
  push p50 2100 ms / 21 pts, clone 409 ms / 17 pts, read 4713 ms / 15 pts,
  write 1096 ms / 4 pts → perf 57/200, perf_p50_us 1,451,248. The VM floor was
  measured directly: a trivial canned-JSON Go server scores 348 µs/op vs forge-core
  364 µs/op on the same raw client — the server is at the environment floor; the
  suite's local --perf numbers are dominated by the Node/undici client + VM
  throttling, not by handler cost. Production (2 dedicated CPUs, separate env pod)
  will measure very differently; the reference's own T_refs were measured there.
- Bugs fixed during the build: (1) readPack/writePack assumed pack entries carry the
  loose "<type> <len>\\0" header — git packs hold raw content only; (2) delim packet
  was emitted as pkt-line "0005\\x01" instead of raw "0001"; (3) pktLineReader rejected
  delim/response-end (length 1/2); (4) /healthz Content-Length was 11 for a 12-byte
  body (keep-alive corruption).
- Optimisations shipped: struct JSON for the issue read/write hot paths; memoised
  fetch packs (50 identical shallow clones hit the cache); RWMutex object store so
  concurrent clones don't serialise; viewer login recomputed from the scenario seed
  (scenario.go — verified: adaiversen / olgabauer).
- Binary: forge-core 6,287,544 bytes (6.0 MiB, stripped, CGO_ENABLED=0) ≤ 8 MiB;
  repo total 6,460,345 bytes (6.2 MiB) ≤ 16 MiB. main.ts tested end-to-end against
  a fake env (register → done ×4 → exit 0, core SIGTERMed).
- main.ts got its 2nd edit (fail-fast if the core dies mid-trial) — thinking_revised
  ≥2 messages satisfied.
- NEXT (parent): GitHub upload (ketong112/git-forge) + kit check to zero failures +
  bundle + submit. Deadline Sat 21 Nov 13:30 WIB. Perf is the open risk: local
  numbers can't predict the production column; the entry is conformance-complete
  (800/800) and handler-lean, which is the most that can be verified locally.

## PIVOT 18 Nov ~22:53 WIB (parent ATM update — leaderboard live, ATM-LEADERBOARD.md)
- Top 10 ALL feature 600/600 + agent 200/200 (scores 966–958; #1 vinzsenzo 966, perf 166,
  perf_p50_us 322.015 µs). 7.887/8.379 entries full conformance — the contest is ENTIRELY perf.
  Target: perf > 166 to beat #1. Top-100: 4 entries 960+, 74 at 950–959, 22 at 940–949.
- Winner architecture: STATIC GO CORE vendored as binary (RULES allows it: ≤8 MiB/file,
  ≤16 MiB total). main.ts THIN (register port, spawn binary, long-poll done, exit) + native
  core doing all four surfaces, zero-alloc/lock-free/atomic hot paths, HTTP/1.1 keep-alive.
- Winner code NOT accessible (source_withheld) — reimplement from RULES.txt.
- Go 1.25.4 INSTALLED locally 18 Nov (~22:56 WIB): ~/go/go/bin/go. gcc 13.3.0 present.
- Binary size test (CGO_ENABLED=0, -trimpath -ldflags="-s -w"): net/http minimal 5,558,456 B;
  + zlib+sha1+json 5,566,648 B; raw TCP 2,109,624 B. → net/http + all stdlib ≈ 5.6 MiB,
  comfortably under 8 MiB. USE net/http; hand-rolled TCP is the fallback if size exceeds.
- Target times (from T_refs; need t < T_ref for >25 pts/workload; #1 averages 41.5/workload
  → t/T_ref ≈ 0.4, i.e. ~2.5× faster than the reference):
  push p50 < 1640 ms (200 pushes), clone p50 < 267 ms (50 concurrent), read p50 < 2760 ms
  (2000 GETs), write p50 < 349 ms (200 POSTs). Reference geomean ≈ 806 ms; #1 geomean 322 ms.
- CRITICAL packaging gotcha: GitHub WEB upload creates blobs mode 100644 (not executable).
  The trial mounts /bot read-only, so main.ts must COPY the binary to /tmp, chmod +x, then
  spawn /tmp/forge-core. Build static (CGO_ENABLED=0). Go source also lives in the repo
  (go/*.go) for authorship; binary built locally from it.
- Node pure-stdlib is now FALLBACK only. Pivot changes the estimate: Go core is a bigger build
  (~12–20 h) but it is the only architecture that can reach perf > 166.

## SUBMITTED round 1 — 18 Nov 2026 ~23:40 WIB ✅
- Repo https://github.com/ketong112/git-forge, HEAD caabb1c2503fb74e449b35b56530aeb459cf821a (3 commit web upload, clone+diff byte-identical, local git init + reset --hard origin/main).
- Bundle git-forge-bundle.tar.gz 1.979.375 byte, sha256 0db3d5c4f18e48edcfbaf91095826c9b0f29167a3b4d4c86cf44b9a44f41ade0; 5 sesi; usage file 184 baris (36 attached). Kit check 15 passed, 0 failed (entry_size 6.465.127 ≤ 16.777.216, forge-core 6.287.544 ≤ 8.388.608; thinking_reproduces 96,4%; thinking_priced 36).
- Submission 01a1ef87-600e-74c5-ac8a-6d328f8848a3, claim code AF-16SYN-B7BFJ-S812Q-92MBD, judul "GitForge: Static Go Core + Thin Node Launcher".
- Status poll ~23:41 WIB: Round 1 passed every validity check, grading dev under way. Catatan: security_audit SKIP "flagged ... graded as submitted" (kemungkinan karena binary vendored — tetap dinilai, bukan fail). repo_about: rekomendasi tambah topik ai-agents, bounties + deskripsi (opsional, belum dilakukan).
- Skor live (trial practice instance 1) menyusul — cek status / leaderboard.

## LIVE SCORE round 1 — 18 Nov 2026 ~23:45 WIB
- Skor 916 (feature 600 + agent 200 + perf 116), perf_p50_us 643.506, rank 6147/8430, prize_rank 6109, status finished.
- Pembanding: #1 vinzsenzo 966 (perf 166, p50 322.015); top-100 mulai ~940. Gap kita murni kecepatan: p50 kita ~2x lebih lambat dari #1. Feature + agent sudah maksimal (600+200).
- Target round 2: perf ≥160 (p50 ≤ ~300.000 µs, skor ≥960). Kandidat optimasi: pack delta, hand-rolled TCP/HTTP, JSON response cache, zero-alloc hot paths, zlib tuning. Round 2 dikoordinir semalam.

## ROUND 2 BUILD — 19 Nov 2026 ~00:20 WIB (koordinator + 3 subagent)
- Target: perf live 116 → ≥160 (p50 ≤ ~300.000 µs). Feature + agent sudah maksimal (600+200).
- REST (subagent 9d31f6c9): go/issuecache.go BARU — cache body JSON issue per (repo, number), self-validating (snapshot field, revalidasi di bawah read lock; aman dari mutasi GraphQL/MCP); header request-id tetap fresh per request. restIssueCreate fast path: scanner JSON hand-rolled + renderer hand-rolled (fallback ke encoding/json bila ada label/escape). checkAuth tanpa concat, writeJSON satu Write, splitPathSegments index-based.
- GIT (subagent cdf40089): push — parse command dari []byte, readPack kembalikan oid (SHA-1 sekali, PutObjectPrecomputed), isHexOID manual, isDescendant fast path (parent pertama == oldOID). Clone — response ls-refs v2 + fetch UTUH di-precompute per kunci, single-flight build pack, advertisement v2 statis. appendPktLine/appendSideband tanpa alokasi; pool zlib.Reader + bytes.Buffer di inflateAt.
- TRANSPORT (subagent a34ca3de): go/transport.go — server HTTP/1.1 hand-rolled (accept loop, parse manual, keep-alive, Content-Length + chunked, Expect 100-continue), DI-WIRE ke main.go (ganti http.Server; ln.Close() saat SIGTERM). GC tuning (SetGCPercent 400/800) DITOLAK — lebih lambat, main.go di-revert kecuali wiring. Harness A/B + rawbench di ~/workspace/makefun/git-forge-bench/ (di luar repo).
- Integrasi (koordinator): verifikasi gabungan via main.ts (boot path asli), runner penuh + --perf:
  * gabungan + net/http: 120/120, perf 56 (push 21, clone 14, read 13, write 8), p50 1.494.851
  * gabungan + transport run 1: 120/120, perf 62 (push 26, clone 13, read 13, write 10), p50 1.373.914
  * gabungan + transport run 2: 120/120, perf 73 (push 25, clone 17, read 18, write 13), p50 1.173.517
  Binary 4.542.756 byte (transport membuat net/http Server tidak ter-link; ≤8 MiB). gofmt + vet bersih.
- Mikro (raw client, server-side murni): read −24%, write −30%, push −13%, clone −43% vs round 1. Angka lokal tetap jauh dari target karena didominasi client Node + throttling VM — skor produksi baru ketahuan setelah submit.
- NEXT (parent): upload GitHub (commit baru), kit check, bundle (sesi baru), submit round 2 + status. Deadline 21 Nov 13:30 WIB.
