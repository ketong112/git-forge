// transport.go: prototype hand-rolled HTTP/1.1 server over raw TCP.
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
	br := bufio.NewReaderSize(conn, 32<<10)
	bw := bufio.NewWriterSize(conn, 32<<10)
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
		parts := strings.SplitN(line, " ", 3)
		if len(parts) != 3 {
			return
		}
		method, target, proto := parts[0], parts[1], parts[2]
		if !strings.HasPrefix(proto, "HTTP/") {
			return
		}
		// Headers.
		hdr := make(http.Header)
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
			key := textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(hline[:colon]))
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
				if strings.Contains(strings.ToLower(val), "chunked") {
					chunked = true
				}
			case "Expect":
				if strings.EqualFold(val, "100-continue") {
					expectContinue = true
				}
			case "Connection":
				lv := strings.ToLower(val)
				if strings.Contains(lv, "close") {
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
			body = make([]byte, contentLength)
			if _, err := io.ReadFull(br, body); err != nil {
				return
			}
		} else if contentLength == 0 {
			body = nil
		}
		u, err := url.ParseRequestURI(target)
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
			RemoteAddr:    conn.RemoteAddr().String(),
			RequestURI:    target,
		}
		if strings.HasPrefix(proto, "HTTP/1.0") {
			req.ProtoMinor = 0
		}
		rw := &transportResponseWriter{}
		handler.ServeHTTP(rw, req)
		status := rw.status
		if status == 0 {
			status = http.StatusOK
		}
		// Frame the response: exact Content-Length from the buffered body.
		var sb strings.Builder
		sb.WriteString("HTTP/1.1 ")
		sb.WriteString(strconv.Itoa(status))
		sb.WriteByte(' ')
		sb.WriteString(http.StatusText(status))
		sb.WriteString("\r\n")
		hdrOut := rw.Header()
		// Drop any handler-set Content-Length; ours is authoritative.
		hdrOut.Del("Content-Length")
		hdrOut.Del("Connection")
		for k, vs := range hdrOut {
			for _, v := range vs {
				sb.WriteString(k)
				sb.WriteString(": ")
				sb.WriteString(v)
				sb.WriteString("\r\n")
			}
		}
		sb.WriteString("Content-Length: ")
		sb.WriteString(strconv.Itoa(rw.body.Len()))
		sb.WriteString("\r\n")
		keepAlive := !connClose && proto != "HTTP/1.0"
		if !keepAlive {
			sb.WriteString("Connection: close\r\n")
		}
		sb.WriteString("\r\n")
		if _, err := bw.WriteString(sb.String()); err != nil {
			return
		}
		if _, err := bw.Write(rw.body.Bytes()); err != nil {
			return
		}
		if err := bw.Flush(); err != nil {
			return
		}
		if !keepAlive {
			return
		}
	}
}
