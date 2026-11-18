// forge-core: a standalone GitHub-compatible git forge in one process.
//
// Surfaces (all from one in-memory store, stdlib only):
//
//	git smart HTTP  /{owner}/{repo}.git  (upload-pack v2, receive-pack v0)
//	REST v3         /api/v3              (repos, issues, pulls, contents, labels)
//	GraphQL v4      /api/graphql
//	MCP             /mcp                 (streamable HTTP, JSON-RPC 2.0)
//
// Boot contract (see the kit's starter/main.ts and RULES.txt): the parent (main.ts)
// spawns this binary; it binds an ephemeral port and prints "PORT <n>" as its first
// stdout line, then serves until it is killed. The parent registers that port with
// the suite and long-polls /done.
//
// Performance notes: the hot paths (issue read, issue write, push, clone) are kept
// allocation-light — direct map lookups, pre-sized buffers, no per-request scans.
// The store is sharded by repository so concurrent clones do not serialize on one lock.
package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	store := NewStore()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Length", "12")
		_, _ = w.Write([]byte("{\"ok\":true}\n"))
	})
	registerRoutes(mux, store)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[core] listen: %v\n", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	// First stdout line: the bound port, so the parent can register it.
	fmt.Printf("PORT %d\n", port)

	srv := &http.Server{Handler: mux}
	go func() {
		_ = srv.Serve(ln)
	}()

	// Serve until killed; the parent kills us once the suite says done.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	_ = srv.Close()
}
