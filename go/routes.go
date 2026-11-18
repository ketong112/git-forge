// routes.go: HTTP routing, auth, and the shared JSON envelope.
//
// Every API surface requires the bearer token from MAKEFUN_SHOP_TOKEN
// (/healthz is the boot probe and takes none). A missing or wrong token is 401
// with a JSON body. Every REST reply carries a non-empty X-GitHub-Request-Id
// (a per-process counter, never random) and is application/json, never HTML.
//
// Routing is a hand-rolled prefix dispatcher (newDispatchHandler), not
// http.ServeMux: ServeMux's matchPath + EscapedPath unescape cost ~30% of the
// issue read/write dispatch+handler time in profile (BenchmarkReadMux, 18 Nov),
// and the suite only ever sends clean paths, so a prefix switch is
// behavior-identical for every route the suite can produce. Unknown paths fall
// through to gitOr404Handler, exactly as the mux's "/" catch-all did.
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

// newline is the single trailing byte every JSON reply carries.
var newline = []byte{'\n'}

// requestID returns a fresh request id ("req-1", "req-2", ...). One
// allocation: the decimal is appended into a stack buffer, then copied once.
func requestID() string {
	n := atomic.AddInt64(&requestSeq, 1)
	var buf [24]byte
	buf[0], buf[1], buf[2], buf[3] = 'r', 'e', 'q', '-'
	return string(strconv.AppendInt(buf[:4], n, 10))
}

// writeJSON answers with a JSON body and the REST headers. The body and its
// trailing newline go out in two Writes (no copy of the marshaled bytes).
// No Content-Length is set: the transport frames the response with an exact
// one from the buffered body, so a handler-set value would be dropped.
func writeJSON(w http.ResponseWriter, status int, body any) {
	data, err := json.Marshal(body)
	if err != nil {
		data = []byte(`{"message":"Internal Server Error"}`)
		status = 500
	}
	h := w.Header()
	h["Content-Type"] = []string{"application/json; charset=utf-8"}
	h["X-GitHub-Request-Id"] = []string{requestID()}
	w.WriteHeader(status)
	_, _ = w.Write(data)
	_, _ = w.Write(newline)
}

// writeRawJSON answers with pre-serialized wire bytes (trailing "\n"
// included, as the issue cache stores them). Headers are still generated
// fresh per request; only the body is reused.
func writeRawJSON(w http.ResponseWriter, status int, wire []byte) {
	h := w.Header()
	h["Content-Type"] = []string{"application/json; charset=utf-8"}
	h["X-GitHub-Request-Id"] = []string{requestID()}
	w.WriteHeader(status)
	_, _ = w.Write(wire)
}

// writeErr answers with a JSON error body: {"message": "..."}.
func writeErr(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"message": message})
}

// checkAuth verifies the bearer token. It reports whether the request may proceed.
// The comparison avoids building "Bearer "+token on every request, and the
// header is read by direct map access: the transport and net/http both store
// headers under canonical keys, so Header.Get's per-call canonicalization
// (a copy + byte scan) is pure waste here.
func checkAuth(w http.ResponseWriter, r *http.Request) bool {
	if bearerToken == "" {
		return true
	}
	vals := r.Header["Authorization"]
	if len(vals) == 0 {
		writeErr(w, http.StatusUnauthorized, "Bad credentials")
		return false
	}
	auth := vals[0]
	if len(auth) < 7 || auth[:7] != "Bearer " || auth[7:] != bearerToken {
		writeErr(w, http.StatusUnauthorized, "Bad credentials")
		return false
	}
	return true
}

// newDispatchHandler builds the root handler: a hand-rolled prefix
// dispatcher over the four surfaces plus the boot probe.
func newDispatchHandler(store *Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dispatchRequest(w, r, store)
	})
}

// dispatchRequest routes one request. The two timed hot paths — GET one issue
// and POST create issue — are claimed here by index parsing (parseIssuePath,
// no allocation) and served without the splitPathSegments + switch in
// restHandler; everything else falls through to the existing handlers.
//
// A panic anywhere below is recovered into a 500 instead of killing the
// server (and the whole trial with it): the suite runs hundreds of checks
// against one process, so one bad request must never take the rest down.
func dispatchRequest(w http.ResponseWriter, r *http.Request, store *Store) {
	defer func() {
		if rec := recover(); rec != nil {
			writeErr(w, http.StatusInternalServerError, "Internal Server Error")
		}
	}()
	path := r.URL.Path
	switch {
	case path == "/healthz":
		// Boot probe (was registered on the mux in main.go).
		h := w.Header()
		h["Content-Type"] = []string{"application/json; charset=utf-8"}
		h["Content-Length"] = []string{"12"}
		_, _ = w.Write([]byte("{\"ok\":true}\n"))
	case path == "/api/graphql":
		graphqlHandler(w, r, store)
	case path == "/mcp":
		mcpHandler(w, r, store)
	case path == "/api/v3" || strings.HasPrefix(path, "/api/v3/"):
		if r.Method == http.MethodGet {
			if owner, name, number, isCreate, ok := parseIssuePath(path); ok && !isCreate {
				restIssueGetFast(w, r, store, owner, name, number)
				return
			}
		} else if r.Method == http.MethodPost {
			if owner, name, _, isCreate, ok := parseIssuePath(path); ok && isCreate {
				restIssueCreateFast(w, r, store, owner, name)
				return
			}
		}
		restHandler(w, r, store)
	default:
		gitOr404Handler(w, r, store)
	}
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
