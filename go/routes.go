// routes.go: HTTP routing, auth, and the shared JSON envelope.
//
// Every API surface requires the bearer token from MAKEFUN_SHOP_TOKEN
// (/healthz is the boot probe and takes none). A missing or wrong token is 401
// with a JSON body. Every REST reply carries a non-empty X-GitHub-Request-Id
// (a per-process counter, never random) and is application/json, never HTML.
package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
)

var requestSeq int64

// bearerToken is read once at startup from MAKEFUN_SHOP_TOKEN.
var bearerToken = os.Getenv("MAKEFUN_SHOP_TOKEN")

// requestID returns a fresh request id ("req-1", "req-2", ...).
func requestID() string {
	return "req-" + strconv.FormatInt(atomic.AddInt64(&requestSeq, 1), 10)
}

// writeJSON answers with a JSON body and the REST headers. The body and its
// trailing newline go out in a single Write.
func writeJSON(w http.ResponseWriter, status int, body any) {
	data, err := json.Marshal(body)
	if err != nil {
		data = []byte(`{"message":"Internal Server Error"}`)
		status = 500
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-GitHub-Request-Id", requestID())
	w.Header().Set("Content-Length", strconv.Itoa(len(data)+1))
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}

// writeRawJSON answers with pre-serialized wire bytes (trailing "\n"
// included, as the issue cache stores them). Headers are still generated
// fresh per request; only the body is reused.
func writeRawJSON(w http.ResponseWriter, status int, wire []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-GitHub-Request-Id", requestID())
	w.Header().Set("Content-Length", strconv.Itoa(len(wire)))
	w.WriteHeader(status)
	_, _ = w.Write(wire)
}

// writeErr answers with a JSON error body: {"message": "..."}.
func writeErr(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"message": message})
}

// checkAuth verifies the bearer token. It reports whether the request may proceed.
// The comparison avoids building "Bearer "+token on every request.
func checkAuth(w http.ResponseWriter, r *http.Request) bool {
	if bearerToken == "" {
		return true
	}
	auth := r.Header.Get("Authorization")
	if len(auth) < 7 || auth[:7] != "Bearer " || auth[7:] != bearerToken {
		writeErr(w, http.StatusUnauthorized, "Bad credentials")
		return false
	}
	return true
}

// registerRoutes wires the four surfaces onto the mux.
func registerRoutes(mux *http.ServeMux, store *Store) {
	mux.HandleFunc("/api/v3", func(w http.ResponseWriter, r *http.Request) {
		restHandler(w, r, store)
	})
	mux.HandleFunc("/api/v3/", func(w http.ResponseWriter, r *http.Request) {
		restHandler(w, r, store)
	})
	mux.HandleFunc("/api/graphql", func(w http.ResponseWriter, r *http.Request) {
		graphqlHandler(w, r, store)
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		mcpHandler(w, r, store)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		gitOr404Handler(w, r, store)
	})
}

// gitOr404Handler serves git smart HTTP under /{owner}/{repo}.git/* and a JSON
// 404 for anything else.
func gitOr404Handler(w http.ResponseWriter, r *http.Request, store *Store) {
	path := r.URL.Path
	if strings.HasSuffix(path, ".git") || strings.Contains(path, ".git/") {
		gitHandler(w, r, store)
		return
	}
	writeErr(w, http.StatusNotFound, "Not Found")
}
