// rest.go: the REST v3 surface (/api/v3).
//
// Contract (from the suite's checks-rest.ts): every reply is application/json
// with a non-empty X-GitHub-Request-Id; errors are JSON {"message": ...};
// missing token is 401; a repo under any owner other than the token's login
// is 404, never 403. Extra fields are allowed everywhere, so responses carry
// the GitHub-shaped fields the checks read plus the usual extras.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// ---------------------------------------------------------------- helpers

// readJSONBody decodes a JSON object body. A malformed body is 400.
func readJSONBody(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Bad Request")
		return nil, false
	}
	if len(data) == 0 {
		return map[string]any{}, true
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "Problems parsing JSON")
		return nil, false
	}
	if body == nil {
		body = map[string]any{}
	}
	return body, true
}

func bodyString(body map[string]any, key string) string {
	if v, ok := body[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func bodyBool(body map[string]any, key string, def bool) bool {
	if v, ok := body[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

// writeNoContent answers 204 with the request-id header and no body.
func writeNoContent(w http.ResponseWriter) {
	w.Header().Set("X-GitHub-Request-Id", requestID())
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusNoContent)
}

// repoJSON is the repository representation.
func repoJSON(repo *Repo) map[string]any {
	return map[string]any{
		"id":             repo.ID,
		"node_id":        "R_" + strconv.FormatInt(repo.ID, 10),
		"name":           repo.Name,
		"full_name":      repo.Owner + "/" + repo.Name,
		"owner":          map[string]any{"login": repo.Owner, "id": 1000, "type": "User"},
		"private":        repo.Private,
		"description":    repo.Description,
		"default_branch": repo.DefaultBranch,
		"created_at":     FixedTimestamp,
		"updated_at":     FixedTimestamp,
		"has_issues":     true,
		"html_url":       "https://example.invalid/" + repo.Owner + "/" + repo.Name,
	}
}

func labelJSON(label *Label) map[string]any {
	return map[string]any{
		"id":          label.ID,
		"node_id":     "L_" + strconv.FormatInt(label.ID, 10),
		"name":        label.Name,
		"color":       label.Color,
		"default":     false,
		"description": "",
	}
}

// --- fast struct-based issue encoding (read/write hot paths) ---

type labelOut struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color,omitempty"`
}

type userOut struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Type  string `json:"type"`
}

type issueOut struct {
	ID        int64      `json:"id"`
	NodeID    string     `json:"node_id"`
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	State     string     `json:"state"`
	Labels    []labelOut `json:"labels"`
	User      userOut    `json:"user"`
	CreatedAt string     `json:"created_at"`
	UpdatedAt string     `json:"updated_at"`
	Comments  int        `json:"comments"`
}

// fastIssueJSON builds the issue representation with struct marshaling
// (much cheaper than nested maps) under the repo read lock.
func fastIssueJSON(repo *Repo, issue *Issue) issueOut {
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	labels := make([]labelOut, 0, len(issue.Labels))
	for _, name := range issue.Labels {
		if label := repo.labels[name]; label != nil {
			labels = append(labels, labelOut{ID: label.ID, Name: label.Name, Color: label.Color})
		} else {
			labels = append(labels, labelOut{Name: name})
		}
	}
	return issueOut{
		ID:        issue.ID,
		NodeID:    "I_" + strconv.FormatInt(issue.ID, 10),
		Number:    issue.Number,
		Title:     issue.Title,
		Body:      issue.Body,
		State:     issue.State,
		Labels:    labels,
		User:      userOut{Login: ViewerLogin, ID: 1000, Type: "User"},
		CreatedAt: FixedTimestamp,
		UpdatedAt: FixedTimestamp,
		Comments:  len(issue.Comments),
	}
}

// issueCreateIn is the struct-decoded POST /issues body.
type issueCreateIn struct {
	Title  string            `json:"title"`
	Body   string            `json:"body"`
	Labels []json.RawMessage `json:"labels"`
}

// parseLabelNames extracts label names from raw JSON values (strings or
// {"name": ...} objects).
func parseLabelNames(raw []json.RawMessage) []string {
	var out []string
	for _, r := range raw {
		if len(r) > 0 && r[0] == '"' {
			var s string
			if json.Unmarshal(r, &s) == nil {
				out = append(out, s)
			}
			continue
		}
		var obj struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(r, &obj) == nil && obj.Name != "" {
			out = append(out, obj.Name)
		}
	}
	return out
}

func issueJSON(issue *Issue) map[string]any {
	labels := make([]any, 0, len(issue.Labels))
	for _, name := range issue.Labels {
		labels = append(labels, map[string]any{"name": name})
	}
	return map[string]any{
		"id":         issue.ID,
		"node_id":    "I_" + strconv.FormatInt(issue.ID, 10),
		"number":     issue.Number,
		"title":      issue.Title,
		"body":       issue.Body,
		"state":      issue.State,
		"labels":     labels,
		"user":       map[string]any{"login": ViewerLogin, "id": 1000, "type": "User"},
		"created_at": FixedTimestamp,
		"updated_at": FixedTimestamp,
		"comments":   len(issue.Comments),
	}
}

// issueJSONFull includes the label objects with ids (for single-issue reads
// the suite only needs names, but the full shape is the same either way).
func issueJSONFull(repo *Repo, issue *Issue) map[string]any {
	m := issueJSON(issue)
	labels := make([]any, 0, len(issue.Labels))
	for _, name := range issue.Labels {
		if label := repo.labels[name]; label != nil {
			labels = append(labels, labelJSON(label))
		} else {
			labels = append(labels, map[string]any{"name": name})
		}
	}
	m["labels"] = labels
	return m
}

func pullJSON(pr *PullRequest, owner string) map[string]any {
	mergedAt := any(nil)
	mergeSHA := any(nil)
	if pr.Merged {
		mergedAt = pr.MergedAt
		mergeSHA = pr.MergeSHA
	}
	return map[string]any{
		"id":      pr.ID,
		"node_id": "P_" + strconv.FormatInt(pr.ID, 10),
		"number":  pr.Number,
		"title":   pr.Title,
		"body":    pr.Body,
		"state":   pr.State,
		"head": map[string]any{
			"ref":   pr.Head,
			"sha":   pr.HeadSHA,
			"label": owner + ":" + pr.Head,
		},
		"base": map[string]any{
			"ref":   pr.Base,
			"sha":   pr.BaseSHA,
			"label": owner + ":" + pr.Base,
		},
		"merged":           pr.Merged,
		"merged_at":        mergedAt,
		"merge_commit_sha": mergeSHA,
		"mergeable":        pr.Mergeable,
		"user":             map[string]any{"login": ViewerLogin, "id": 1000, "type": "User"},
		"created_at":       FixedTimestamp,
		"updated_at":       FixedTimestamp,
	}
}

// lookupRepo resolves /repos/{owner}/{repo}; owner must be the viewer.
func lookupRepo(w http.ResponseWriter, store *Store, owner, name string) *Repo {
	if owner != ViewerLogin {
		writeErr(w, http.StatusNotFound, "Not Found")
		return nil
	}
	repo := store.GetRepo(owner, name)
	if repo == nil {
		writeErr(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return repo
}

// ---------------------------------------------------------------- dispatch

// restHandler serves the REST v3 API under /api/v3.
func restHandler(w http.ResponseWriter, r *http.Request, store *Store) {
	if !checkAuth(w, r) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v3")
	segments := splitPathSegments(path)

	if len(segments) == 0 {
		// GET /api/v3/ — root.
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		w.Header().Set("X-OAuth-Scopes", "repo, user")
		writeJSON(w, http.StatusOK, map[string]any{
			"current_user_url": "https://example.invalid/api/v3/user",
		})
		return
	}

	switch segments[0] {
	case "meta":
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"installed_version": "1.0.0"})
		return
	case "user":
		restUser(w, r, store, segments[1:])
		return
	case "repos":
		restRepos(w, r, store, segments[1:])
		return
	}
	writeErr(w, http.StatusNotFound, "Not Found")
}

// splitPathSegments splits a path on "/". It scans by index with one
// pre-sized allocation instead of strings.Trim + strings.Split.
func splitPathSegments(path string) []string {
	start := 0
	for start < len(path) && path[start] == '/' {
		start++
	}
	end := len(path)
	for end > start && path[end-1] == '/' {
		end--
	}
	if start >= end {
		return nil
	}
	n := 1
	for i := start; i < end; i++ {
		if path[i] == '/' {
			n++
		}
	}
	out := make([]string, 0, n)
	segStart := start
	for i := start; i <= end; i++ {
		if i == end || path[i] == '/' {
			out = append(out, path[segStart:i])
			segStart = i + 1
		}
	}
	return out
}

// restUser handles /api/v3/user and /api/v3/user/repos.
func restUser(w http.ResponseWriter, r *http.Request, store *Store, rest []string) {
	if len(rest) == 0 {
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"login": ViewerLogin,
			"id":    1000,
			"name":  viewerName,
			"email": viewerEmail,
			"type":  "User",
		})
		return
	}
	if rest[0] != "repos" || len(rest) > 1 {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		q := r.URL.Query()
		perPage := queryInt(q.Get("per_page"), 30)
		page := queryInt(q.Get("page"), 1)
		if perPage < 1 {
			perPage = 30
		}
		if page < 1 {
			page = 1
		}
		repos := store.ListRepos(ViewerLogin)
		start := (page - 1) * perPage
		if start > len(repos) {
			start = len(repos)
		}
		end := start + perPage
		if end > len(repos) {
			end = len(repos)
		}
		list := make([]any, 0, end-start)
		for _, repo := range repos[start:end] {
			list = append(list, repoJSON(repo))
		}
		if end < len(repos) {
			scheme := "http"
			if r.TLS != nil {
				scheme = "https"
			}
			next := scheme + "://" + r.Host + "/api/v3/user/repos?per_page=" +
				strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page+1)
			w.Header().Set("Link", "<"+next+">; rel=\"next\"")
		}
		writeJSON(w, http.StatusOK, list)
	case http.MethodPost:
		body, ok := readJSONBody(w, r)
		if !ok {
			return
		}
		name := bodyString(body, "name")
		if name == "" {
			writeErr(w, http.StatusUnprocessableEntity, "name is required")
			return
		}
		repo := store.CreateRepo(ViewerLogin, name, bodyString(body, "description"), bodyBool(body, "private", false))
		if repo == nil {
			writeErr(w, http.StatusUnprocessableEntity, "Repository already exists")
			return
		}
		writeJSON(w, http.StatusCreated, repoJSON(repo))
	default:
		writeErr(w, http.StatusNotFound, "Not Found")
	}
}

func queryInt(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// restRepos handles /api/v3/repos/{owner}/{repo}[/...].
func restRepos(w http.ResponseWriter, r *http.Request, store *Store, rest []string) {
	if len(rest) < 2 {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	owner, name := rest[0], rest[1]
	tail := rest[2:]

	if len(tail) == 0 {
		repo := lookupRepo(w, store, owner, name)
		if repo == nil {
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, repoJSON(repo))
		case http.MethodPatch:
			body, ok := readJSONBody(w, r)
			if !ok {
				return
			}
			repo.mu.Lock()
			if v, ok := body["description"]; ok {
				if s, ok := v.(string); ok {
					repo.Description = s
				}
			}
			repo.mu.Unlock()
			writeJSON(w, http.StatusOK, repoJSON(repo))
		case http.MethodDelete:
			store.DeleteRepo(owner, name)
			issueCachePurgeRepo(repo)
			writeNoContent(w)
		default:
			writeErr(w, http.StatusNotFound, "Not Found")
		}
		return
	}

	switch tail[0] {
	case "branches":
		restBranches(w, r, store, owner, name, tail[1:])
	case "commits":
		restCommits(w, r, store, owner, name)
	case "contents":
		restContents(w, r, store, owner, name, tail[1:])
	case "labels":
		restLabels(w, r, store, owner, name)
	case "issues":
		restIssues(w, r, store, owner, name, tail[1:])
	case "pulls":
		restPulls(w, r, store, owner, name, tail[1:])
	case "git":
		restGitRefs(w, r, store, owner, name, tail[1:])
	default:
		writeErr(w, http.StatusNotFound, "Not Found")
	}
}

// restBranches handles branches listing and single-branch reads.
func restBranches(w http.ResponseWriter, r *http.Request, store *Store, owner, name string, tail []string) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	repo := lookupRepo(w, store, owner, name)
	if repo == nil {
		return
	}
	if len(tail) == 0 {
		refs := repo.ListRefs()
		names := make([]string, 0, len(refs))
		for ref := range refs {
			if strings.HasPrefix(ref, "refs/heads/") {
				names = append(names, strings.TrimPrefix(ref, "refs/heads/"))
			}
		}
		sortStrings(names)
		list := make([]any, 0, len(names))
		for _, branch := range names {
			sha, _ := repo.GetRef("refs/heads/" + branch)
			list = append(list, map[string]any{
				"name":      branch,
				"commit":    map[string]any{"sha": sha},
				"protected": false,
			})
		}
		writeJSON(w, http.StatusOK, list)
		return
	}
	if len(tail) == 1 {
		sha, ok := repo.GetRef("refs/heads/" + tail[0])
		if !ok {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"name":      tail[0],
			"commit":    map[string]any{"sha": sha},
			"protected": false,
		})
		return
	}
	writeErr(w, http.StatusNotFound, "Not Found")
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// restCommits lists commits on a branch (default branch when ?sha is absent).
func restCommits(w http.ResponseWriter, r *http.Request, store *Store, owner, name string) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	repo := lookupRepo(w, store, owner, name)
	if repo == nil {
		return
	}
	sha := r.URL.Query().Get("sha")
	var tip string
	if sha == "" {
		tip, _ = repo.GetRef("refs/heads/" + repo.DefaultBranch)
	} else if len(sha) == 40 {
		tip = sha
	} else {
		tip, _ = repo.GetRef("refs/heads/" + sha)
	}
	list := make([]any, 0)
	seen := make(map[string]bool)
	for cur := tip; cur != "" && !seen[cur]; {
		seen[cur] = true
		obj := repo.GetObject(cur)
		if obj == nil || obj.Type != "commit" {
			break
		}
		c, err := parseCommit(obj.Data)
		if err != nil {
			break
		}
		parents := make([]any, 0, len(c.Parents))
		for _, p := range c.Parents {
			parents = append(parents, map[string]any{"sha": p})
		}
		author, committer := splitIdent(c.Author), splitIdent(c.Committer)
		list = append(list, map[string]any{
			"sha": cur,
			"commit": map[string]any{
				"message":   c.Message,
				"author":    author,
				"committer": committer,
			},
			"parents": parents,
		})
		if len(c.Parents) == 0 {
			break
		}
		cur = c.Parents[0]
	}
	writeJSON(w, http.StatusOK, list)
}

// splitIdent turns "Name <email> epoch tz" into a GitHub author object.
func splitIdent(ident string) map[string]any {
	name := ident
	email := ""
	if lt := strings.Index(ident, "<"); lt >= 0 {
		name = strings.TrimSpace(ident[:lt])
		rest := ident[lt+1:]
		if gt := strings.Index(rest, ">"); gt >= 0 {
			email = rest[:gt]
		}
	}
	return map[string]any{"name": name, "email": email, "date": FixedTimestamp}
}

// branchTip resolves a branch name (or "" for the default branch) to its tip.
func branchTip(repo *Repo, branch string) (string, bool) {
	if branch == "" {
		branch = repo.DefaultBranch
	}
	return repo.GetRef("refs/heads/" + branch)
}

// restContents handles GET/PUT/DELETE on /contents/{path}.
func restContents(w http.ResponseWriter, r *http.Request, store *Store, owner, name string, tail []string) {
	repo := lookupRepo(w, store, owner, name)
	if repo == nil {
		return
	}
	path := strings.Join(tail, "/")
	switch r.Method {
	case http.MethodGet:
		restContentsGet(w, r, repo, path)
	case http.MethodPut:
		restContentsPut(w, r, repo, path)
	case http.MethodDelete:
		restContentsDelete(w, r, repo, path)
	default:
		writeErr(w, http.StatusNotFound, "Not Found")
	}
}

func restContentsGet(w http.ResponseWriter, r *http.Request, repo *Repo, path string) {
	branch := r.URL.Query().Get("ref")
	tip, ok := branchTip(repo, branch)
	if !ok {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	treeOID := repo.commitTree(tip)
	if path == "" {
		writeJSON(w, http.StatusOK, dirJSON(repo, treeOID, ""))
		return
	}
	if blobOID, found := repo.blobAtPath(treeOID, path); found {
		obj := repo.GetObject(blobOID)
		if obj == nil {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"type":     "file",
			"encoding": "base64",
			"size":     len(obj.Data),
			"name":     baseName(path),
			"path":     path,
			"content":  base64.StdEncoding.EncodeToString(obj.Data),
			"sha":      blobOID,
		})
		return
	}
	if entries := repo.dirEntries(treeOID, path); entries != nil {
		writeJSON(w, http.StatusOK, dirJSON(repo, treeOID, path))
		return
	}
	writeErr(w, http.StatusNotFound, "Not Found")
}

func dirJSON(repo *Repo, treeOID, path string) []any {
	entries := repo.dirEntries(treeOID, path)
	out := make([]any, 0, len(entries))
	for _, e := range entries {
		typ := "file"
		if e.Mode == "40000" {
			typ = "dir"
		}
		p := e.Name
		if path != "" {
			p = path + "/" + e.Name
		}
		out = append(out, map[string]any{
			"type": typ,
			"name": e.Name,
			"path": p,
			"sha":  e.OID,
		})
	}
	return out
}

func baseName(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

func restContentsPut(w http.ResponseWriter, r *http.Request, repo *Repo, path string) {
	body, ok := readJSONBody(w, r)
	if !ok {
		return
	}
	if path == "" {
		writeErr(w, http.StatusUnprocessableEntity, "path is required")
		return
	}
	contentB64 := bodyString(body, "content")
	raw, err := base64.StdEncoding.DecodeString(strings.Map(func(c rune) rune {
		if c == '\r' || c == '\n' || c == '\t' || c == ' ' {
			return -1
		}
		return c
	}, contentB64))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "content is not valid base64")
		return
	}
	branch := bodyString(body, "branch")
	tip, hasTip := branchTip(repo, branch)
	if branch == "" {
		branch = repo.DefaultBranch
	}
	currentTree := ""
	if hasTip {
		currentTree = repo.commitTree(tip)
	}
	existingSHA, exists := repo.blobAtPath(currentTree, path)
	if sha := bodyString(body, "sha"); sha != "" {
		if !exists || !strings.EqualFold(sha, existingSHA) {
			writeErr(w, http.StatusConflict, "sha does not match the current blob")
			return
		}
	}
	blobOID := repo.PutObject("blob", raw)
	newTree := repo.setPathInTree(currentTree, path, blobOID)
	if newTree == "" {
		newTree = emptyTreeOID
	}
	message := bodyString(body, "message")
	if message == "" {
		message = "Update " + path
	}
	var parents []string
	if hasTip {
		parents = []string{tip}
	}
	commitData := buildCommit(newTree, parents, viewerIdent(), viewerIdent(), message)
	commitOID := repo.PutObject("commit", commitData)
	repo.SetRef("refs/heads/"+branch, commitOID)
	status := http.StatusCreated
	if exists {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{
		"content": map[string]any{
			"name":     baseName(path),
			"path":     path,
			"sha":      blobOID,
			"size":     len(raw),
			"encoding": "base64",
			"type":     "file",
		},
		"commit": map[string]any{"sha": commitOID},
	})
}

func restContentsDelete(w http.ResponseWriter, r *http.Request, repo *Repo, path string) {
	body, ok := readJSONBody(w, r)
	if !ok {
		return
	}
	if path == "" {
		writeErr(w, http.StatusUnprocessableEntity, "path is required")
		return
	}
	branch := bodyString(body, "branch")
	tip, hasTip := branchTip(repo, branch)
	if !hasTip {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if branch == "" {
		branch = repo.DefaultBranch
	}
	currentTree := repo.commitTree(tip)
	existingSHA, exists := repo.blobAtPath(currentTree, path)
	if !exists {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if sha := bodyString(body, "sha"); sha != "" && !strings.EqualFold(sha, existingSHA) {
		writeErr(w, http.StatusConflict, "sha does not match the current blob")
		return
	}
	newTree := repo.setPathInTree(currentTree, path, "")
	if newTree == "" {
		newTree = emptyTreeOID
	}
	message := bodyString(body, "message")
	if message == "" {
		message = "Delete " + path
	}
	commitData := buildCommit(newTree, []string{tip}, viewerIdent(), viewerIdent(), message)
	commitOID := repo.PutObject("commit", commitData)
	repo.SetRef("refs/heads/"+branch, commitOID)
	writeJSON(w, http.StatusOK, map[string]any{
		"commit": map[string]any{"sha": commitOID},
	})
}

// restLabels handles POST/GET labels.
func restLabels(w http.ResponseWriter, r *http.Request, store *Store, owner, name string) {
	repo := lookupRepo(w, store, owner, name)
	if repo == nil {
		return
	}
	switch r.Method {
	case http.MethodPost:
		body, ok := readJSONBody(w, r)
		if !ok {
			return
		}
		labelName := bodyString(body, "name")
		if labelName == "" {
			writeErr(w, http.StatusUnprocessableEntity, "name is required")
			return
		}
		color := bodyString(body, "color")
		if color == "" {
			color = "ededed"
		}
		label := repo.GetOrCreateLabel(store, labelName, color)
		writeJSON(w, http.StatusCreated, labelJSON(label))
	case http.MethodGet:
		repo.mu.RLock()
		names := make([]string, 0, len(repo.labels))
		for n := range repo.labels {
			names = append(names, n)
		}
		repo.mu.RUnlock()
		sortStrings(names)
		list := make([]any, 0, len(names))
		for _, n := range names {
			if l := repo.labels[n]; l != nil {
				list = append(list, labelJSON(l))
			}
		}
		writeJSON(w, http.StatusOK, list)
	default:
		writeErr(w, http.StatusNotFound, "Not Found")
	}
}

// parseIssuePath extracts owner, repo and (when present) the issue number
// from a path that starts with "/api/v3/repos/". It reports ok=false unless
// the path is exactly an /issues route: POST .../issues (isCreate) or
// GET .../issues/{positive number}. Everything else — /issues/{n}/comments,
// PATCH, non-numeric numbers, trailing slashes — falls through to
// restHandler, so behavior for those routes is unchanged. All results are
// substrings of path: no allocation.
func parseIssuePath(path string) (owner, name string, number int, isCreate bool, ok bool) {
	const prefix = "/api/v3/repos/"
	if len(path) < len(prefix) || path[:len(prefix)] != prefix {
		return "", "", 0, false, false
	}
	rest := path[len(prefix):]
	i := strings.IndexByte(rest, '/')
	if i <= 0 {
		return "", "", 0, false, false
	}
	owner = rest[:i]
	rest = rest[i+1:]
	j := strings.IndexByte(rest, '/')
	if j <= 0 {
		return "", "", 0, false, false
	}
	name = rest[:j]
	rest = rest[j+1:]
	if rest == "issues" {
		return owner, name, 0, true, true
	}
	if !strings.HasPrefix(rest, "issues/") {
		return "", "", 0, false, false
	}
	tail := rest[len("issues/"):]
	if strings.IndexByte(tail, '/') >= 0 {
		return "", "", 0, false, false
	}
	n, err := strconv.Atoi(tail)
	if err != nil || n < 1 {
		return "", "", 0, false, false
	}
	return owner, name, n, false, true
}

// serveIssueGet answers GET /issues/{n} from the response cache when valid.
func serveIssueGet(w http.ResponseWriter, repo *Repo, issue *Issue) {
	if data, ok := issueCacheGet(repo, issue); ok {
		writeRawJSON(w, http.StatusOK, data)
	} else if data := buildAndCacheIssueJSON(repo, issue); data != nil {
		writeRawJSON(w, http.StatusOK, data)
	} else {
		writeJSON(w, http.StatusOK, fastIssueJSON(repo, issue))
	}
}

// restIssueGetFast is the dispatcher's hot path for GET one issue: auth,
// repo lookup, issue lookup, cached render. It is the read workload's path.
func restIssueGetFast(w http.ResponseWriter, r *http.Request, store *Store, owner, name string, number int) {
	if !checkAuth(w, r) {
		return
	}
	repo := lookupRepo(w, store, owner, name)
	if repo == nil {
		return
	}
	issue := repo.GetIssue(number)
	if issue == nil {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	serveIssueGet(w, repo, issue)
}

// restIssueCreateFast is the dispatcher's hot path for POST create issue.
// It is the write workload's path.
func restIssueCreateFast(w http.ResponseWriter, r *http.Request, store *Store, owner, name string) {
	if !checkAuth(w, r) {
		return
	}
	repo := lookupRepo(w, store, owner, name)
	if repo == nil {
		return
	}
	restIssueCreate(w, r, store, repo)
}

// restIssues handles /issues, /issues/{n}, /issues/{n}/comments.
func restIssues(w http.ResponseWriter, r *http.Request, store *Store, owner, name string, tail []string) {
	repo := lookupRepo(w, store, owner, name)
	if repo == nil {
		return
	}
	if len(tail) == 0 {
		switch r.Method {
		case http.MethodPost:
			restIssueCreate(w, r, store, repo)
		case http.MethodGet:
			restIssueList(w, r, repo)
		default:
			writeErr(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	number, err := strconv.Atoi(tail[0])
	if err != nil || number < 1 {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if len(tail) == 1 {
		issue := repo.GetIssue(number)
		if issue == nil {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		switch r.Method {
		case http.MethodGet:
			serveIssueGet(w, repo, issue)
		case http.MethodPatch:
			body, ok := readJSONBody(w, r)
			if !ok {
				return
			}
			if state := bodyString(body, "state"); state == "open" || state == "closed" {
				repo.SetIssueState(issue, state)
			}
			if title := bodyString(body, "title"); title != "" {
				repo.mu.Lock()
				issue.Title = title
				repo.mu.Unlock()
			}
			if b, ok := body["body"]; ok {
				if s, ok := b.(string); ok {
					repo.mu.Lock()
					issue.Body = s
					repo.mu.Unlock()
				}
			}
			if data := buildAndCacheIssueJSON(repo, issue); data != nil {
				writeRawJSON(w, http.StatusOK, data)
			} else {
				writeJSON(w, http.StatusOK, fastIssueJSON(repo, issue))
			}
		default:
			writeErr(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	if len(tail) == 2 && tail[1] == "comments" {
		issue := repo.GetIssue(number)
		if issue == nil {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		switch r.Method {
		case http.MethodPost:
			body, ok := readJSONBody(w, r)
			if !ok {
				return
			}
			comment := repo.AddComment(store, issue, bodyString(body, "body"))
			issueCacheInvalidate(repo, number)
			writeJSON(w, http.StatusCreated, map[string]any{
				"id":         comment.ID,
				"body":       comment.Body,
				"user":       map[string]any{"login": ViewerLogin, "id": 1000, "type": "User"},
				"created_at": FixedTimestamp,
				"updated_at": FixedTimestamp,
			})
		case http.MethodGet:
			repo.mu.RLock()
			comments := append([]*Comment(nil), issue.Comments...)
			repo.mu.RUnlock()
			list := make([]any, 0, len(comments))
			for _, c := range comments {
				list = append(list, map[string]any{
					"id":         c.ID,
					"body":       c.Body,
					"user":       map[string]any{"login": ViewerLogin, "id": 1000, "type": "User"},
					"created_at": FixedTimestamp,
					"updated_at": FixedTimestamp,
				})
			}
			writeJSON(w, http.StatusOK, list)
		default:
			writeErr(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	writeErr(w, http.StatusNotFound, "Not Found")
}

// issueBodyPool reuses the read buffer for POST /issues bodies. The pooled
// bytes never escape restIssueCreate: the scanner copies title/body into
// strings, the renderer copies them into fresh wire bytes, the slow path's
// json.Unmarshal copies every string it keeps, and the buffer is returned
// to the pool only after all of that (defer). Do not retain data or any
// slice of it past the create call.
var issueBodyPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

func restIssueCreate(w http.ResponseWriter, r *http.Request, store *Store, repo *Repo) {
	buf := issueBodyPool.Get().(*bytes.Buffer)
	defer issueBodyPool.Put(buf)
	buf.Reset()
	if _, err := buf.ReadFrom(io.LimitReader(r.Body, 1<<20)); err != nil {
		writeErr(w, http.StatusBadRequest, "Bad Request")
		return
	}
	data := buf.Bytes()
	// Fast path: flat object, string title/body, no labels (or an empty/null
	// labels array) — the shape the write workload sends. Anything unusual
	// falls through to the full decoder below, which stays authoritative.
	if title, body, ok := scanIssueCreate(data); ok {
		if title == "" {
			writeErr(w, http.StatusUnprocessableEntity, "title is required")
			return
		}
		issue := repo.CreateIssue(store, title, body, nil)
		if wire, ok := renderIssueCreateWire(issue.ID, issue.Number, title, body); ok {
			issueCacheStoreRaw(repo, issue.Number, wire, title, body)
			writeRawJSON(w, http.StatusCreated, wire)
			return
		}
		if wire := buildAndCacheIssueJSON(repo, issue); wire != nil {
			writeRawJSON(w, http.StatusCreated, wire)
			return
		}
		writeJSON(w, http.StatusCreated, fastIssueJSON(repo, issue))
		return
	}
	var in issueCreateIn
	if len(data) > 0 {
		if err := json.Unmarshal(data, &in); err != nil {
			writeErr(w, http.StatusBadRequest, "Problems parsing JSON")
			return
		}
	}
	if in.Title == "" {
		writeErr(w, http.StatusUnprocessableEntity, "title is required")
		return
	}
	labels := parseLabelNames(in.Labels)
	for _, name := range labels {
		if !repo.HasLabel(name) {
			writeErr(w, http.StatusUnprocessableEntity, "label "+name+" does not exist")
			return
		}
	}
	issue := repo.CreateIssue(store, in.Title, in.Body, labels)
	if wire := buildAndCacheIssueJSON(repo, issue); wire != nil {
		writeRawJSON(w, http.StatusCreated, wire)
		return
	}
	writeJSON(w, http.StatusCreated, fastIssueJSON(repo, issue))
}

// ---------------------------------------------------------------------------
// POST /issues fast path: hand-rolled JSON scanning and response rendering.
//
// scanIssueCreate extracts the top-level "title" and "body" strings without
// encoding/json. It is deliberately strict: any malformed JSON, any
// non-string title/body, any non-empty labels array, or any escaped key
// returns ok=false and the caller uses the full decoder, so behavior for
// every unusual body is exactly the old behavior. Duplicate keys keep the
// last value, matching encoding/json.
func scanIssueCreate(data []byte) (title, body string, ok bool) {
	if len(data) == 0 {
		return "", "", true
	}
	p := &jsonScanner{data: data}
	if !p.ws() || p.data[p.i] != '{' {
		return "", "", false
	}
	p.i++
	if !p.ws() {
		return "", "", false
	}
	if p.data[p.i] == '}' {
		p.i++
		return p.finish(title, body)
	}
	for {
		if p.data[p.i] != '"' {
			return "", "", false
		}
		start, end, hasEscape, ok := p.stringToken()
		if !ok || hasEscape {
			// Escaped keys are rare; let the full decoder handle them.
			return "", "", false
		}
		key := string(p.data[start+1 : end-1])
		if !p.ws() || p.data[p.i] != ':' {
			return "", "", false
		}
		p.i++
		if !p.ws() {
			return "", "", false
		}
		switch key {
		case "title", "body":
			s, ok := p.stringOrNull()
			if !ok {
				return "", "", false
			}
			if key == "title" {
				title = s
			} else {
				body = s
			}
		case "labels":
			if !p.labelsEmptyOrNull() {
				return "", "", false
			}
		default:
			if !p.skipValue() {
				return "", "", false
			}
		}
		if !p.ws() {
			return "", "", false
		}
		switch p.data[p.i] {
		case ',':
			p.i++
			if !p.ws() {
				return "", "", false
			}
		case '}':
			p.i++
			return p.finish(title, body)
		default:
			return "", "", false
		}
	}
}

func (p *jsonScanner) finish(title, body string) (string, string, bool) {
	p.ws()
	if !p.eof() {
		return "", "", false
	}
	return title, body, true
}

// jsonScanner is a strict, allocation-light JSON reader for the create-issue
// fast path. It validates as it scans; any deviation bails to the slow path.
type jsonScanner struct {
	data []byte
	i    int
}

func (p *jsonScanner) eof() bool { return p.i >= len(p.data) }

// ws skips JSON whitespace; it reports false at end of input.
func (p *jsonScanner) ws() bool {
	for p.i < len(p.data) {
		switch p.data[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return true
		}
	}
	return false
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// stringToken parses a JSON string starting at the opening quote. It returns
// the token bounds (including quotes), whether it contains a backslash
// escape, and whether the token is well-formed.
func (p *jsonScanner) stringToken() (start, end int, hasEscape bool, ok bool) {
	start = p.i
	p.i++
	for {
		if p.eof() {
			return 0, 0, false, false
		}
		c := p.data[p.i]
		if c == '"' {
			p.i++
			return start, p.i, hasEscape, true
		}
		if c == '\\' {
			hasEscape = true
			p.i++
			if p.eof() {
				return 0, 0, false, false
			}
			switch p.data[p.i] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				p.i++
			case 'u':
				p.i++
				for k := 0; k < 4; k++ {
					if p.eof() || !isHex(p.data[p.i]) {
						return 0, 0, false, false
					}
					p.i++
				}
			default:
				return 0, 0, false, false
			}
			continue
		}
		if c < 0x20 {
			return 0, 0, false, false
		}
		p.i++
	}
}

// stringOrNull parses a string value (decoding escapes when present) or a
// JSON null (which decodes to ""). Anything else is rejected.
func (p *jsonScanner) stringOrNull() (string, bool) {
	if p.eof() {
		return "", false
	}
	if p.data[p.i] == 'n' {
		if !p.literal("null") {
			return "", false
		}
		return "", true
	}
	if p.data[p.i] != '"' {
		return "", false
	}
	start, end, hasEscape, ok := p.stringToken()
	if !ok {
		return "", false
	}
	if hasEscape {
		var s string
		if err := json.Unmarshal(p.data[start:end], &s); err != nil {
			return "", false
		}
		return s, true
	}
	raw := string(p.data[start+1 : end-1])
	if !utf8.ValidString(raw) {
		// Invalid UTF-8 would be replaced by encoding/json; slow path.
		return "", false
	}
	return raw, true
}

// labelsEmptyOrNull accepts a null or an empty labels array; a non-empty
// array (or anything else) is rejected so the slow path validates it.
func (p *jsonScanner) labelsEmptyOrNull() bool {
	if p.eof() {
		return false
	}
	if p.data[p.i] == 'n' {
		return p.literal("null")
	}
	if p.data[p.i] != '[' {
		return false
	}
	p.i++
	if !p.ws() {
		return false
	}
	if p.data[p.i] != ']' {
		return false
	}
	p.i++
	return true
}

func (p *jsonScanner) literal(s string) bool {
	if p.i+len(s) > len(p.data) {
		return false
	}
	if string(p.data[p.i:p.i+len(s)]) != s {
		return false
	}
	p.i += len(s)
	return true
}

// skipValue strictly skips one JSON value of any shape.
func (p *jsonScanner) skipValue() bool {
	if p.eof() {
		return false
	}
	switch c := p.data[p.i]; {
	case c == '"':
		_, _, _, ok := p.stringToken()
		return ok
	case c == '{':
		p.i++
		if !p.ws() {
			return false
		}
		if p.data[p.i] == '}' {
			p.i++
			return true
		}
		for {
			if p.eof() || p.data[p.i] != '"' {
				return false
			}
			if _, _, _, ok := p.stringToken(); !ok {
				return false
			}
			if !p.ws() || p.data[p.i] != ':' {
				return false
			}
			p.i++
			if !p.ws() || !p.skipValue() {
				return false
			}
			if !p.ws() {
				return false
			}
			switch p.data[p.i] {
			case ',':
				p.i++
				if !p.ws() {
					return false
				}
			case '}':
				p.i++
				return true
			default:
				return false
			}
		}
	case c == '[':
		p.i++
		if !p.ws() {
			return false
		}
		if p.data[p.i] == ']' {
			p.i++
			return true
		}
		for {
			if !p.skipValue() {
				return false
			}
			if !p.ws() {
				return false
			}
			switch p.data[p.i] {
			case ',':
				p.i++
				if !p.ws() {
					return false
				}
			case ']':
				p.i++
				return true
			default:
				return false
			}
		}
	case c == 't':
		return p.literal("true")
	case c == 'f':
		return p.literal("false")
	case c == 'n':
		return p.literal("null")
	case c == '-' || c >= '0' && c <= '9':
		return p.skipNumber()
	default:
		return false
	}
}

// skipNumber strictly skips one JSON number.
func (p *jsonScanner) skipNumber() bool {
	if p.eof() {
		return false
	}
	if p.data[p.i] == '-' {
		p.i++
		if p.eof() {
			return false
		}
	}
	if p.data[p.i] == '0' {
		p.i++
	} else if p.data[p.i] >= '1' && p.data[p.i] <= '9' {
		for !p.eof() && p.data[p.i] >= '0' && p.data[p.i] <= '9' {
			p.i++
		}
	} else {
		return false
	}
	if !p.eof() && p.data[p.i] == '.' {
		p.i++
		if p.eof() || p.data[p.i] < '0' || p.data[p.i] > '9' {
			return false
		}
		for !p.eof() && p.data[p.i] >= '0' && p.data[p.i] <= '9' {
			p.i++
		}
	}
	if !p.eof() && (p.data[p.i] == 'e' || p.data[p.i] == 'E') {
		p.i++
		if !p.eof() && (p.data[p.i] == '+' || p.data[p.i] == '-') {
			p.i++
		}
		if p.eof() || p.data[p.i] < '0' || p.data[p.i] > '9' {
			return false
		}
		for !p.eof() && p.data[p.i] >= '0' && p.data[p.i] <= '9' {
			p.i++
		}
	}
	return true
}

// jsonSafeString reports whether s can be copied verbatim into a JSON string
// the way encoding/json would render it: no byte needing an escape
// (control, quote, backslash, or the HTML-escaped < > &), no U+2028/U+2029,
// and valid UTF-8.
func jsonSafeString(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	if strings.Contains(s, "\u2028") || strings.Contains(s, "\u2029") {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' {
			return false
		}
	}
	return true
}

// renderIssueCreateWire hand-builds the POST /issues response for a
// label-less issue, byte-identical to encoding/json's rendering of the
// issueOut struct (field order included). It returns ok=false when any
// string needs escaping, and the caller falls back to encoding/json.
func renderIssueCreateWire(id int64, number int, title, body string) ([]byte, bool) {
	if !jsonSafeString(title) || !jsonSafeString(body) || !jsonSafeString(ViewerLogin) {
		return nil, false
	}
	buf := make([]byte, 0, len(title)+len(body)+280)
	buf = append(buf, `{"id":`...)
	buf = strconv.AppendInt(buf, id, 10)
	buf = append(buf, `,"node_id":"I_`...)
	buf = strconv.AppendInt(buf, id, 10)
	buf = append(buf, `","number":`...)
	buf = strconv.AppendInt(buf, int64(number), 10)
	buf = append(buf, `,"title":"`...)
	buf = append(buf, title...)
	buf = append(buf, `","body":"`...)
	buf = append(buf, body...)
	buf = append(buf, `","state":"open","labels":[],"user":{"login":"`...)
	buf = append(buf, ViewerLogin...)
	buf = append(buf, `","id":1000,"type":"User"},"created_at":"`...)
	buf = append(buf, FixedTimestamp...)
	buf = append(buf, `","updated_at":"`...)
	buf = append(buf, FixedTimestamp...)
	buf = append(buf, `","comments":0}`...)
	buf = append(buf, '\n')
	return buf, true
}

func restIssueList(w http.ResponseWriter, r *http.Request, repo *Repo) {
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	direction := q.Get("direction")
	if direction == "" {
		direction = "desc"
	}
	perPage := queryInt(q.Get("per_page"), 30)
	page := queryInt(q.Get("page"), 1)
	if perPage < 1 {
		perPage = 30
	}
	if page < 1 {
		page = 1
	}
	issues := repo.ListIssues()
	var filtered []*Issue
	for _, issue := range issues {
		if state == "all" || issue.State == state {
			filtered = append(filtered, issue)
		}
	}
	if direction == "asc" {
		// ListIssues is already number-ascending.
	} else {
		for i, j := 0, len(filtered)-1; i < j; i, j = i+1, j-1 {
			filtered[i], filtered[j] = filtered[j], filtered[i]
		}
	}
	start := (page - 1) * perPage
	if start > len(filtered) {
		start = len(filtered)
	}
	end := start + perPage
	if end > len(filtered) {
		end = len(filtered)
	}
	list := make([]any, 0, end-start)
	for _, issue := range filtered[start:end] {
		list = append(list, fastIssueJSON(repo, issue))
	}
	writeJSON(w, http.StatusOK, list)
}

// restPulls handles /pulls, /pulls/{n}, /pulls/{n}/files, /pulls/{n}/merge.
func restPulls(w http.ResponseWriter, r *http.Request, store *Store, owner, name string, tail []string) {
	repo := lookupRepo(w, store, owner, name)
	if repo == nil {
		return
	}
	if len(tail) == 0 {
		switch r.Method {
		case http.MethodPost:
			body, ok := readJSONBody(w, r)
			if !ok {
				return
			}
			head := bodyString(body, "head")
			base := bodyString(body, "base")
			if head == "" || base == "" {
				writeErr(w, http.StatusUnprocessableEntity, "head and base are required")
				return
			}
			headSHA, _ := repo.GetRef("refs/heads/" + head)
			baseSHA, _ := repo.GetRef("refs/heads/" + base)
			var files []PRFile
			if headSHA != "" && baseSHA != "" {
				files = repo.diffTrees(repo.commitTree(baseSHA), repo.commitTree(headSHA))
			}
			pr := repo.CreatePull(store, bodyString(body, "title"), bodyString(body, "body"), head, base, headSHA, baseSHA, files)
			pr.Mergeable = true
			writeJSON(w, http.StatusCreated, pullJSON(pr, owner))
		case http.MethodGet:
			q := r.URL.Query()
			state := q.Get("state")
			if state == "" {
				state = "open"
			}
			perPage := queryInt(q.Get("per_page"), 30)
			page := queryInt(q.Get("page"), 1)
			if perPage < 1 {
				perPage = 30
			}
			if page < 1 {
				page = 1
			}
			pulls := repo.ListPulls()
			var filtered []*PullRequest
			for _, pr := range pulls {
				if state == "all" || pr.State == state {
					filtered = append(filtered, pr)
				}
			}
			start := (page - 1) * perPage
			if start > len(filtered) {
				start = len(filtered)
			}
			end := start + perPage
			if end > len(filtered) {
				end = len(filtered)
			}
			list := make([]any, 0, end-start)
			for _, pr := range filtered[start:end] {
				list = append(list, pullJSON(pr, owner))
			}
			writeJSON(w, http.StatusOK, list)
		default:
			writeErr(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	number, err := strconv.Atoi(tail[0])
	if err != nil || number < 1 {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if len(tail) == 1 {
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		pr := repo.GetPull(number)
		if pr == nil {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		writeJSON(w, http.StatusOK, pullJSON(pr, owner))
		return
	}
	if len(tail) == 2 && tail[1] == "files" {
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		pr := repo.GetPull(number)
		if pr == nil {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		list := make([]any, 0, len(pr.Files))
		for _, f := range pr.Files {
			list = append(list, map[string]any{
				"filename":  f.Path,
				"status":    f.Status,
				"additions": f.Additions,
				"deletions": f.Deletions,
				"changes":   f.Additions + f.Deletions,
			})
		}
		writeJSON(w, http.StatusOK, list)
		return
	}
	if len(tail) == 2 && tail[1] == "merge" {
		if r.Method != http.MethodPut {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		mergePull(w, r, store, repo, owner, number)
		return
	}
	writeErr(w, http.StatusNotFound, "Not Found")
}

// doMergePR performs a squash merge. It returns the merge commit sha, or ""
// plus the conflicting path when the merge conflicts. It is shared by the
// REST merge endpoint and the GraphQL mergePullRequest mutation.
func doMergePR(repo *Repo, pr *PullRequest, owner string) (string, string) {
	if pr.Merged {
		return pr.MergeSHA, ""
	}
	// Conflict: a file the PR changes was also changed on the base branch
	// since the PR was created.
	baseTip, hasBase := repo.GetRef("refs/heads/" + pr.Base)
	if !hasBase {
		return "", "base branch has no commits"
	}
	baseTreeAtCreation := repo.commitTree(pr.BaseSHA)
	currentBaseTree := repo.commitTree(baseTip)
	headTree := repo.commitTree(pr.HeadSHA)
	for _, f := range pr.Files {
		baseBlob, _ := repo.blobAtPath(baseTreeAtCreation, f.Path)
		currentBlob, _ := repo.blobAtPath(currentBaseTree, f.Path)
		headBlob, _ := repo.blobAtPath(headTree, f.Path)
		if currentBlob != baseBlob && headBlob != baseBlob {
			return "", f.Path
		}
	}
	// Squash: apply the PR's changed files onto the current base tree.
	newTree := currentBaseTree
	for _, f := range pr.Files {
		if f.Status == "removed" {
			newTree = repo.setPathInTree(newTree, f.Path, "")
		} else if blobOID, ok := repo.blobAtPath(headTree, f.Path); ok {
			newTree = repo.setPathInTree(newTree, f.Path, blobOID)
		}
	}
	if newTree == "" {
		newTree = emptyTreeOID
	}
	message := "Merge pull request #" + strconv.Itoa(pr.Number) + " from " + owner + "/" + pr.Head + "\n"
	commitData := buildCommit(newTree, []string{baseTip}, viewerIdent(), viewerIdent(), message)
	mergeSHA := repo.PutObject("commit", commitData)
	repo.SetRef("refs/heads/"+pr.Base, mergeSHA)
	repo.mu.Lock()
	pr.State = "closed"
	pr.Merged = true
	pr.MergeSHA = mergeSHA
	pr.MergedAt = FixedTimestamp
	repo.mu.Unlock()
	return mergeSHA, ""
}

// mergePull performs a squash merge of a pull request.
func mergePull(w http.ResponseWriter, r *http.Request, store *Store, repo *Repo, owner string, number int) {
	pr := repo.GetPull(number)
	if pr == nil {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if pr.Merged {
		writeJSON(w, http.StatusOK, map[string]any{
			"sha":     pr.MergeSHA,
			"merged":  true,
			"message": "Pull request successfully merged",
		})
		return
	}
	mergeSHA, conflict := doMergePR(repo, pr, owner)
	if conflict != "" {
		if conflict == "base branch has no commits" {
			writeErr(w, http.StatusConflict, conflict)
		} else {
			writeErr(w, http.StatusMethodNotAllowed, "merge conflict: "+conflict+" was changed on "+pr.Base)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sha":     mergeSHA,
		"merged":  true,
		"message": "Pull request successfully merged",
	})
}

// restGitRefs handles /git/refs (create + read).
func restGitRefs(w http.ResponseWriter, r *http.Request, store *Store, owner, name string, tail []string) {
	repo := lookupRepo(w, store, owner, name)
	if repo == nil {
		return
	}
	if len(tail) == 0 || tail[0] != "refs" {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	rest := tail[1:]
	switch r.Method {
	case http.MethodPost:
		if len(rest) != 0 {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		body, ok := readJSONBody(w, r)
		if !ok {
			return
		}
		ref := bodyString(body, "ref")
		sha := bodyString(body, "sha")
		if ref == "" || sha == "" {
			writeErr(w, http.StatusUnprocessableEntity, "ref and sha are required")
			return
		}
		if _, exists := repo.GetRef(ref); exists {
			writeErr(w, http.StatusUnprocessableEntity, "Reference already exists")
			return
		}
		if repo.GetObject(sha) == nil {
			writeErr(w, http.StatusUnprocessableEntity, "Object does not exist")
			return
		}
		repo.SetRef(ref, sha)
		writeJSON(w, http.StatusCreated, map[string]any{
			"ref":    ref,
			"object": map[string]any{"sha": sha},
		})
	case http.MethodGet:
		if len(rest) == 0 {
			refs := repo.ListRefs()
			names := make([]string, 0, len(refs))
			for n := range refs {
				names = append(names, n)
			}
			sortStrings(names)
			list := make([]any, 0, len(names))
			for _, n := range names {
				list = append(list, map[string]any{
					"ref":    n,
					"object": map[string]any{"sha": refs[n]},
				})
			}
			writeJSON(w, http.StatusOK, list)
			return
		}
		refName := strings.Join(rest, "/")
		sha, ok := repo.GetRef(refName)
		if !ok {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ref":    refName,
			"object": map[string]any{"sha": sha},
		})
	default:
		writeErr(w, http.StatusNotFound, "Not Found")
	}
}
