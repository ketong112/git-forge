// treeutil.go: git tree manipulation for the REST surfaces.
//
// Contents writes, pull-request diffs and merges all work by reading and
// rebuilding trees. Everything here is pure object-store work: parse a tree,
// walk it, rebuild it bottom-up, hash the new objects into the repo.
package main

import (
	"bytes"
	"encoding/hex"
	"sort"
	"strings"
)

// splitPath splits "a/b/c.txt" into ["a" "b" "c.txt"].
func splitPath(path string) []string {
	path = strings.Trim(path, "/")
	if path == "" {
		return nil
	}
	return strings.Split(path, "/")
}

// commitTree returns the tree oid of a commit object, "" when unknown.
func (r *Repo) commitTree(commitOID string) string {
	obj := r.GetObject(commitOID)
	if obj == nil || obj.Type != "commit" {
		return ""
	}
	c, err := parseCommit(obj.Data)
	if err != nil {
		return ""
	}
	return c.Tree
}

// commitParents returns the parent oids of a commit object.
func (r *Repo) commitParents(commitOID string) []string {
	obj := r.GetObject(commitOID)
	if obj == nil || obj.Type != "commit" {
		return nil
	}
	c, err := parseCommit(obj.Data)
	if err != nil {
		return nil
	}
	return c.Parents
}

// treeEntries returns the parsed entries of a tree object.
func (r *Repo) treeEntries(treeOID string) []TreeEntry {
	obj := r.GetObject(treeOID)
	if obj == nil || obj.Type != "tree" {
		return nil
	}
	entries, err := parseTree(obj.Data)
	if err != nil {
		return nil
	}
	return entries
}

// blobAtPath walks a tree to a file path and returns its blob oid.
func (r *Repo) blobAtPath(treeOID string, path string) (string, bool) {
	parts := splitPath(path)
	if len(parts) == 0 || treeOID == "" {
		return "", false
	}
	current := treeOID
	for i, part := range parts {
		entries := r.treeEntries(current)
		found := ""
		isDir := false
		for _, e := range entries {
			if e.Name == part {
				found = e.OID
				isDir = e.Mode == "40000"
				break
			}
		}
		if found == "" {
			return "", false
		}
		if i == len(parts)-1 {
			if isDir {
				return "", false
			}
			return found, true
		}
		if !isDir {
			return "", false
		}
		current = found
	}
	return "", false
}

// dirEntries lists the entries of a directory path inside a tree.
// It returns nil when the path is not a directory.
func (r *Repo) dirEntries(treeOID string, path string) []TreeEntry {
	parts := splitPath(path)
	current := treeOID
	if len(parts) == 0 {
		return r.treeEntries(current)
	}
	for _, part := range parts {
		entries := r.treeEntries(current)
		next := ""
		for _, e := range entries {
			if e.Name == part && e.Mode == "40000" {
				next = e.OID
				break
			}
		}
		if next == "" {
			return nil
		}
		current = next
	}
	return r.treeEntries(current)
}

// setPathInTree returns a new tree oid equal to treeOID with path set to
// blobOID (mode 100644), or removed when blobOID is "". Intermediate trees
// are created as needed; emptied directories are pruned. An empty treeOID
// means "start from the empty tree".
func (r *Repo) setPathInTree(treeOID, path, blobOID string) string {
	parts := splitPath(path)
	if len(parts) == 0 {
		return treeOID
	}
	return r.setPathRec(treeOID, parts, blobOID)
}

func (r *Repo) setPathRec(treeOID string, parts []string, blobOID string) string {
	entries := r.treeEntries(treeOID)
	// Map name -> entry for quick lookup, keeping git's sort order on write.
	byName := make(map[string]TreeEntry, len(entries)+1)
	for _, e := range entries {
		byName[e.Name] = e
	}
	name := parts[0]
	if len(parts) == 1 {
		if blobOID == "" {
			delete(byName, name)
		} else {
			byName[name] = TreeEntry{Mode: "100644", Name: name, OID: blobOID}
		}
	} else {
		childOID := ""
		if e, ok := byName[name]; ok && e.Mode == "40000" {
			childOID = e.OID
		}
		newChild := r.setPathRec(childOID, parts[1:], blobOID)
		if newChild == "" {
			delete(byName, name)
		} else {
			byName[name] = TreeEntry{Mode: "40000", Name: name, OID: newChild}
		}
	}
	if len(byName) == 0 {
		return ""
	}
	out := make([]TreeEntry, 0, len(byName))
	for _, e := range byName {
		out = append(out, e)
	}
	data := buildTree(out)
	if len(data) == 0 {
		return ""
	}
	return r.PutObject("tree", data)
}

// emptyTreeOID is the oid of the empty tree, computed once per process.
var emptyTreeOID = GitOID("tree", []byte{})

// flattenTree returns path -> blob oid for every file under a tree.
func (r *Repo) flattenTree(treeOID string) map[string]string {
	out := make(map[string]string)
	var walk func(treeOID, prefix string)
	walk = func(treeOID, prefix string) {
		for _, e := range r.treeEntries(treeOID) {
			if e.Mode == "40000" {
				walk(e.OID, prefix+e.Name+"/")
			} else {
				out[prefix+e.Name] = e.OID
			}
		}
	}
	if treeOID != "" {
		walk(treeOID, "")
	}
	return out
}

// diffTrees compares two trees and returns the changed files, sorted by path.
// Line counts come from the blob contents (added = lines in the new blob,
// deleted = lines in the old blob).
func (r *Repo) diffTrees(baseTree, headTree string) []PRFile {
	base := r.flattenTree(baseTree)
	head := r.flattenTree(headTree)
	paths := make(map[string]bool)
	for p := range base {
		paths[p] = true
	}
	for p := range head {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	var out []PRFile
	for _, p := range sorted {
		b, inBase := base[p]
		h, inHead := head[p]
		if inBase && inHead && b == h {
			continue
		}
		f := PRFile{Path: p}
		switch {
		case !inBase:
			f.Status = "added"
		case !inHead:
			f.Status = "removed"
		default:
			f.Status = "modified"
		}
		if inHead {
			if obj := r.GetObject(h); obj != nil {
				f.Additions = countLines(obj.Data)
			}
		}
		if inBase {
			if obj := r.GetObject(b); obj != nil {
				f.Deletions = countLines(obj.Data)
			}
		}
		out = append(out, f)
	}
	return out
}

// countLines counts "\n"-terminated lines (a trailing partial line counts).
func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := bytes.Count(data, []byte("\n"))
	if data[len(data)-1] != '\n' {
		n++
	}
	return n
}

// reachableObjects returns the set of oids reachable from the given commit
// tips (commits, their trees/blobs recursively, and tag targets), following
// parents. When maxDepth > 0, commit ancestry is cut at that depth (depth 1
// = only the tips themselves); trees/blobs of included commits are always
// fully included. haves lists commits whose own objects are excluded
// (shallow fetch); when deep is false the exclusion is the full reachable
// closure of the haves, when deep is true only the haves' own tree closure.
func (r *Repo) reachableObjects(tips []string, maxDepth int, haves []string, deep bool) map[string]*GitObject {
	// Exclude set.
	exclude := make(map[string]bool)
	if len(haves) > 0 {
		if deep {
			for _, h := range haves {
				// Own objects only: the commit, its tree closure.
				if obj := r.GetObject(h); obj != nil {
					exclude[h] = true
					if obj.Type == "commit" {
						if c, err := parseCommit(obj.Data); err == nil {
							r.markTreeClosure(c.Tree, exclude)
						}
					} else if obj.Type == "tree" {
						r.markTreeClosure(h, exclude)
					}
				}
			}
		} else {
			for _, h := range haves {
				r.markReachable(h, exclude)
			}
		}
	}
	out := make(map[string]*GitObject)
	// BFS over commits with depth.
	type item struct {
		oid   string
		depth int
	}
	seen := make(map[string]bool)
	queue := make([]item, 0, len(tips))
	for _, tip := range tips {
		if tip == "" || tip == zeroOID {
			continue
		}
		queue = append(queue, item{tip, 1})
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur.oid] {
			continue
		}
		seen[cur.oid] = true
		obj := r.GetObject(cur.oid)
		if obj == nil {
			continue
		}
		if !exclude[cur.oid] {
			out[cur.oid] = obj
		}
		switch obj.Type {
		case "commit":
			c, err := parseCommit(obj.Data)
			if err != nil {
				continue
			}
			if !exclude[c.Tree] {
				r.addTreeClosure(c.Tree, out, exclude)
			}
			if maxDepth <= 0 || cur.depth < maxDepth {
				for _, p := range c.Parents {
					queue = append(queue, item{p, cur.depth + 1})
				}
			}
		case "tag":
			// Annotated tag: follow its target.
			target := tagTarget(obj.Data)
			if target != "" && !seen[target] {
				queue = append(queue, item{target, cur.depth})
			}
		}
	}
	// Tips that are tags/blobs (not commits) still contribute their closure.
	for _, tip := range tips {
		if tip == "" || tip == zeroOID || seen[tip] {
			continue
		}
		if obj := r.GetObject(tip); obj != nil {
			if obj.Type == "tree" {
				r.addTreeClosure(tip, out, exclude)
			} else if !exclude[tip] {
				out[tip] = obj
			}
		}
	}
	return out
}

// markTreeClosure marks a tree and everything under it in the set.
func (r *Repo) markTreeClosure(treeOID string, set map[string]bool) {
	if treeOID == "" || set[treeOID] {
		return
	}
	set[treeOID] = true
	for _, e := range r.treeEntries(treeOID) {
		if e.Mode == "40000" {
			r.markTreeClosure(e.OID, set)
		} else {
			set[e.OID] = true
		}
	}
}

// addTreeClosure adds a tree and everything under it to out (minus exclude).
func (r *Repo) addTreeClosure(treeOID string, out map[string]*GitObject, exclude map[string]bool) {
	if treeOID == "" || exclude[treeOID] {
		return
	}
	if obj := r.GetObject(treeOID); obj != nil {
		out[treeOID] = obj
	}
	for _, e := range r.treeEntries(treeOID) {
		if exclude[e.OID] {
			continue
		}
		if e.Mode == "40000" {
			r.addTreeClosure(e.OID, out, exclude)
		} else if obj := r.GetObject(e.OID); obj != nil {
			out[e.OID] = obj
		}
	}
}

// markReachable marks a commit tip and its full closure in the set.
func (r *Repo) markReachable(tip string, set map[string]bool) {
	if tip == "" || tip == zeroOID || set[tip] {
		return
	}
	obj := r.GetObject(tip)
	if obj == nil {
		return
	}
	set[tip] = true
	switch obj.Type {
	case "commit":
		c, err := parseCommit(obj.Data)
		if err != nil {
			return
		}
		r.markTreeClosure(c.Tree, set)
		for _, p := range c.Parents {
			r.markReachable(p, set)
		}
	case "tag":
		if target := tagTarget(obj.Data); target != "" {
			r.markReachable(target, set)
		}
	case "tree":
		r.markTreeClosure(tip, set)
	}
}

// tagTarget parses the "object <oid>" line of a tag object.
func tagTarget(data []byte) string {
	for _, line := range splitLines(string(data)) {
		if strings.HasPrefix(line, "object ") {
			return strings.TrimSpace(line[len("object "):])
		}
		if line == "" {
			break
		}
	}
	return ""
}

// isDescendant reports whether newOID is a descendant of oldOID (or equal).
// It walks parents from newOID; missing objects stop the walk (not a descendant).
func (r *Repo) isDescendant(newOID, oldOID string) bool {
	if newOID == oldOID {
		return true
	}
	seen := make(map[string]bool)
	queue := []string{newOID}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if cur == oldOID {
			return true
		}
		for _, p := range r.commitParents(cur) {
			if p == oldOID {
				return true
			}
			queue = append(queue, p)
		}
	}
	return false
}

// hexOID decodes a 20-byte binary oid from a tree entry; kept here for the
// pack writer's use of raw oids.
func oidBytes(hexOID string) []byte {
	b, _ := hex.DecodeString(hexOID)
	return b
}

const zeroOID = "0000000000000000000000000000000000000000"
