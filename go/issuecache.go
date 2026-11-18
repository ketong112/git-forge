// issuecache.go: serialized-issue response cache for the REST hot paths.
//
// GET /repos/{owner}/{repo}/issues/{number} is the read workload: 2,000
// sequential reads of the same issue on one connection. Rebuilding the
// struct and re-running encoding/json on every read is pure waste, so the
// first render is cached as wire bytes (body + trailing newline) per (repo,
// issue number) and served verbatim on later reads.
//
// Correctness rules:
//
//   - The cache stores body bytes only, never headers: X-GitHub-Request-Id
//     is still generated fresh per request by writeRawJSON.
//   - Every entry snapshots the issue fields that feed the render (title,
//     body, state, comment count, rendered labels). A hit re-validates the
//     snapshot against the live issue under the repo read lock, so a
//     mutation from ANY surface — REST PATCH/comment, GraphQL closeIssue /
//     addComment, MCP createIssue — is observed and the entry is rebuilt
//     instead of served stale. Explicit invalidation in rest.go (PATCH,
//     comment POST, repo DELETE) is a belt-and-braces shortcut, not the
//     correctness mechanism.
//   - The key is the repo pointer, not its name: a repo deleted and
//     recreated under the same name is a different *Repo, so its entries can
//     never collide with the deleted repo's. Repo DELETE also purges the
//     repo's entries outright.
//   - Snapshot and bytes are consistent by construction: entries are stored
//     from the issueOut struct the bytes were marshaled from.
package main

import (
	"encoding/json"
	"sync"
)

// issueCacheKey identifies one cached issue render.
type issueCacheKey struct {
	repo   *Repo
	number int
}

// issueCacheEntry is one cached render plus the snapshot it was built from.
type issueCacheEntry struct {
	wire      []byte // exact response bytes, trailing "\n" included
	title     string
	body      string
	state     string
	nComments int
	labels    []labelOut
}

var issueCacheMu sync.RWMutex
var issueCache = make(map[issueCacheKey]*issueCacheEntry)

// labelsEqualLocked renders the issue's labels the way fastIssueJSON does
// and compares them with the snapshot. Caller must hold repo.mu for reading.
func labelsEqualLocked(repo *Repo, issue *Issue, snap []labelOut) bool {
	if len(issue.Labels) != len(snap) {
		return false
	}
	for i, name := range issue.Labels {
		var want labelOut
		if label := repo.labels[name]; label != nil {
			want = labelOut{ID: label.ID, Name: label.Name, Color: label.Color}
		} else {
			want = labelOut{Name: name}
		}
		if want != snap[i] {
			return false
		}
	}
	return true
}

// issueCacheGet returns cached wire bytes when the entry exists and its
// snapshot still matches the live issue.
func issueCacheGet(repo *Repo, issue *Issue) ([]byte, bool) {
	issueCacheMu.RLock()
	e := issueCache[issueCacheKey{repo, issue.Number}]
	issueCacheMu.RUnlock()
	if e == nil {
		return nil, false
	}
	repo.mu.RLock()
	ok := issue.Title == e.title && issue.Body == e.body && issue.State == e.state &&
		len(issue.Comments) == e.nComments && labelsEqualLocked(repo, issue, e.labels)
	repo.mu.RUnlock()
	if !ok {
		return nil, false
	}
	return e.wire, true
}

// issueCacheStore records wire bytes built from out (the struct the bytes
// were marshaled from, so snapshot and bytes agree by construction).
func issueCacheStore(repo *Repo, out issueOut, wire []byte) {
	labels := make([]labelOut, len(out.Labels))
	copy(labels, out.Labels)
	e := &issueCacheEntry{
		wire:      wire,
		title:     out.Title,
		body:      out.Body,
		state:     out.State,
		nComments: out.Comments,
		labels:    labels,
	}
	issueCacheMu.Lock()
	issueCache[issueCacheKey{repo, out.Number}] = e
	issueCacheMu.Unlock()
}

// issueCacheStoreRaw records hand-built wire bytes with an explicit snapshot
// (used by the POST /issues fast path, which never builds an issueOut).
func issueCacheStoreRaw(repo *Repo, number int, wire []byte, title, body string) {
	e := &issueCacheEntry{
		wire:      wire,
		title:     title,
		body:      body,
		state:     "open",
		nComments: 0,
		labels:    nil,
	}
	issueCacheMu.Lock()
	issueCache[issueCacheKey{repo, number}] = e
	issueCacheMu.Unlock()
}

// issueCacheInvalidate drops one issue's entry (REST comment POST).
func issueCacheInvalidate(repo *Repo, number int) {
	issueCacheMu.Lock()
	delete(issueCache, issueCacheKey{repo, number})
	issueCacheMu.Unlock()
}

// issueCachePurgeRepo drops every entry of a repo (REST repo DELETE).
func issueCachePurgeRepo(repo *Repo) {
	issueCacheMu.Lock()
	for k := range issueCache {
		if k.repo == repo {
			delete(issueCache, k)
		}
	}
	issueCacheMu.Unlock()
}

// buildAndCacheIssueJSON renders an issue exactly like fastIssueJSON, stores
// the wire bytes, and returns them. It returns nil only if marshaling fails,
// which cannot happen for issueOut.
func buildAndCacheIssueJSON(repo *Repo, issue *Issue) []byte {
	out := fastIssueJSON(repo, issue)
	data, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	wire := append(data, '\n')
	issueCacheStore(repo, out, wire)
	return wire
}
