// git-http.go: git smart HTTP — /{owner}/{repo}.git.
//
// Surfaces: GET info/refs (upload-pack v2/v0, receive-pack v0),
// POST git-upload-pack (ls-refs v2, fetch v2/v0),
// POST git-receive-pack (push v0 with report-status + side-band-64k).
// The packfile/object codec lives in git.go; tree walking in treeutil.go.
package main

import (
	"bytes"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

// infoRefsCache memoises ref advertisements: the bytes are a pure function of
// (repo, refsVersion) for a given service/variant, and refs only change via
// SetRef/DeleteRef (which bump refsVersion). Keyed by a string the builders
// below construct; values are immutable response bodies, served as one Write.
var infoRefsCache sync.Map // string -> []byte

func refsVersionOf(repo *Repo) uint64 {
	return atomic.LoadUint64(&repo.refsVersion)
}

func infoRefsKey(repo *Repo, variant string) string {
	return variant + "|" + strconv.FormatInt(repo.ID, 10) + "|" + strconv.FormatUint(refsVersionOf(repo), 10)
}

// uploadPackV2Ad is the v2 upload-pack advertisement: it names no refs, so it
// is identical for every repository and is built once.
var uploadPackV2Ad = func() []byte {
	buf := make([]byte, 0, 128)
	buf = appendPktLine(buf, []byte("# service=git-upload-pack\n"))
	buf = appendPktLine(buf, []byte("version 2\n"))
	buf = appendPktLine(buf, []byte("agent="+gitAgent+"\n"))
	buf = appendPktLine(buf, []byte("ls-refs=unborn\n"))
	buf = appendPktLine(buf, []byte("fetch=shallow\n"))
	buf = appendPktLine(buf, []byte("object-format=sha1\n"))
	buf = appendPktLine(buf, nil)
	return buf
}()

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
	if r.Header.Get("Git-Protocol") == "version=2" {
		// The v2 advertisement names no refs: one precomputed body.
		writeGitBytes(w, "application/x-git-upload-pack-advertisement", uploadPackV2Ad)
		return
	}
	if cached, ok := infoRefsCache.Load(infoRefsKey(repo, "up0")); ok {
		writeGitBytes(w, "application/x-git-upload-pack-advertisement", cached.([]byte))
		return
	}
	refs := repo.ListRefs()
	buf := make([]byte, 0, 256)
	buf = appendPktLine(buf, []byte("# service=git-upload-pack\n"))
	names := sortedRefs(refs)
	if len(names) == 0 {
		buf = appendPktLine(buf, []byte(zeroOID+" capabilities^{}\x00"+refCapabilities("upload-pack")+"\n"))
	} else {
		for i, name := range names {
			line := refs[name] + " " + name
			if i == 0 {
				line += "\x00" + refCapabilities("upload-pack")
			}
			line += "\n"
			buf = appendPktLine(buf, []byte(line))
			if peeled := peeledOID(repo, name, refs[name]); peeled != "" {
				buf = appendPktLine(buf, []byte(peeled+" "+name+"^{}\n"))
			}
		}
	}
	buf = appendPktLine(buf, nil)
	infoRefsCache.Store(infoRefsKey(repo, "up0"), buf)
	writeGitBytes(w, "application/x-git-upload-pack-advertisement", buf)
}

// gitInfoRefsReceive answers the receive-pack advertisement (always v0).
func gitInfoRefsReceive(w http.ResponseWriter, r *http.Request, repo *Repo) {
	if cached, ok := infoRefsCache.Load(infoRefsKey(repo, "recv")); ok {
		writeGitBytes(w, "application/x-git-receive-pack-advertisement", cached.([]byte))
		return
	}
	refs := repo.ListRefs()
	buf := make([]byte, 0, 256)
	buf = appendPktLine(buf, []byte("# service=git-receive-pack\n"))
	names := sortedRefs(refs)
	if len(names) == 0 {
		buf = appendPktLine(buf, []byte(zeroOID+" capabilities^{}\x00"+refCapabilities("receive-pack")+"\n"))
	} else {
		for i, name := range names {
			line := refs[name] + " " + name
			if i == 0 {
				line += "\x00" + refCapabilities("receive-pack")
			}
			line += "\n"
			buf = appendPktLine(buf, []byte(line))
			if peeled := peeledOID(repo, name, refs[name]); peeled != "" {
				buf = appendPktLine(buf, []byte(peeled+" "+name+"^{}\n"))
			}
		}
	}
	buf = appendPktLine(buf, nil)
	infoRefsCache.Store(infoRefsKey(repo, "recv"), buf)
	writeGitBytes(w, "application/x-git-receive-pack-advertisement", buf)
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
	// No Content-Length: transport.go frames the response with an exact one
	// and drops any handler-set value (see transport.go framing).
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
	// non-nil payload. Arguments are compared as bytes; only the command
	// value and the argument strings are materialised.
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
		text := bytes.TrimSuffix(line, []byte("\n"))
		if !seenDelim {
			if bytes.HasPrefix(text, []byte("command=")) {
				command = string(text[len("command="):])
			}
			continue
		}
		args = append(args, string(text))
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

// lsRefsCache memoises v2 ls-refs responses: the bytes are a pure function of
// (repo, refsVersion, peel, symrefs, ref-prefix set). Ref changes bump
// refsVersion; a tag object is content-addressed, so the same version can
// never peel to a different target.
var lsRefsCache sync.Map // string -> []byte

func lsRefsKey(repo *Repo, peel, symrefs bool, prefixes []string) string {
	var sb strings.Builder
	sb.WriteString(strconv.FormatInt(repo.ID, 10))
	sb.WriteByte('|')
	sb.WriteString(strconv.FormatUint(refsVersionOf(repo), 10))
	sb.WriteByte('|')
	if peel {
		sb.WriteByte('p')
	}
	if symrefs {
		sb.WriteByte('s')
	}
	sb.WriteByte('|')
	sorted := append([]string(nil), prefixes...)
	sort.Strings(sorted)
	for _, p := range sorted {
		sb.WriteString(p)
		sb.WriteByte(',')
	}
	return sb.String()
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
	key := lsRefsKey(repo, peel, symrefs, prefixes)
	if cached, ok := lsRefsCache.Load(key); ok {
		writeGitBytes(w, "application/x-git-upload-pack-result", cached.([]byte))
		return
	}
	refs := repo.ListRefs()
	buf := make([]byte, 0, 256)
	mainTip, hasMain := refs["refs/heads/main"]
	if len(prefixes) == 0 && symrefs {
		if hasMain {
			buf = appendPktLine(buf, []byte(mainTip+" HEAD symref-target:refs/heads/main\n"))
		} else {
			buf = appendPktLine(buf, []byte("unborn HEAD symref-target:refs/heads/main\n"))
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
		buf = appendPktLine(buf, []byte(line))
	}
	buf = appendPktLine(buf, nil)
	lsRefsCache.Store(key, buf)
	writeGitBytes(w, "application/x-git-upload-pack-result", buf)
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

// fetchRespCache memoises whole fetch responses (framing + sideband + pack):
// a fetch response is a pure function of (repo, wants, haves, deepen,
// shallows) — objects are content-addressed, so the same key can never mean
// different bytes. Values are immutable and served as a single Write.
var fetchRespCache sync.Map // string -> []byte

// fetchBuildMu single-flights fetch response builds: when 50 concurrent
// clones miss the cache together, only one builds the pack and the response;
// the rest wait and then hit the cache.
var fetchBuildMu sync.Mutex

func fetchRespKey(repo *Repo, req fetchRequest, v2 bool) string {
	prefix := "v0|"
	if v2 {
		prefix = "v2|"
	}
	key := prefix + packCacheKey(repo, req)
	if len(req.shallows) == 0 {
		return key
	}
	shallows := append([]string(nil), req.shallows...)
	sort.Strings(shallows)
	var sb strings.Builder
	sb.WriteString(key)
	sb.WriteString("|shallow:")
	for _, s := range shallows {
		sb.WriteString(s)
		sb.WriteByte(',')
	}
	return sb.String()
}

// fetchAllResolved reports whether every want in req resolves to a stored
// object (a missing want today could exist tomorrow, so such a response is
// never cached). Mirrors the gate in cachedFetchPack.
func fetchAllResolved(repo *Repo, req fetchRequest) bool {
	for _, w := range req.wants {
		if w == "" || w == zeroOID {
			continue
		}
		if repo.GetObject(w) == nil {
			return false
		}
	}
	return true
}

// buildFetchResponseV2 frames a v2 fetch response: acknowledgments, optional
// shallow-info, packfile sideband, flush.
func buildFetchResponseV2(pack []byte, shallow, unshallow []string) []byte {
	buf := make([]byte, 0, len(pack)+256)
	buf = appendPktLine(buf, []byte("acknowledgments\n"))
	buf = appendPktLine(buf, []byte("NAK\n"))
	buf = append(buf, "0001"...)
	if len(shallow) > 0 || len(unshallow) > 0 {
		buf = appendPktLine(buf, []byte("shallow-info\n"))
		for _, oid := range shallow {
			buf = appendPktLine(buf, []byte("shallow "+oid+"\n"))
		}
		for _, oid := range unshallow {
			buf = appendPktLine(buf, []byte("unshallow "+oid+"\n"))
		}
		buf = append(buf, "0001"...)
	}
	buf = appendPktLine(buf, []byte("packfile\n"))
	buf = appendSideband(buf, pack)
	buf = appendPktLine(buf, nil)
	return buf
}

// buildFetchResponseV0 frames a v0 fetch response: NAK, shallow lines,
// sideband pack, flush.
func buildFetchResponseV0(pack []byte, shallow, unshallow []string) []byte {
	buf := make([]byte, 0, len(pack)+128)
	buf = appendPktLine(buf, []byte("NAK\n"))
	for _, oid := range shallow {
		buf = appendPktLine(buf, []byte("shallow "+oid+"\n"))
	}
	for _, oid := range unshallow {
		buf = appendPktLine(buf, []byte("unshallow "+oid+"\n"))
	}
	buf = appendSideband(buf, pack)
	buf = appendPktLine(buf, nil)
	return buf
}

// cachedFetchResponse returns a precomputed fetch response when one is cached
// (or can be built and cached); ok is false when the request must not be
// cached (an unresolvable want), in which case the caller builds fresh.
func cachedFetchResponse(repo *Repo, req fetchRequest, v2 bool) ([]byte, bool) {
	key := fetchRespKey(repo, req, v2)
	if cached, ok := fetchRespCache.Load(key); ok {
		return cached.([]byte), true
	}
	fetchBuildMu.Lock()
	defer fetchBuildMu.Unlock()
	if cached, ok := fetchRespCache.Load(key); ok {
		return cached.([]byte), true
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
	if !fetchAllResolved(repo, req) {
		return nil, false
	}
	var resp []byte
	if v2 {
		resp = buildFetchResponseV2(pack, shallow, unshallow)
	} else {
		resp = buildFetchResponseV0(pack, shallow, unshallow)
	}
	fetchRespCache.Store(key, resp)
	return resp, true
}

// gitFetchV2 answers a v2 fetch with sectioned, sideband-framed output.
func gitFetchV2(w http.ResponseWriter, repo *Repo, args []string) {
	req := parseFetchArgs(args)
	if resp, ok := cachedFetchResponse(repo, req, true); ok {
		writeGitBytes(w, "application/x-git-upload-pack-result", resp)
		return
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
	writeGitBytes(w, "application/x-git-upload-pack-result", buildFetchResponseV2(pack, shallow, unshallow))
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
	if resp, ok := cachedFetchResponse(repo, req, false); ok {
		writeGitBytes(w, "application/x-git-upload-pack-result", resp)
		return
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
	writeGitBytes(w, "application/x-git-upload-pack-result", buildFetchResponseV0(pack, shallow, unshallow))
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

// appendSideband appends data as band-1 sideband packets (≤65515 data bytes
// each) to buf. Identical bytes to the previous writeSideband.
func appendSideband(buf []byte, data []byte) []byte {
	const maxChunk = 65515
	if len(data) == 0 {
		return appendPktLine(buf, []byte{1})
	}
	for len(data) > 0 {
		n := len(data)
		if n > maxChunk {
			n = maxChunk
		}
		length := 4 + 1 + n
		buf = append(buf,
			hexDigit(byte(length>>12)),
			hexDigit(byte(length>>8)),
			hexDigit(byte(length>>4)),
			hexDigit(byte(length)),
			1)
		buf = append(buf, data[:n]...)
		data = data[n:]
	}
	return buf
}

// ---------------------------------------------------------------- receive-pack

type pushCommand struct {
	oldOID string
	newOID string
	ref    string
}

// fieldsOf splits a line on ASCII whitespace runs, mirroring strings.Fields
// for the ASCII inputs a git client sends (oids, ref names, capabilities).
// A field is returned as a subslice of line: no allocation.
func fieldsOf(line []byte) [][]byte {
	var out [][]byte
	i := 0
	for i < len(line) {
		for i < len(line) && isSpaceByte(line[i]) {
			i++
		}
		if i >= len(line) {
			break
		}
		start := i
		for i < len(line) && !isSpaceByte(line[i]) {
			i++
		}
		out = append(out, line[start:i])
	}
	return out
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
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
	// Command lines are parsed on the raw bytes: no string(payload) copy,
	// no strings.Fields allocation per line.
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
		line := payload
		// First line may carry "\0<capabilities>".
		if i := bytes.IndexByte(line, 0); i >= 0 {
			if bytes.Contains(line[i+1:], []byte("side-band-64k")) {
				sideband = true
			}
			line = line[:i]
		}
		line = bytes.TrimSuffix(line, []byte("\n"))
		fields := fieldsOf(line)
		if len(fields) != 3 {
			continue
		}
		commands = append(commands, pushCommand{oldOID: string(fields[0]), newOID: string(fields[1]), ref: string(fields[2])})
	}

	packOK := true
	if packStart < len(body) {
		objects, err := readPack(body[packStart:])
		if err != nil {
			packOK = false
		} else {
			// readPack already computed each oid once; store precomputed
			// instead of hashing every object a second time in PutObject.
			for _, po := range objects {
				repo.PutObjectPrecomputed(po.oid, po.obj)
			}
		}
	}

	// Build the report (inner pkt-lines), then wrap in sideband when asked.
	inner := make([]byte, 0, 128)
	if packOK {
		inner = appendPktLine(inner, []byte("unpack ok\n"))
	} else {
		inner = appendPktLine(inner, []byte("unpack failed to parse pack\n"))
	}
	for _, cmd := range commands {
		status, reason := applyPush(repo, cmd, packOK)
		if status == "ok" {
			inner = appendPktLine(inner, []byte("ok "+cmd.ref+"\n"))
		} else {
			inner = appendPktLine(inner, []byte("ng "+cmd.ref+" "+reason+"\n"))
		}
	}
	inner = appendPktLine(inner, nil)

	if sideband {
		writeGitBytes(w, "application/x-git-receive-pack-result", appendSideband(nil, inner))
		return
	}
	writeGitBytes(w, "application/x-git-receive-pack-result", inner)
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
	for i := 0; i < len(s); i++ {
		if hexVal(s[i]) < 0 {
			return false
		}
	}
	return true
}
