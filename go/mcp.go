// mcp.go: MCP over streamable HTTP at POST /mcp (JSON-RPC 2.0).
//
// The suite speaks the modern 2026-07-28 revision (server/discover, no
// handshake) and falls back to the legacy initialize handshake only when
// discover is refused — so this server answers discover. Six tools share the
// store with REST: create_repo, get_repo, create_issue, list_issues,
// create_pull_request, merge_pull_request. tools/list is byte-for-byte
// deterministic (fixed struct order, no maps, no timestamps).
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// ---------------------------------------------------------------- JSON-RPC plumbing

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// writeRPCResult answers a JSON-RPC result with the request's id echoed.
func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeRPC(w, http.StatusOK, id, result, nil)
}

// writeRPCError answers a JSON-RPC error with the request's id echoed.
func writeRPCError(w http.ResponseWriter, status int, id json.RawMessage, code int, message string) {
	writeRPC(w, status, id, nil, &rpcError{Code: code, Message: message})
}

func writeRPC(w http.ResponseWriter, status int, id json.RawMessage, result any, rpcErr *rpcError) {
	var buf bytes.Buffer
	buf.WriteString(`{"jsonrpc":"2.0","id":`)
	if len(id) == 0 {
		buf.WriteString("null")
	} else {
		buf.Write(id)
	}
	if rpcErr != nil {
		data, _ := json.Marshal(rpcErr)
		buf.WriteString(`,"error":`)
		buf.Write(data)
	} else {
		data, _ := json.Marshal(result)
		buf.WriteString(`,"result":`)
		buf.Write(data)
	}
	buf.WriteString("}")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-GitHub-Request-Id", requestID())
	// No Content-Length: transport.go frames the response with an exact one
	// and drops any handler-set value.
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// ---------------------------------------------------------------- tools

// mcpToolSchema is the fixed, deterministically-marshalled tool description.
type mcpToolSchema struct {
	Type       string                     `json:"type"`
	Properties map[string]mcpToolProperty `json:"properties,omitempty"`
	Required   []string                   `json:"required,omitempty"`
}

type mcpToolProperty struct {
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
}

type mcpTool struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	InputSchema mcpToolSchema `json:"inputSchema"`
}

func strProp(desc string) mcpToolProperty { return mcpToolProperty{Type: "string", Description: desc} }
func boolProp(desc string) mcpToolProperty {
	return mcpToolProperty{Type: "boolean", Description: desc}
}
func intProp(desc string) mcpToolProperty { return mcpToolProperty{Type: "integer", Description: desc} }

// mcpTools is the fixed tool list (order matters: tools/list must be
// byte-identical across calls).
var mcpTools = []mcpTool{
	{
		Name:        "create_repo",
		Description: "Create a new repository.",
		InputSchema: mcpToolSchema{
			Type: "object",
			Properties: map[string]mcpToolProperty{
				"name":        strProp("The repository name."),
				"description": strProp("A short description of the repository."),
				"private":     boolProp("Whether the repository is private."),
				"owner":       strProp("The owner login."),
			},
			Required: []string{"name"},
		},
	},
	{
		Name:        "get_repo",
		Description: "Get a repository by owner and name.",
		InputSchema: mcpToolSchema{
			Type: "object",
			Properties: map[string]mcpToolProperty{
				"owner": strProp("The owner login."),
				"repo":  strProp("The repository name."),
			},
			Required: []string{"owner", "repo"},
		},
	},
	{
		Name:        "create_issue",
		Description: "Create an issue in a repository.",
		InputSchema: mcpToolSchema{
			Type: "object",
			Properties: map[string]mcpToolProperty{
				"owner": strProp("The owner login."),
				"repo":  strProp("The repository name."),
				"title": strProp("The issue title."),
				"body":  strProp("The issue body."),
			},
			Required: []string{"owner", "repo", "title"},
		},
	},
	{
		Name:        "list_issues",
		Description: "List issues in a repository.",
		InputSchema: mcpToolSchema{
			Type: "object",
			Properties: map[string]mcpToolProperty{
				"owner": strProp("The owner login."),
				"repo":  strProp("The repository name."),
				"state": strProp("Filter by state: open, closed, or all."),
			},
			Required: []string{"owner", "repo"},
		},
	},
	{
		Name:        "create_pull_request",
		Description: "Create a pull request in a repository.",
		InputSchema: mcpToolSchema{
			Type: "object",
			Properties: map[string]mcpToolProperty{
				"owner": strProp("The owner login."),
				"repo":  strProp("The repository name."),
				"title": strProp("The pull request title."),
				"body":  strProp("The pull request body."),
				"head":  strProp("The head branch name."),
				"base":  strProp("The base branch name."),
			},
			Required: []string{"owner", "repo", "title", "head", "base"},
		},
	},
	{
		Name:        "merge_pull_request",
		Description: "Merge a pull request in a repository.",
		InputSchema: mcpToolSchema{
			Type: "object",
			Properties: map[string]mcpToolProperty{
				"owner":       strProp("The owner login."),
				"repo":        strProp("The repository name."),
				"pull_number": intProp("The pull request number."),
			},
			Required: []string{"owner", "repo", "pull_number"},
		},
	},
}

// toolResult is a successful tools/call result.
type toolResult struct {
	Content           []toolContent `json:"content"`
	StructuredContent any           `json:"structuredContent,omitempty"`
	IsError           bool          `json:"isError,omitempty"`
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func okToolResult(payload any) *toolResult {
	data, _ := json.Marshal(payload)
	return &toolResult{
		Content:           []toolContent{{Type: "text", Text: string(data)}},
		StructuredContent: payload,
	}
}

func errToolResult(message string) *toolResult {
	return &toolResult{
		Content: []toolContent{{Type: "text", Text: message}},
		IsError: true,
	}
}

// ---------------------------------------------------------------- dispatch

// mcpHandler serves MCP over streamable HTTP at /mcp.
func mcpHandler(w http.ResponseWriter, r *http.Request, store *Store) {
	if r.Method == http.MethodDelete {
		// Session close (best-effort): always fine.
		if !checkAuth(w, r) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	if !checkAuth(w, r) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeRPCError(w, http.StatusBadRequest, nil, -32700, "Parse error")
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		// Not JSON at all: HTTP 400 with JSON-RPC -32700 (modern era).
		writeRPCError(w, http.StatusBadRequest, nil, -32700, "Parse error")
		return
	}
	if req.Method == "" {
		writeRPCError(w, http.StatusBadRequest, req.ID, -32600, "Invalid Request")
		return
	}
	// Notifications (no id) get no JSON-RPC response.
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "server/discover":
		writeRPCResult(w, req.ID, map[string]any{
			"supportedVersions": []string{"2026-07-28"},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        map[string]any{"name": "forge-core", "version": "1.0.0"},
		})
	case "initialize":
		var params map[string]any
		_ = json.Unmarshal(req.Params, &params)
		version := asString(params["protocolVersion"])
		if !isDateRevision(version) {
			version = "2025-11-25"
		}
		writeRPCResult(w, req.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "forge-core", "version": "1.0.0"},
		})
	case "tools/list":
		writeRPCResult(w, req.ID, map[string]any{"tools": mcpTools})
	case "tools/call":
		mcpCallTool(w, req, store)
	default:
		writeRPCError(w, http.StatusOK, req.ID, -32601, "Method not found: "+req.Method)
	}
}

func isDateRevision(s string) bool {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	for i, c := range s {
		if i == 4 || i == 7 {
			continue
		}
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func mcpCallTool(w http.ResponseWriter, req rpcRequest, store *Store) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &params)
	}
	if params.Arguments == nil {
		params.Arguments = map[string]any{}
	}
	name := params.Name
	// Match by suffix, case-insensitive (a server prefix is allowed).
	matched := ""
	for _, tool := range mcpTools {
		if strings.EqualFold(name, tool.Name) || strings.HasSuffix(strings.ToLower(name), strings.ToLower(tool.Name)) {
			matched = tool.Name
			break
		}
	}
	if matched == "" {
		writeRPCError(w, http.StatusOK, req.ID, -32601, "Unknown tool: "+name)
		return
	}
	// Required-argument check.
	var required []string
	for _, tool := range mcpTools {
		if tool.Name == matched {
			required = tool.InputSchema.Required
		}
	}
	for _, key := range required {
		if _, ok := params.Arguments[key]; !ok {
			writeRPCError(w, http.StatusOK, req.ID, -32602, "Missing required argument: "+key)
			return
		}
	}
	result := dispatchTool(store, matched, params.Arguments)
	writeRPCResult(w, req.ID, result)
}

func argString(args map[string]any, key string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func argBool(args map[string]any, key string, def bool) bool {
	if v, ok := args[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

func argInt(args map[string]any, key string) int {
	if v, ok := args[key]; ok {
		return asInt(v)
	}
	return 0
}

// dispatchTool runs one tool against the shared store.
func dispatchTool(store *Store, name string, args map[string]any) *toolResult {
	owner := argString(args, "owner")
	if owner == "" {
		owner = ViewerLogin
	}
	repoName := argString(args, "repo")
	switch name {
	case "create_repo":
		repoName = argString(args, "name")
		if repoName == "" {
			return errToolResult("name is required")
		}
		repo := store.CreateRepo(owner, repoName, argString(args, "description"), argBool(args, "private", false))
		if repo == nil {
			return errToolResult("repository already exists")
		}
		return okToolResult(map[string]any{
			"id":          repo.ID,
			"name":        repo.Name,
			"full_name":   repo.Owner + "/" + repo.Name,
			"owner":       map[string]any{"login": repo.Owner},
			"private":     repo.Private,
			"description": repo.Description,
		})
	case "get_repo":
		repo := store.GetRepo(owner, repoName)
		if repo == nil || owner != ViewerLogin {
			return errToolResult("repository not found")
		}
		return okToolResult(repoJSON(repo))
	case "create_issue":
		repo := store.GetRepo(owner, repoName)
		if repo == nil || owner != ViewerLogin {
			return errToolResult("repository not found")
		}
		title := argString(args, "title")
		if title == "" {
			return errToolResult("title is required")
		}
		issue := repo.CreateIssue(store, title, argString(args, "body"), nil)
		return okToolResult(map[string]any{
			"id":     issue.ID,
			"number": issue.Number,
			"title":  issue.Title,
			"body":   issue.Body,
			"state":  issue.State,
		})
	case "list_issues":
		repo := store.GetRepo(owner, repoName)
		if repo == nil || owner != ViewerLogin {
			return errToolResult("repository not found")
		}
		state := argString(args, "state")
		if state == "" {
			state = "open"
		}
		var list []any
		for _, issue := range repo.ListIssues() {
			if state != "all" && issue.State != state {
				continue
			}
			list = append(list, map[string]any{
				"id":     issue.ID,
				"number": issue.Number,
				"title":  issue.Title,
				"body":   issue.Body,
				"state":  issue.State,
			})
		}
		if list == nil {
			list = []any{}
		}
		return okToolResult(map[string]any{"issues": list})
	case "create_pull_request":
		repo := store.GetRepo(owner, repoName)
		if repo == nil || owner != ViewerLogin {
			return errToolResult("repository not found")
		}
		head, base := argString(args, "head"), argString(args, "base")
		headSHA, _ := repo.GetRef("refs/heads/" + head)
		baseSHA, _ := repo.GetRef("refs/heads/" + base)
		var files []PRFile
		if headSHA != "" && baseSHA != "" {
			files = repo.diffTrees(repo.commitTree(baseSHA), repo.commitTree(headSHA))
		}
		pr := repo.CreatePull(store, argString(args, "title"), argString(args, "body"), head, base, headSHA, baseSHA, files)
		pr.Mergeable = true
		return okToolResult(map[string]any{
			"id":     pr.ID,
			"number": pr.Number,
			"title":  pr.Title,
			"state":  pr.State,
			"head":   pr.Head,
			"base":   pr.Base,
		})
	case "merge_pull_request":
		repo := store.GetRepo(owner, repoName)
		if repo == nil || owner != ViewerLogin {
			return errToolResult("repository not found")
		}
		pr := repo.GetPull(argInt(args, "pull_number"))
		if pr == nil {
			return errToolResult("pull request not found")
		}
		mergeSHA, conflict := doMergePR(repo, pr, owner)
		if conflict != "" {
			return errToolResult("merge conflict: " + conflict)
		}
		return okToolResult(map[string]any{
			"number": pr.Number,
			"merged": true,
			"sha":    mergeSHA,
		})
	}
	return errToolResult("unknown tool")
}
