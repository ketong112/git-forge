// transport.go: hand-rolled HTTP/1.1 server over raw TCP.
//
// Drop-in alternative to net/http's Server for this forge: serveTransport
// takes the same http.Handler (the ServeMux) and serves it with an accept
// loop, a manual request-line + header parser, keep-alive, Content-Length
// and chunked bodies, and responses written straight to the socket.
//
// Why try this: the suite's client is sequential per connection and the
// workloads are small-request / small-response. net/http pays for generality
// (per-request goroutine + bufio + header maps + sniffing + timers) on every
// op; a tight loop that parses only what this forge needs may cut the
// per-op cost. The response is buffered in full before writing so the
// Content-Length is always exact and keep-alive stays correct even for
// handlers that never set one.
//
// Wired into main.go in round 2, after the A/B numbers (rawbench + runner
// 120/120) showed a clear win; see ~/workspace/makefun/git-forge-bench/.
//
// Round 3: the hot path is allocation-lean — fast-path URL parse (no
// url.ParseRequestURI for plain origin-form targets), exact-match header
// keys (no CanonicalMIMEHeaderKey scan for the keys clients actually send),
// a pooled response writer, a per-connection body scratch buffer, and the
// connection's RemoteAddr computed once. Handlers only ever read the
// Authorization and Git-Protocol request headers (verified by grep), but the
// full header map is still built, so any future header read keeps working.
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// transportResponseWriter captures a handler's reply in memory, then the
// connection loop frames it with an exact Content-Length.
type transportResponseWriter struct {
	header      http.Header
	status      int
	wroteHeader bool
	body        bytes.Buffer
}

func (w *transportResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *transportResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
}

func (w *transportResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.body.Write(p)
}

// trwPool reuses response writers across requests. The body buffer keeps its
// capacity (bounded on return); the header map is cleared, not reallocated.
var trwPool = sync.Pool{
	New: func() any { return &transportResponseWriter{} },
}

func acquireTRW() *transportResponseWriter {
	w := trwPool.Get().(*transportResponseWriter)
	w.status = 0
	w.wroteHeader = false
	w.body.Reset()
	if w.header != nil {
		clear(w.header)
	}
	return w
}

func releaseTRW(w *transportResponseWriter) {
	if w.body.Cap() > 256<<10 {
		return // let an oversized buffer be collected, not pooled
	}
	trwPool.Put(w)
}

// hdrPool reuses request header maps across requests (cleared on release;
// handlers only read them during ServeHTTP).
var hdrPool = sync.Pool{
	New: func() any { return make(http.Header, 8) },
}

// suite's clients send are already canonical, so they hit the exact-match
// table (a length + memcmp chain, no per-byte scan); anything else falls
// back to textproto, which itself returns canonical input unchanged.
func canonicalKeyFast(key string) string {
	switch key {
	case "Host", "Authorization", "Git-Protocol", "Content-Length",
		"Content-Type", "Transfer-Encoding", "Expect", "Connection",
		"User-Agent", "Accept", "Accept-Encoding":
		return key
	default:
		return textproto.CanonicalMIMEHeaderKey(key)
	}
}

// parseTarget parses an origin-form request target without url.ParseRequestURI
// (which pays for full URL grammar + escaping). Anything that is not a plain
// ASCII path with an optional query falls back to the stdlib parser, so
// %-escapes, fragments and absolute-form targets keep exact stdlib behaviour.
func parseTarget(target string) (*url.URL, error) {
	if len(target) == 0 || target[0] != '/' {
		return url.ParseRequestURI(target)
	}
	for i := 0; i < len(target); i++ {
		c := target[i]
		if c == '%' || c == '#' || c < 0x21 || c > 0x7e {
			return url.ParseRequestURI(target)
		}
	}
	u := &url.URL{}
	if i := strings.IndexByte(target, '?'); i >= 0 {
		u.Path = target[:i]
		u.RawQuery = target[i+1:]
	} else {
		u.Path = target
	}
	return u, nil
}

// foldContains reports whether needle occurs in s case-insensitively.
// Allocation-free; used for the Connection / Transfer-Encoding token checks.
func foldContains(s, needle string) bool {
	if len(needle) == 0 || len(s) < len(needle) {
		return false
	}
	for i := 0; i+len(needle) <= len(s); i++ {
		if strings.EqualFold(s[i:i+len(needle)], needle) {
			return true
		}
	}
	return false
}

// serveTransport serves handler on ln until ln is closed. One goroutine per
// connection; each connection loops over keep-alive requests.
func serveTransport(ln net.Listener, handler http.Handler) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go handleTransportConn(conn, handler)
	}
}

// readChunkedBody decodes a Transfer-Encoding: chunked body.
func readChunkedBody(br *bufio.Reader, limit int64) ([]byte, error) {
	var out bytes.Buffer
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if i := strings.IndexByte(line, ';'); i >= 0 {
			line = line[:i]
		}
		size, err := strconv.ParseInt(strings.TrimSpace(line), 16, 64)
		if err != nil {
			return nil, fmt.Errorf("bad chunk size %q", line)
		}
		if size == 0 {
			// trailer headers until blank line
			for {
				t, err := br.ReadString('\n')
				if err != nil {
					return nil, err
				}
				if t == "\r\n" || t == "\n" {
					break
				}
			}
			return out.Bytes(), nil
		}
		if int64(out.Len())+size > limit {
			return nil, fmt.Errorf("body too large")
		}
		if _, err := io.CopyN(&out, br, size); err != nil {
			return nil, err
		}
		// trailing CRLF after the chunk
		if _, err := br.ReadString('\n'); err != nil {
			return nil, err
		}
	}
}

func handleTransportConn(conn net.Conn, handler http.Handler) {
	defer conn.Close()
	remoteAddr := conn.RemoteAddr().String()
	br := bufio.NewReaderSize(conn, 32<<10)
	bw := bufio.NewWriterSize(conn, 32<<10)
	var bodyBuf []byte  // per-connection body scratch, grows to the largest body seen
	var frameBuf []byte // per-connection response-head scratch
	for {
		// Tolerate one leading blank line (some clients send CRLF first).
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		if line == "\r\n" || line == "\n" {
			line, err = br.ReadString('\n')
			if err != nil {
				return
			}
		}
		line = strings.TrimRight(line, "\r\n")
		sp1 := strings.IndexByte(line, ' ')
		if sp1 < 0 {
			return
		}
		sp2 := strings.IndexByte(line[sp1+1:], ' ')
		if sp2 < 0 {
			return
		}
		sp2 += sp1 + 1
		method, target, proto := line[:sp1], line[sp1+1:sp2], line[sp2+1:]
		if !strings.HasPrefix(proto, "HTTP/") {
			return
		}
		// Headers.
		hdr := hdrPool.Get().(http.Header)
		clear(hdr)
		var host string
		var contentLength int64 = -1
		chunked := false
		expectContinue := false
		connClose := false
		for {
			hline, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if hline == "\r\n" || hline == "\n" {
				break
			}
			hline = strings.TrimRight(hline, "\r\n")
			colon := strings.IndexByte(hline, ':')
			if colon < 0 {
				return
			}
			key := canonicalKeyFast(strings.TrimSpace(hline[:colon]))
			val := strings.TrimSpace(hline[colon+1:])
			hdr.Add(key, val)
			switch key {
			case "Host":
				host = val
			case "Content-Length":
				if n, err := strconv.ParseInt(val, 10, 64); err == nil {
					contentLength = n
				}
			case "Transfer-Encoding":
				if foldContains(val, "chunked") {
					chunked = true
				}
			case "Expect":
				if strings.EqualFold(val, "100-continue") {
					expectContinue = true
				}
			case "Connection":
				if foldContains(val, "close") {
					connClose = true
				}
			}
		}
		if expectContinue {
			_, _ = bw.WriteString("HTTP/1.1 100 Continue\r\n\r\n")
			_ = bw.Flush()
		}
		// Body.
		var body []byte
		const maxBody = 64 << 20
		if chunked {
			body, err = readChunkedBody(br, maxBody)
			if err != nil {
				return
			}
		} else if contentLength > 0 {
			if contentLength > maxBody {
				return
			}
			if int64(cap(bodyBuf)) < contentLength {
				bodyBuf = make([]byte, contentLength)
			} else {
				bodyBuf = bodyBuf[:contentLength]
			}
			if _, err := io.ReadFull(br, bodyBuf); err != nil {
				return
			}
			body = bodyBuf
		} else if contentLength == 0 {
			body = nil
		}
		u, err := parseTarget(target)
		if err != nil {
			_, _ = bw.WriteString("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			_ = bw.Flush()
			return
		}
		req := &http.Request{
			Method:        method,
			URL:           u,
			Proto:         proto,
			ProtoMajor:    1,
			ProtoMinor:    1,
			Header:        hdr,
			Body:          io.NopCloser(bytes.NewReader(body)),
			ContentLength: int64(len(body)),
			Host:          host,
			RemoteAddr:    remoteAddr,
			RequestURI:    target,
		}
		if strings.HasPrefix(proto, "HTTP/1.0") {
			req.ProtoMinor = 0
		}
		rw := acquireTRW()
		handler.ServeHTTP(rw, req)
		if len(hdr) <= 16 {
			hdrPool.Put(hdr)
		}
		status := rw.status
		if status == 0 {
			status = http.StatusOK
		}
		// Frame the response: exact Content-Length from the buffered body.
		// Built in a per-connection scratch buffer, then one buffered write
		// for the head and one for the body; a single Flush issues the
		// syscall(s).
		frameBuf = frameBuf[:0]
		frameBuf = append(frameBuf, "HTTP/1.1 "...)
		frameBuf = strconv.AppendInt(frameBuf, int64(status), 10)
		frameBuf = append(frameBuf, ' ')
		frameBuf = append(frameBuf, http.StatusText(status)...)
		frameBuf = append(frameBuf, "\r\n"...)
		hdrOut := rw.Header()
		for k, vs := range hdrOut {
			// Drop any handler-set Content-Length / Connection; ours are
			// authoritative.
			if k == "Content-Length" || k == "Connection" {
				continue
			}
			for _, v := range vs {
				frameBuf = append(frameBuf, k...)
				frameBuf = append(frameBuf, ": "...)
				frameBuf = append(frameBuf, v...)
				frameBuf = append(frameBuf, "\r\n"...)
			}
		}
		frameBuf = append(frameBuf, "Content-Length: "...)
		frameBuf = strconv.AppendInt(frameBuf, int64(rw.body.Len()), 10)
		frameBuf = append(frameBuf, "\r\n"...)
		keepAlive := !connClose && proto != "HTTP/1.0"
		if !keepAlive {
			frameBuf = append(frameBuf, "Connection: close\r\n"...)
		}
		frameBuf = append(frameBuf, "\r\n"...)
		if _, err := bw.Write(frameBuf); err != nil {
			releaseTRW(rw)
			return
		}
		if _, err := bw.Write(rw.body.Bytes()); err != nil {
			releaseTRW(rw)
			return
		}
		releaseTRW(rw)
		if err := bw.Flush(); err != nil {
			return
		}
		if !keepAlive {
			return
		}
	}
}
