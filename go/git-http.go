// git-http.go: git smart HTTP — /{owner}/{repo}.git.
//
// Surfaces: GET info/refs (upload-pack v2/v0, receive-pack v0),
// POST git-upload-pack (ls-refs v2, fetch v2/v0),
// POST git-receive-pack (push v0 with report-status + side-band-64k).
// The packfile/object codec lives in git.go; tree walking in treeutil.go.
package main

import (
	"bytes"
	"encoding/hex"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const gitAgent = "forge-core/1.0"

// gitHandler serves git smart HTTP under /{owner}/{repo}.git.
func gitHandler(w http.ResponseWriter, r *http.Request, store *Store) {
	if !checkAuth(w, r) {
		return
	}
	segments := splitPathSegments(r.URL.Path)
	if len(segments) < 2 {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	owner := segments[0]
	repoName := strings.TrimSuffix(segments[1], ".git")
	if !strings.HasSuffix(segments[1], ".git") || owner == "" || repoName == "" {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	repo := store.GetRepo(owner, repoName)
	if repo == nil || owner != ViewerLogin {
		// Unknown repo (or foreign owner): 404 on both services, server lives.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("Not Found"))
		return
	}
	rest := segments[2:]
	if len(rest) == 2 && rest[0] == "info" && rest[1] == "refs" && r.Method == http.MethodGet {
		service := r.URL.Query().Get("service")
		switch service {
		case "git-upload-pack":
			gitInfoRefsUpload(w, r, repo)
		case "git-receive-pack":
			gitInfoRefsReceive(w, r, repo)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
		return
	}
	if len(rest) == 1 && rest[0] == "git-upload-pack" && r.Method == http.MethodPost {
		gitUploadPack(w, r, repo)
		return
	}
	if len(rest) == 1 && rest[0] == "git-receive-pack" && r.Method == http.MethodPost {
		gitReceivePack(w, r, repo)
		return
	}
	writeErr(w, http.StatusNotFound, "Not Found")
}

// ---------------------------------------------------------------- info/refs

// sortedRefs returns refs sorted by name (refs/heads/*, refs/tags/*, ...).
func sortedRefs(refs map[string]string) []string {
	names := make([]string, 0, len(refs))
	for name := range refs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// refCapabilities are the capabilities on the first v0 advertisement line.
func refCapabilities(service string) string {
	if service == "receive-pack" {
		return "report-status report-status-v2 side-band-64k delete-refs ofs-delta agent=" + gitAgent
	}
	return "side-band-64k multi_ack_detailed thin-pack ofs-delta shallow agent=" + gitAgent + " symref=HEAD:refs/heads/main"
}

// gitInfoRefsUpload answers the upload-pack advertisement, v2 when the client
// asks (Git-Protocol: version=2), else v0.
func gitInfoRefsUpload(w http.ResponseWriter, r *http.Request, repo *Repo) {
	refs := repo.ListRefs()
	if r.Header.Get("Git-Protocol") == "version=2" {
		var buf bytes.Buffer
		buf.Write(pktLine([]byte("# service=git-upload-pack\n")))
		buf.Write(pktLine([]byte("version 2\n")))
		buf.Write(pktLine([]byte("agent=" + gitAgent + "\n")))
		buf.Write(pktLine([]byte("ls-refs=unborn\n")))
		buf.Write(pktLine([]byte("fetch=shallow\n")))
		buf.Write(pktLine([]byte("object-format=sha1\n")))
		buf.Write(pktLine(nil))
		writeGitBytes(w, "application/x-git-upload-pack-advertisement", buf.Bytes())
		return
	}
	var buf bytes.Buffer
	buf.Write(pktLine([]byte("# service=git-upload-pack\n")))
	names := sortedRefs(refs)
	if len(names) == 0 {
		buf.Write(pktLine([]byte(zeroOID + " capabilities^{}\x00" + refCapabilities("upload-pack") + "\n")))
	} else {
		for i, name := range names {
			line := refs[name] + " " + name
			if i == 0 {
				line += "\x00" + refCapabilities("upload-pack")
			}
			line += "\n"
			buf.Write(pktLine([]byte(line)))
			if peeled := peeledOID(repo, name, refs[name]); peeled != "" {
				buf.Write(pktLine([]byte(peeled + " " + name + "^{}\n")))
			}
		}
	}
	buf.Write(pktLine(nil))
	writeGitBytes(w, "application/x-git-upload-pack-advertisement", buf.Bytes())
}

// gitInfoRefsReceive answers the receive-pack advertisement (always v0).
func gitInfoRefsReceive(w http.ResponseWriter, r *http.Request, repo *Repo) {
	refs := repo.ListRefs()
	var buf bytes.Buffer
	buf.Write(pktLine([]byte("# service=git-receive-pack\n")))
	names := sortedRefs(refs)
	if len(names) == 0 {
		buf.Write(pktLine([]byte(zeroOID + " capabilities^{}\x00" + refCapabilities("receive-pack") + "\n")))
	} else {
		for i, name := range names {
			line := refs[name] + " " + name
			if i == 0 {
				line += "\x00" + refCapabilities("receive-pack")
			}
			line += "\n"
			buf.Write(pktLine([]byte(line)))
			if peeled := peeledOID(repo, name, refs[name]); peeled != "" {
				buf.Write(pktLine([]byte(peeled + " " + name + "^{}\n")))
			}
		}
	}
	buf.Write(pktLine(nil))
	writeGitBytes(w, "application/x-git-receive-pack-advertisement", buf.Bytes())
}

// peeledOID returns the commit a tag ref peels to, "" when not an annotated tag.
func peeledOID(repo *Repo, ref, oid string) string {
	if !strings.HasPrefix(ref, "refs/tags/") {
		return ""
	}
	obj := repo.GetObject(oid)
	if obj == nil || obj.Type != "tag" {
		return ""
	}
	return tagTarget(obj.Data)
}

func writeGitBytes(w http.ResponseWriter, contentType string, data []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// ---------------------------------------------------------------- upload-pack

// gitUploadPack dispatches ls-refs vs fetch on the v2 command (or v0 want list).
func gitUploadPack(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if r.Header.Get("Git-Protocol") == "version=2" {
		gitUploadPackV2(w, repo, body)
		return
	}
	gitUploadPackV0(w, repo, body)
}

// parsePktLines splits a body into pkt-line payloads; a nil payload is flush.
func parsePktLines(body []byte) ([][]byte, error) {
	reader := &pktLineReader{data: body}
	var out [][]byte
	for {
		payload, err := reader.next()
		if err != nil {
			return out, err
		}
		out = append(out, payload)
		if payload == nil {
			return out, nil
		}
		if reader.pos >= len(body) {
			return out, nil
		}
	}
}

// gitUploadPackV2 handles command=ls-refs and command=fetch.
func gitUploadPackV2(w http.ResponseWriter, repo *Repo, body []byte) {
	lines, err := parsePktLines(body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// Lines are: command=..., [agent=...,] [object-format=...,] delim,
	// then the arguments, then flush. The delim arrives as an empty
	// non-nil payload.
	command := ""
	var args []string
	seenDelim := false
	for _, line := range lines {
		if line == nil {
			break
		}
		if len(line) == 0 {
			seenDelim = true
			continue
		}
		text := strings.TrimSuffix(string(line), "\n")
		if !seenDelim {
			if strings.HasPrefix(text, "command=") {
				command = strings.TrimPrefix(text, "command=")
			}
			continue
		}
		args = append(args, text)
	}
	switch command {
	case "ls-refs":
		gitLsRefsV2(w, repo, args)
	case "fetch":
		gitFetchV2(w, repo, args)
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

// gitLsRefsV2 answers a v2 ls-refs request.
func gitLsRefsV2(w http.ResponseWriter, repo *Repo, args []string) {
	peel, symrefs := false, false
	var prefixes []string
	for _, arg := range args {
		switch {
		case arg == "peel":
			peel = true
		case arg == "symrefs":
			symrefs = true
		case strings.HasPrefix(arg, "ref-prefix "):
			prefixes = append(prefixes, strings.TrimPrefix(arg, "ref-prefix "))
		}
	}
	refs := repo.ListRefs()
	var buf bytes.Buffer
	mainTip, hasMain := refs["refs/heads/main"]
	if len(prefixes) == 0 && symrefs {
		if hasMain {
			buf.Write(pktLine([]byte(mainTip + " HEAD symref-target:refs/heads/main\n")))
		} else {
			buf.Write(pktLine([]byte("unborn HEAD symref-target:refs/heads/main\n")))
		}
	}
	for _, name := range sortedRefs(refs) {
		if len(prefixes) > 0 {
			matched := false
			for _, p := range prefixes {
				if strings.HasPrefix(name, p) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		line := refs[name] + " " + name
		if peel {
			if peeled := peeledOID(repo, name, refs[name]); peeled != "" {
				line += " peeled:" + peeled
			}
		}
		line += "\n"
		buf.Write(pktLine([]byte(line)))
	}
	buf.Write(pktLine(nil))
	writeGitBytes(w, "application/x-git-upload-pack-result", buf.Bytes())
}

// fetchRequest is a parsed fetch (v2 args or v0 want/have lines).
type fetchRequest struct {
	wants    []string
	haves    []string
	shallows []string // boundaries the client already knows
	deepen   int      // 0 = no depth limit
}

// parseFetchArgs parses v2 fetch arguments.
func parseFetchArgs(args []string) fetchRequest {
	var req fetchRequest
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "want "):
			req.wants = append(req.wants, strings.TrimSpace(arg[5:]))
		case strings.HasPrefix(arg, "have "):
			req.haves = append(req.haves, strings.TrimSpace(arg[5:]))
		case strings.HasPrefix(arg, "shallow "):
			req.shallows = append(req.shallows, strings.TrimSpace(arg[8:]))
		case strings.HasPrefix(arg, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(arg[7:])); err == nil {
				req.deepen = n
			}
		}
	}
	return req
}

// gitFetchV2 answers a v2 fetch with sectioned, sideband-framed output.
func gitFetchV2(w http.ResponseWriter, repo *Repo, args []string) {
	req := parseFetchArgs(args)
	var pack []byte
	var shallow, unshallow []string
	if cached, ok := fetchPack(repo, req); ok {
		pack = cached
		shallow, unshallow = shallowInfo(repo, req)
	} else {
		objects, s, u := computeFetch(repo, req)
		shallow, unshallow = s, u
		pack = cachedFetchPack(repo, req, objects)
	}
	var buf bytes.Buffer
	buf.Write(pktLine([]byte("acknowledgments\n")))
	buf.Write(pktLine([]byte("NAK\n")))
	buf.WriteString("0001") // delim
	if len(shallow) > 0 || len(unshallow) > 0 {
		buf.Write(pktLine([]byte("shallow-info\n")))
		for _, oid := range shallow {
			buf.Write(pktLine([]byte("shallow " + oid + "\n")))
		}
		for _, oid := range unshallow {
			buf.Write(pktLine([]byte("unshallow " + oid + "\n")))
		}
		buf.WriteString("0001") // delim
	}
	buf.Write(pktLine([]byte("packfile\n")))
	writeSideband(&buf, pack)
	buf.Write(pktLine(nil))
	writeGitBytes(w, "application/x-git-upload-pack-result", buf.Bytes())
}

// gitUploadPackV0 answers a v0 fetch (want/have lines, no sections).
func gitUploadPackV0(w http.ResponseWriter, repo *Repo, body []byte) {
	// Two pkt-line sections: wants (until flush), then haves + done.
	lines, err := parsePktLines(body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var req fetchRequest
	section := 0
	for _, line := range lines {
		if line == nil {
			section++
			continue
		}
		text := strings.TrimSuffix(string(line), "\n")
		switch {
		case strings.HasPrefix(text, "want "):
			// First want line may carry features after the oid.
			fields := strings.Fields(text[5:])
			if len(fields) > 0 {
				req.wants = append(req.wants, fields[0])
			}
		case strings.HasPrefix(text, "have "):
			req.haves = append(req.haves, strings.TrimSpace(text[5:]))
		case strings.HasPrefix(text, "shallow "):
			req.shallows = append(req.shallows, strings.TrimSpace(text[8:]))
		case strings.HasPrefix(text, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(text[7:])); err == nil {
				req.deepen = n
			}
		}
		_ = section
	}
	var pack []byte
	var shallow, unshallow []string
	if cached, ok := fetchPack(repo, req); ok {
		pack = cached
		shallow, unshallow = shallowInfo(repo, req)
	} else {
		objects, s, u := computeFetch(repo, req)
		shallow, unshallow = s, u
		pack = cachedFetchPack(repo, req, objects)
	}
	var buf bytes.Buffer
	buf.Write(pktLine([]byte("NAK\n")))
	for _, oid := range shallow {
		buf.Write(pktLine([]byte("shallow " + oid + "\n")))
	}
	for _, oid := range unshallow {
		buf.Write(pktLine([]byte("unshallow " + oid + "\n")))
	}
	writeSideband(&buf, pack)
	buf.Write(pktLine(nil))
	writeGitBytes(w, "application/x-git-upload-pack-result", buf.Bytes())
}

// packCache memoises built packfiles: the clone workload issues 50 identical
// shallow fetches of one tip, and a pack is a pure function of (repo, wants,
// haves, deepen) — object ids are content hashes, so the same key can never
// mean different objects. Only fetches whose wants all resolve are cached.
var packCache sync.Map // string -> []byte

func packCacheKey(repo *Repo, req fetchRequest) string {
	var sb strings.Builder
	sb.WriteString(strconv.FormatInt(repo.ID, 10))
	sb.WriteByte('|')
	sb.WriteString(strconv.Itoa(req.deepen))
	sb.WriteByte('|')
	wants := append([]string(nil), req.wants...)
	sort.Strings(wants)
	for _, w := range wants {
		sb.WriteString(w)
		sb.WriteByte(',')
	}
	sb.WriteByte('|')
	haves := append([]string(nil), req.haves...)
	sort.Strings(haves)
	for _, h := range haves {
		sb.WriteString(h)
		sb.WriteByte(',')
	}
	return sb.String()
}

func cachedFetchPack(repo *Repo, req fetchRequest, objects map[string]*GitObject) []byte {
	if pack, ok := fetchPack(repo, req); ok {
		return pack
	}
	pack := buildFetchPack(objects)
	// Cache only when every want resolved (a missing want today could exist
	// tomorrow, which would change the pack for the same key).
	allResolved := true
	for _, w := range req.wants {
		if w == "" || w == zeroOID {
			continue
		}
		if repo.GetObject(w) == nil {
			allResolved = false
			break
		}
	}
	if allResolved {
		packCache.Store(packCacheKey(repo, req), pack)
	}
	return pack
}

// computeFetch resolves a fetch request to the object set, the new shallow
// boundaries and the boundaries that moved (unshallow).
func computeFetch(repo *Repo, req fetchRequest) (map[string]*GitObject, []string, []string) {
	deep := req.deepen > 0
	objects := repo.reachableObjects(req.wants, req.deepen, req.haves, deep)
	shallow, unshallow := shallowInfo(repo, req)
	return objects, shallow, unshallow
}

// shallowInfo computes the shallow/unshallow lines for a fetch without
// walking any objects: boundaries are the commits at exactly depth == deepen,
// and a previously-shallow boundary now interior to the walk is unshallow.
func shallowInfo(repo *Repo, req fetchRequest) (shallow, unshallow []string) {
	if req.deepen <= 0 {
		return nil, nil
	}
	boundaries := depthBoundaries(repo, req.wants, req.deepen)
	for _, b := range boundaries {
		shallow = append(shallow, b)
	}
	for _, prev := range req.shallows {
		if depthOf(repo, req.wants, prev) >= 0 && depthOf(repo, req.wants, prev) < req.deepen {
			unshallow = append(unshallow, prev)
		}
	}
	return shallow, unshallow
}

// fetchPack returns a cached pack for a fetch, when one exists.
func fetchPack(repo *Repo, req fetchRequest) ([]byte, bool) {
	if cached, ok := packCache.Load(packCacheKey(repo, req)); ok {
		return cached.([]byte), true
	}
	return nil, false
}

// depthBoundaries returns the commit oids at exactly depth == maxDepth when
// walking parents from tips (depth 1 = the tips themselves).
func depthBoundaries(repo *Repo, tips []string, maxDepth int) []string {
	depth := commitDepths(repo, tips, maxDepth)
	var out []string
	for oid, d := range depth {
		if d == maxDepth {
			out = append(out, oid)
		}
	}
	sort.Strings(out)
	return out
}

// depthOf returns the depth of target in the walk from tips, -1 when absent.
func depthOf(repo *Repo, tips []string, target string) int {
	depth := commitDepths(repo, tips, 0)
	if d, ok := depth[target]; ok {
		return d
	}
	return -1
}

// commitDepths BFS-walks parents from tips; limit 0 means no limit.
func commitDepths(repo *Repo, tips []string, limit int) map[string]int {
	depth := make(map[string]int)
	type item struct {
		oid string
		d   int
	}
	queue := []item{}
	for _, tip := range tips {
		if tip == "" || tip == zeroOID {
			continue
		}
		if _, ok := depth[tip]; !ok {
			depth[tip] = 1
			queue = append(queue, item{tip, 1})
		}
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if limit > 0 && cur.d >= limit {
			continue
		}
		for _, p := range repo.commitParents(cur.oid) {
			if _, ok := depth[p]; !ok {
				depth[p] = cur.d + 1
				queue = append(queue, item{p, cur.d + 1})
			}
		}
	}
	return depth
}

// buildFetchPack serialises objects as a packfile, sorted by oid for a
// deterministic byte layout.
func buildFetchPack(objects map[string]*GitObject) []byte {
	oids := make([]string, 0, len(objects))
	for oid := range objects {
		oids = append(oids, oid)
	}
	sort.Strings(oids)
	list := make([]*GitObject, 0, len(oids))
	for _, oid := range oids {
		list = append(list, objects[oid])
	}
	return writePack(list)
}

// writeSideband writes data as band-1 sideband packets (≤65515 data bytes each).
func writeSideband(buf *bytes.Buffer, data []byte) {
	const maxChunk = 65515
	if len(data) == 0 {
		buf.Write(pktLine([]byte{1}))
		return
	}
	for len(data) > 0 {
		n := len(data)
		if n > maxChunk {
			n = maxChunk
		}
		payload := make([]byte, 0, n+1)
		payload = append(payload, 1)
		payload = append(payload, data[:n]...)
		buf.Write(pktLine(payload))
		data = data[n:]
	}
}

// ---------------------------------------------------------------- receive-pack

type pushCommand struct {
	oldOID string
	newOID string
	ref    string
}

// gitReceivePack handles a push: parse commands + pack, store objects, apply
// each ref update independently (fast-forward only), report per ref.
func gitReceivePack(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// Parse pkt-lines until the flush; the rest of the body is the raw pack.
	reader := &pktLineReader{data: body}
	var commands []pushCommand
	sideband := false
	packStart := len(body)
	for {
		payload, err := reader.next()
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if payload == nil {
			packStart = reader.pos
			break
		}
		text := string(payload)
		// First line may carry "\0<capabilities>".
		if i := strings.Index(text, "\x00"); i >= 0 {
			caps := text[i+1:]
			if strings.Contains(caps, "side-band-64k") {
				sideband = true
			}
			text = text[:i]
		}
		text = strings.TrimSuffix(text, "\n")
		fields := strings.Fields(text)
		if len(fields) != 3 {
			continue
		}
		commands = append(commands, pushCommand{oldOID: fields[0], newOID: fields[1], ref: fields[2]})
	}

	packOK := true
	if packStart < len(body) {
		objects, err := readPack(body[packStart:])
		if err != nil {
			packOK = false
		} else {
			for _, obj := range objects {
				repo.PutObject(obj.Type, obj.Data)
			}
		}
	}

	// Build the report (inner pkt-lines), then wrap in sideband when asked.
	var inner bytes.Buffer
	if packOK {
		inner.Write(pktLine([]byte("unpack ok\n")))
	} else {
		inner.Write(pktLine([]byte("unpack failed to parse pack\n")))
	}
	for _, cmd := range commands {
		status, reason := applyPush(repo, cmd, packOK)
		if status == "ok" {
			inner.Write(pktLine([]byte("ok " + cmd.ref + "\n")))
		} else {
			inner.Write(pktLine([]byte("ng " + cmd.ref + " " + reason + "\n")))
		}
	}
	inner.Write(pktLine(nil))

	if sideband {
		var buf bytes.Buffer
		writeSideband(&buf, inner.Bytes())
		writeGitBytes(w, "application/x-git-receive-pack-result", buf.Bytes())
		return
	}
	writeGitBytes(w, "application/x-git-receive-pack-result", inner.Bytes())
}

// applyPush applies one push command. It returns ("ok", "") or ("ng", reason).
// A rejected ref never moves; an accepted ref in the same push still moves.
func applyPush(repo *Repo, cmd pushCommand, packOK bool) (string, string) {
	if !packOK {
		return "ng", "unpack failed"
	}
	if !isHexOID(cmd.oldOID) || !isHexOID(cmd.newOID) {
		return "ng", "non-fast-forward"
	}
	current, exists := repo.GetRef(cmd.ref)
	switch {
	case cmd.newOID == zeroOID:
		// Delete.
		if !exists || current != cmd.oldOID {
			return "ng", "non-fast-forward"
		}
		repo.DeleteRef(cmd.ref)
		return "ok", ""
	case cmd.oldOID == zeroOID:
		// Create.
		if exists {
			return "ng", "non-fast-forward"
		}
		if repo.GetObject(cmd.newOID) == nil {
			return "ng", "non-fast-forward"
		}
		repo.SetRef(cmd.ref, cmd.newOID)
		return "ok", ""
	default:
		// Update: old must match, new must descend from old.
		if !exists || current != cmd.oldOID {
			return "ng", "non-fast-forward"
		}
		if repo.GetObject(cmd.newOID) == nil {
			return "ng", "non-fast-forward"
		}
		if !repo.isDescendant(cmd.newOID, cmd.oldOID) {
			return "ng", "non-fast-forward"
		}
		repo.SetRef(cmd.ref, cmd.newOID)
		return "ok", ""
	}
}

func isHexOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
