// store.go: the forge's one in-memory store.
//
// Everything the four surfaces serve lives here. No clocks, no randomness, no
// network: ids are counters, timestamps are the fixed scenario epoch, and git
// object ids are content hashes. Two runs on the same inputs therefore hold the
// same state, which is what the replay comparison needs.
//
// Locking: one RWMutex guards the repository map (RLock for lookups, which is
// the read workload's path). Each repository carries its own RWMutex for its
// issues/labels/pulls/refs counters, and a separate RWMutex for its git object
// map, so concurrent clones of one repo never serialize on one lock.
package main

import (
	"crypto/sha1"
	"encoding/hex"
	"sync"
	"sync/atomic"
)

// FixedTimestamp is the timestamp every created object carries. It is the
// scenario's epoch (2025-01-01T00:00:00Z): deterministic, and it makes
// created_at ties sort by number, exactly as the GraphQL contract specifies.
const FixedTimestamp = "2025-01-01T00:00:00Z"

// FixedEpochSec is FixedTimestamp as a unix epoch (for git idents).
const FixedEpochSec = 1735689600

// GitObject is one git object: its type and its raw content bytes (without the
// "<type> <len>\0" header; the header is rebuilt when hashing or packing).
type GitObject struct {
	Type string // "blob" | "tree" | "commit" | "tag"
	Data []byte
}

// Comment is one comment on an issue.
type Comment struct {
	ID   int64
	Body string
}

// Issue is one issue in a repository.
type Issue struct {
	ID       int64
	Number   int
	Title    string
	Body     string
	State    string // "open" | "closed"
	Labels   []string
	Comments []*Comment
}

// Label is one label in a repository.
type Label struct {
	ID    int64
	Name  string
	Color string
}

// PullRequest is one pull request in a repository.
type PullRequest struct {
	ID        int64
	Number    int
	Title     string
	Body      string
	State     string // "open" | "closed"
	Head      string // head branch name
	Base      string // base branch name
	HeadSHA   string // head commit oid ("" when the branch has no commit yet)
	BaseSHA   string // base branch tip oid at creation time
	Merged    bool
	MergeSHA  string // squash commit oid, "" until merged
	MergedAt  string // "" until merged
	Mergeable bool
	Files     []PRFile // changed files, computed at creation
}

// PRFile is one changed file of a pull request.
type PRFile struct {
	Path      string
	Status    string // "added" | "modified" | "removed"
	Additions int
	Deletions int
}

// Repo is one repository: its git object store, its refs, and its REST state.
type Repo struct {
	ID            int64
	Owner         string
	Name          string
	Description   string
	Private       bool
	DefaultBranch string

	mu         sync.RWMutex // guards Issues, Labels, Pulls and their counters
	issues     map[int]*Issue
	labels     map[string]*Label
	pulls      map[int]*PullRequest
	issueSeq   int
	pullSeq    int
	commentSeq int64

	gitMu   sync.RWMutex // guards Objects and Refs
	Objects map[string]*GitObject
	Refs    map[string]string // full ref name ("refs/heads/main") -> commit oid
}

// Store is the whole forge: repositories by "owner/name".
type Store struct {
	mu      sync.RWMutex
	repos   map[string]*Repo
	repoSeq int64
	objSeq  int64 // global counter for REST-visible object ids (issues, labels, PRs)
}

// NewStore builds an empty store.
func NewStore() *Store {
	return &Store{repos: make(map[string]*Repo)}
}

// nextID returns a fresh global id for a REST-visible object.
func (s *Store) nextID() int64 {
	return atomic.AddInt64(&s.objSeq, 1)
}

// repoKey is the map key for a repository.
func repoKey(owner, name string) string { return owner + "/" + name }

// CreateRepo creates a repository owned by owner. It returns nil when the
// repository already exists.
func (s *Store) CreateRepo(owner, name, description string, private bool) *Repo {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := repoKey(owner, name)
	if _, exists := s.repos[key]; exists {
		return nil
	}
	s.repoSeq++
	repo := &Repo{
		ID:            s.repoSeq,
		Owner:         owner,
		Name:          name,
		Description:   description,
		Private:       private,
		DefaultBranch: "main",
		issues:        make(map[int]*Issue),
		labels:        make(map[string]*Label),
		pulls:         make(map[int]*PullRequest),
		Objects:       make(map[string]*GitObject),
		Refs:          make(map[string]string),
	}
	s.repos[key] = repo
	return repo
}

// GetRepo returns the repository, or nil when it does not exist.
func (s *Store) GetRepo(owner, name string) *Repo {
	s.mu.RLock()
	repo := s.repos[repoKey(owner, name)]
	s.mu.RUnlock()
	return repo
}

// DeleteRepo removes a repository. It reports whether one existed.
func (s *Store) DeleteRepo(owner, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := repoKey(owner, name)
	if _, exists := s.repos[key]; !exists {
		return false
	}
	delete(s.repos, key)
	return true
}

// ListRepos returns the repositories owned by owner, in creation order.
func (s *Store) ListRepos(owner string) []*Repo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Repo
	for _, repo := range s.repos {
		if repo.Owner == owner {
			out = append(out, repo)
		}
	}
	// Creation order is the id order (insertion sort; lists are short).
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].ID < out[j-1].ID; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// GitOID is the git object id of type+data: SHA-1("<type> <len>\0" + data).
func GitOID(typ string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(typ))
	h.Write([]byte(" "))
	h.Write([]byte(intToString(len(data))))
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func intToString(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// PutObject stores one git object and returns its oid.
func (r *Repo) PutObject(typ string, data []byte) string {
	oid := GitOID(typ, data)
	r.gitMu.Lock()
	if _, exists := r.Objects[oid]; !exists {
		r.Objects[oid] = &GitObject{Type: typ, Data: data}
	}
	r.gitMu.Unlock()
	return oid
}

// PutObjectPrecomputed stores an object whose oid is already known.
func (r *Repo) PutObjectPrecomputed(oid string, obj *GitObject) {
	r.gitMu.Lock()
	if _, exists := r.Objects[oid]; !exists {
		r.Objects[oid] = obj
	}
	r.gitMu.Unlock()
}

// GetObject returns one git object, or nil.
func (r *Repo) GetObject(oid string) *GitObject {
	r.gitMu.RLock()
	obj := r.Objects[oid]
	r.gitMu.RUnlock()
	return obj
}

// HasObject reports whether an object is stored.
func (r *Repo) HasObject(oid string) bool {
	r.gitMu.RLock()
	_, ok := r.Objects[oid]
	r.gitMu.RUnlock()
	return ok
}

// GetRef returns a ref's oid and whether it exists.
func (r *Repo) GetRef(name string) (string, bool) {
	r.gitMu.RLock()
	oid, ok := r.Refs[name]
	r.gitMu.RUnlock()
	return oid, ok
}

// SetRef sets a ref to an oid.
func (r *Repo) SetRef(name, oid string) {
	r.gitMu.Lock()
	r.Refs[name] = oid
	r.gitMu.Unlock()
}

// DeleteRef removes a ref. It reports whether one existed.
func (r *Repo) DeleteRef(name string) bool {
	r.gitMu.Lock()
	defer r.gitMu.Unlock()
	if _, ok := r.Refs[name]; !ok {
		return false
	}
	delete(r.Refs, name)
	return true
}

// ListRefs returns a copy of the ref map.
func (r *Repo) ListRefs() map[string]string {
	r.gitMu.RLock()
	defer r.gitMu.RUnlock()
	out := make(map[string]string, len(r.Refs))
	for name, oid := range r.Refs {
		out[name] = oid
	}
	return out
}

// --- issue / label / pull helpers (mu-guarded) ---

// CreateIssue appends an issue and returns it.
func (r *Repo) CreateIssue(s *Store, title, body string, labels []string) *Issue {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.issueSeq++
	issue := &Issue{
		ID:     s.nextID(),
		Number: r.issueSeq,
		Title:  title,
		Body:   body,
		State:  "open",
		Labels: append([]string(nil), labels...),
	}
	r.issues[issue.Number] = issue
	return issue
}

// GetIssue returns an issue by number, or nil.
func (r *Repo) GetIssue(number int) *Issue {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.issues[number]
}

// ListIssues returns the issues in number order.
func (r *Repo) ListIssues() []*Issue {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Issue, 0, len(r.issues))
	for n := 1; n <= r.issueSeq; n++ {
		if issue, ok := r.issues[n]; ok {
			out = append(out, issue)
		}
	}
	return out
}

// AddComment appends a comment to an issue and returns it.
func (r *Repo) AddComment(s *Store, issue *Issue, body string) *Comment {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commentSeq++
	comment := &Comment{ID: r.commentSeq, Body: body}
	issue.Comments = append(issue.Comments, comment)
	return comment
}

// GetOrCreateLabel returns the label, creating it when missing.
func (r *Repo) GetOrCreateLabel(s *Store, name, color string) *Label {
	r.mu.Lock()
	defer r.mu.Unlock()
	if label, ok := r.labels[name]; ok {
		return label
	}
	label := &Label{ID: s.nextID(), Name: name, Color: color}
	r.labels[name] = label
	return label
}

// HasLabel reports whether the label exists.
func (r *Repo) HasLabel(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.labels[name]
	return ok
}

// CreatePull appends a pull request and returns it.
func (r *Repo) CreatePull(s *Store, title, body, head, base, headSHA, baseSHA string, files []PRFile) *PullRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pullSeq++
	pr := &PullRequest{
		ID:      s.nextID(),
		Number:  r.pullSeq,
		Title:   title,
		Body:    body,
		State:   "open",
		Head:    head,
		Base:    base,
		HeadSHA: headSHA,
		BaseSHA: baseSHA,
		Files:   files,
	}
	r.pulls[pr.Number] = pr
	return pr
}

// GetPull returns a pull request by number, or nil.
func (r *Repo) GetPull(number int) *PullRequest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.pulls[number]
}

// ListPulls returns the pull requests in number order.
func (r *Repo) ListPulls() []*PullRequest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*PullRequest, 0, len(r.pulls))
	for n := 1; n <= r.pullSeq; n++ {
		if pr, ok := r.pulls[n]; ok {
			out = append(out, pr)
		}
	}
	return out
}

// SetIssueState changes an issue's state under the lock.
func (r *Repo) SetIssueState(issue *Issue, state string) {
	r.mu.Lock()
	issue.State = state
	r.mu.Unlock()
}
