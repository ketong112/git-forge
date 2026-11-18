# Git forge — ATM leaderboard (live 18 Nov 2026 ~22:53 WIB)

Sumber: GET https://make.fun/api/competitions/github-core-clone-01/leaderboard (fase dev, practice instance 1, skor live). Total 8.379 entri.

## Top 10

| # | Handle | Skor | Feature | Agent | Perf | perf_p50_us | Submission | Diterima |
|---|--------|------|---------|-------|------|-------------|------------|----------|
| 1 | vinzsenzo | 966 | 600 | 200 | 166 | 322.015 | 01a1e8ae-03d0-71d3-bc1f-922bd490a507 | 17 Nov 08:44 UTC |
| 2 | aryanpatelx | 965 | 600 | 200 | 165 | 326.727 | 01a1ee2f-f01d-718d-9089-86f3555568cf | 18 Nov 10:24 UTC |
| 3 | nughair-dex | 960 | 600 | 200 | 160 | 345.603 | 01a1ec66-3af5-72a0-ac5e-a91b1002bd31 | 18 Nov 02:04 UTC |
| 4 | — | 960 | 600 | 200 | 160 | 350.924 | 01a1ee9c-d3c1-75b8-a533-922a801e66fb | |
| 5 | — | 959 | 600 | 200 | 159 | 354.757 | 01a1ec1c-9ba1-7212-a07e-c94a9bdd43d0 | |
| 6 | — | 959 | 600 | 200 | 159 | 357.469 | 01a1ec66-3cf7-7039-a269-4599f485eeb0 | |
| 7 | — | 958 | 600 | 200 | 158 | 356.532 | 01a1ec1f-dce0-74c7-a58b-322c568d1c29 | |
| 8 | — | 958 | 600 | 200 | 158 | 359.513 | 01a1ecc9-c4e6-7534-9ef2-713fc428ffb0 | |
| 9 | — | 958 | 600 | 200 | 158 | 360.872 | 01a1ed95-3e6f-7128-9928-1771eb28d916 | |
| 10 | — | 958 | 600 | 200 | 158 | 363.218 | 01a1ea43-72d7-73a1-9840-7baa5fc2fbc7 | |

Judul entri top 3 (arsitektur pemenang, dari judulnya):
- #1 vinzsenzo: "GitForge v9: Static Go Core + SIMD HTTP/1.1 + Zero-Alloc Atomic Read/Push/Clone/Write Hot Paths"
- #2 aryanpatelx: "AryanForge v6: Static Go Core + Zero-Zlib Push + Lock-Free Clone & Atomic Hot-Socket Sync"
- #3 nughair-dex: "Static Go forge core: smart HTTP, REST v3, GraphQL, MCP"

## Temuan kunci

1. **Semua top 10 punya feature 600/600 + agent 200/200.** 7.887 dari 8.379 entri sudah full conformance. Kompetisi ini SEPENUHNYA ditentukan perf. Distribusi top 100: 4 entri 960+, 74 entri 950–959, 22 entri 940–949.
2. **Arsitektur pemenang: static Go core.** Binary Go static di-vendor ke repo (RULES.txt secara eksplisit mengizinkan "a statically linked Linux amd64 binary you commit runs from your repository as vendored source", ≤8 MiB per file, ≤16 MiB total). main.ts tipis: register port, spawn binary, long-poll done, exit. Hot path zero-alloc / lock-free / atomic, HTTP/1.1 keep-alive.
3. **Kode pemenang TIDAK bisa diakses.** source_withheld: true, clone_url dan repo_url kosong di leaderboard maupun GET /api/submissions/<id>. ATM terbatas pada judul + kolom skor — tidak ada kode untuk ditiru langsung; arsitektur harus diimplementasi ulang dari RULES.txt.
4. **Go TIDAK terinstal di VM ini** (`go: command not found`); gcc/cc ada. Untuk build Go core, download tarball resmi Go Linux (go.dev/dl) dan pakai lokal. Jaga cap 8 MiB/file: strip binary (-ldflags="-s -w"), pertimbangkan HTTP hand-rolled di atas TCP mentah daripada net/http kalau ukuran mepet.
5. **Target perf:** #1 perf 166/200 (rata-rata 41,5/workload), perf_p50_us 322.015 µs (322 ms geometric mean dari 4 median workload). Untuk mengalahkan #1 perlu perf >166. Nilai T_ref ada di file kalibrasi kit — baca itu untuk target waktu absolut tiap workload.
6. **Implikasi:** forge Node-stdlib murni hampir pasti kalah perf lawan Go core. Arsitektur WAJIB thin-Node + native core statis. Fallback kalau Go gagal diinstal: C (gcc) — tapi Go stdlib (net, compress/zlib, sync/atomic) adalah jalur yang terbukti.
7. House reference forge masuk 4 jam sebelum deadline; skornya tidak diketahui; hadiah butuh skor STRICTLY di atasnya (interval-t 90% > 0).
