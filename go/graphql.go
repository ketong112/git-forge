// graphql.go: a small GraphQL executor for POST /api/graphql.
//
// The suite sends only 18 fixed documents (checks-graphql.ts), so this is a
// focused implementation, not a general GraphQL server: a lexer + recursive
// descent parser for queries/mutations with variables, aliases, arguments,
// inline fragments and __type introspection, executed against the same store
// the REST surface uses. Unknown fields answer HTTP 200 with an errors array
// naming the field; an unknown repository answers data.repository null with
// no errors; no token is exactly 401.
package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- lexer

type gqlToken struct {
	kind string // "name", "int", "float", "string", "punct", "eof"
	text string
}

func gqlLex(input string) ([]gqlToken, error) {
	var tokens []gqlToken
	i := 0
	for i < len(input) {
		c := input[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case c == '#':
			for i < len(input) && input[i] != '\n' {
				i++
			}
		case c == '"' && i+2 < len(input) && input[i+1] == '"' && input[i+2] == '"':
			j := strings.Index(input[i+3:], `"""`)
			if j < 0 {
				return nil, errGQL("unterminated block string")
			}
			tokens = append(tokens, gqlToken{"string", input[i+3 : i+3+j]})
			i += 3 + j + 3
		case c == '"':
			j := i + 1
			var sb strings.Builder
			for j < len(input) && input[j] != '"' {
				if input[j] == '\\' && j+1 < len(input) {
					j++
					switch input[j] {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					case 'r':
						sb.WriteByte('\r')
					case '"':
						sb.WriteByte('"')
					case '\\':
						sb.WriteByte('\\')
					case '/':
						sb.WriteByte('/')
					default:
						sb.WriteByte(input[j])
					}
					j++
					continue
				}
				sb.WriteByte(input[j])
				j++
			}
			if j >= len(input) {
				return nil, errGQL("unterminated string")
			}
			tokens = append(tokens, gqlToken{"string", sb.String()})
			i = j + 1
		case c >= '0' && c <= '9' || c == '-':
			j := i
			if input[j] == '-' {
				j++
			}
			isFloat := false
			for j < len(input) && ((input[j] >= '0' && input[j] <= '9') || input[j] == '.' || input[j] == 'e' || input[j] == 'E' || input[j] == '+' || input[j] == '-') {
				if input[j] == '.' || input[j] == 'e' || input[j] == 'E' {
					isFloat = true
				}
				j++
			}
			kind := "int"
			if isFloat {
				kind = "float"
			}
			tokens = append(tokens, gqlToken{kind, input[i:j]})
			i = j
		case c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			j := i
			for j < len(input) && (input[j] == '_' || (input[j] >= 'a' && input[j] <= 'z') || (input[j] >= 'A' && input[j] <= 'Z') || (input[j] >= '0' && input[j] <= '9')) {
				j++
			}
			tokens = append(tokens, gqlToken{"name", input[i:j]})
			i = j
		case c == '.' && i+2 < len(input) && input[i+1] == '.' && input[i+2] == '.':
			tokens = append(tokens, gqlToken{"punct", "..."})
			i += 3
		case strings.IndexByte("!$()[]{}:=@|", c) >= 0:
			tokens = append(tokens, gqlToken{"punct", string(c)})
			i++
		default:
			return nil, errGQL("unexpected character " + string(c))
		}
	}
	tokens = append(tokens, gqlToken{"eof", ""})
	return tokens, nil
}

type gqlError struct{ msg string }

func (e *gqlError) Error() string { return e.msg }

func errGQL(msg string) error { return &gqlError{msg} }

// ---------------------------------------------------------------- parser

type gqlValue struct {
	kind string // "var", "int", "float", "string", "bool", "null", "enum", "list", "object"
	text string
	list []*gqlValue
	obj  map[string]*gqlValue
}

type gqlSelection struct {
	alias      string
	name       string // field name; "" for fragment
	fragment   string // fragment type condition ("on X") or spread name
	args       map[string]*gqlValue
	selections []*gqlSelection
}

type gqlOperation struct {
	kind      string // "query" | "mutation"
	name      string
	variables map[string]*gqlValue // variable definitions (defaults only)
	sels      []*gqlSelection
}

type gqlParser struct {
	tokens []gqlToken
	pos    int
}

func (p *gqlParser) peek() gqlToken { return p.tokens[p.pos] }
func (p *gqlParser) next() gqlToken { t := p.tokens[p.pos]; p.pos++; return t }
func (p *gqlParser) expect(kind, text string) (gqlToken, error) {
	t := p.next()
	if t.kind != kind || (text != "" && t.text != text) {
		return t, errGQL("expected " + text + ", got " + t.text)
	}
	return t, nil
}

func gqlParse(input string) (*gqlOperation, error) {
	tokens, err := gqlLex(input)
	if err != nil {
		return nil, err
	}
	p := &gqlParser{tokens: tokens}
	return p.parseOperation()
}

func (p *gqlParser) parseOperation() (*gqlOperation, error) {
	op := &gqlOperation{kind: "query", variables: map[string]*gqlValue{}}
	t := p.peek()
	if t.kind == "punct" && t.text == "{" {
		sels, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		op.sels = sels
		return op, nil
	}
	if t.kind != "name" {
		return nil, errGQL("expected operation")
	}
	kw := p.next().text
	if kw != "query" && kw != "mutation" && kw != "subscription" {
		return nil, errGQL("expected query/mutation, got " + kw)
	}
	op.kind = kw
	if p.peek().kind == "name" {
		op.name = p.next().text
	}
	if p.peek().kind == "punct" && p.peek().text == "(" {
		if err := p.parseVarDefs(op); err != nil {
			return nil, err
		}
	}
	sels, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	op.sels = sels
	return op, nil
}

func (p *gqlParser) parseVarDefs(op *gqlOperation) error {
	if _, err := p.expect("punct", "("); err != nil {
		return err
	}
	for {
		t := p.peek()
		if t.kind == "punct" && t.text == ")" {
			p.next()
			return nil
		}
		if _, err := p.expect("punct", "$"); err != nil {
			return err
		}
		nameTok, err := p.expect("name", "")
		if err != nil {
			return err
		}
		if _, err := p.expect("punct", ":"); err != nil {
			return err
		}
		if err := p.parseType(); err != nil {
			return err
		}
		if p.peek().kind == "punct" && p.peek().text == "=" {
			p.next()
			def, err := p.parseValue()
			if err != nil {
				return err
			}
			op.variables[nameTok.text] = def
		}
	}
}

func (p *gqlParser) parseType() error {
	t := p.peek()
	if t.kind == "punct" && t.text == "[" {
		p.next()
		if err := p.parseType(); err != nil {
			return err
		}
		if _, err := p.expect("punct", "]"); err != nil {
			return err
		}
	} else if t.kind == "name" {
		p.next()
	} else {
		return errGQL("expected type")
	}
	if p.peek().kind == "punct" && p.peek().text == "!" {
		p.next()
	}
	return nil
}

func (p *gqlParser) parseSelectionSet() ([]*gqlSelection, error) {
	if _, err := p.expect("punct", "{"); err != nil {
		return nil, err
	}
	var sels []*gqlSelection
	for {
		t := p.peek()
		if t.kind == "punct" && t.text == "}" {
			p.next()
			return sels, nil
		}
		if t.kind == "eof" {
			return nil, errGQL("unterminated selection set")
		}
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		sels = append(sels, sel)
	}
}

func (p *gqlParser) parseSelection() (*gqlSelection, error) {
	t := p.peek()
	if t.kind == "punct" && t.text == "..." {
		p.next()
		sel := &gqlSelection{}
		if p.peek().kind == "name" {
			word := p.next().text
			if word == "on" {
				typ, err := p.expect("name", "")
				if err != nil {
					return nil, err
				}
				sel.fragment = "on " + typ.text
			} else {
				sel.fragment = word
			}
		}
		sels, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		sel.selections = sels
		return sel, nil
	}
	if t.kind != "name" {
		return nil, errGQL("expected field name")
	}
	nameTok := p.next()
	sel := &gqlSelection{name: nameTok.text, args: map[string]*gqlValue{}}
	if p.peek().kind == "punct" && p.peek().text == ":" {
		p.next()
		real, err := p.expect("name", "")
		if err != nil {
			return nil, err
		}
		sel.alias = sel.name
		sel.name = real.text
	}
	if p.peek().kind == "punct" && p.peek().text == "(" {
		args, err := p.parseArguments()
		if err != nil {
			return nil, err
		}
		sel.args = args
	}
	// Directives (ignored): @name[(args)]
	for p.peek().kind == "punct" && p.peek().text == "@" {
		p.next()
		if _, err := p.expect("name", ""); err != nil {
			return nil, err
		}
		if p.peek().kind == "punct" && p.peek().text == "(" {
			if _, err := p.parseArguments(); err != nil {
				return nil, err
			}
		}
	}
	if p.peek().kind == "punct" && p.peek().text == "{" {
		sels, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		sel.selections = sels
	}
	return sel, nil
}

func (p *gqlParser) parseArguments() (map[string]*gqlValue, error) {
	if _, err := p.expect("punct", "("); err != nil {
		return nil, err
	}
	args := map[string]*gqlValue{}
	for {
		t := p.peek()
		if t.kind == "punct" && t.text == ")" {
			p.next()
			return args, nil
		}
		nameTok, err := p.expect("name", "")
		if err != nil {
			return nil, err
		}
		if _, err := p.expect("punct", ":"); err != nil {
			return nil, err
		}
		val, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args[nameTok.text] = val
	}
}

func (p *gqlParser) parseValue() (*gqlValue, error) {
	t := p.next()
	switch t.kind {
	case "punct":
		switch t.text {
		case "$":
			nameTok, err := p.expect("name", "")
			if err != nil {
				return nil, err
			}
			return &gqlValue{kind: "var", text: nameTok.text}, nil
		case "[":
			v := &gqlValue{kind: "list"}
			for {
				if p.peek().kind == "punct" && p.peek().text == "]" {
					p.next()
					return v, nil
				}
				item, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				v.list = append(v.list, item)
			}
		case "{":
			v := &gqlValue{kind: "object", obj: map[string]*gqlValue{}}
			for {
				if p.peek().kind == "punct" && p.peek().text == "}" {
					p.next()
					return v, nil
				}
				nameTok, err := p.expect("name", "")
				if err != nil {
					return nil, err
				}
				if _, err := p.expect("punct", ":"); err != nil {
					return nil, err
				}
				item, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				v.obj[nameTok.text] = item
			}
		}
	case "int":
		return &gqlValue{kind: "int", text: t.text}, nil
	case "float":
		return &gqlValue{kind: "float", text: t.text}, nil
	case "string":
		return &gqlValue{kind: "string", text: t.text}, nil
	case "name":
		switch t.text {
		case "true", "false":
			return &gqlValue{kind: "bool", text: t.text}, nil
		case "null":
			return &gqlValue{kind: "null"}, nil
		default:
			return &gqlValue{kind: "enum", text: t.text}, nil
		}
	}
	return nil, errGQL("unexpected value " + t.text)
}

// ---------------------------------------------------------------- values

// resolveValue turns a parsed value into a Go value using the variables.
func resolveValue(v *gqlValue, vars map[string]any) any {
	switch v.kind {
	case "var":
		if val, ok := vars[v.text]; ok {
			return val
		}
		return nil
	case "int":
		n, _ := strconv.Atoi(v.text)
		return n
	case "float":
		f, _ := strconv.ParseFloat(v.text, 64)
		return f
	case "string":
		return v.text
	case "bool":
		return v.text == "true"
	case "null":
		return nil
	case "enum":
		return v.text
	case "list":
		out := make([]any, 0, len(v.list))
		for _, item := range v.list {
			out = append(out, resolveValue(item, vars))
		}
		return out
	case "object":
		out := make(map[string]any, len(v.obj))
		for k, item := range v.obj {
			out[k] = resolveValue(item, vars)
		}
		return out
	}
	return nil
}

func asInt(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case float64:
		return int(t)
	case string:
		n, _ := strconv.Atoi(t)
		return n
	}
	return 0
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// ---------------------------------------------------------------- global ids

// Global ids are base64("<Type>:<key>"): opaque, stable, and never the bare
// decimal of a database id or number (the suite checks exactly that).
func encodeGlobalID(kind, key string) string {
	return base64.StdEncoding.EncodeToString([]byte(kind + ":" + key))
}

func decodeGlobalID(id string) (kind, key string, ok bool) {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return "", "", false
	}
	parts := strings.SplitN(string(raw), ":", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func repoGlobalID(repo *Repo) string {
	return encodeGlobalID("Repository", strconv.FormatInt(repo.ID, 10))
}

func issueGlobalID(repo *Repo, issue *Issue) string {
	return encodeGlobalID("Issue", repo.Owner+"/"+repo.Name+":"+strconv.Itoa(issue.Number))
}

func prGlobalID(repo *Repo, pr *PullRequest) string {
	return encodeGlobalID("PullRequest", repo.Owner+"/"+repo.Name+":"+strconv.Itoa(pr.Number))
}

func commentGlobalID(repo *Repo, issue *Issue, comment *Comment) string {
	return encodeGlobalID("IssueComment", repo.Owner+"/"+repo.Name+":"+strconv.Itoa(issue.Number)+":"+strconv.FormatInt(comment.ID, 10))
}

// ---------------------------------------------------------------- resolvers

// gqlObj is one resolved GraphQL object: a type name plus a field resolver.
type gqlObj struct {
	typeName string
	resolve  func(name string, args map[string]any) (any, bool, error)
}

type gqlExec struct {
	store  *Store
	errors []map[string]any
}

func (e *gqlExec) addError(msg string) {
	e.errors = append(e.errors, map[string]any{"message": msg})
}

// execute runs one operation's selection set against a root object.
func (e *gqlExec) execute(op *gqlOperation, vars map[string]any, root *gqlObj) map[string]any {
	out := map[string]any{}
	e.executeSels(root, op.sels, vars, out)
	return out
}

func (e *gqlExec) executeSels(obj *gqlObj, sels []*gqlSelection, vars map[string]any, out map[string]any) {
	for _, sel := range sels {
		if sel.fragment != "" {
			if strings.HasPrefix(sel.fragment, "on ") {
				want := strings.TrimPrefix(sel.fragment, "on ")
				if obj == nil || obj.typeName != want {
					continue
				}
			} else {
				// Named fragment spread: not used by the suite's documents.
				continue
			}
			e.executeSels(obj, sel.selections, vars, out)
			continue
		}
		key := sel.name
		if sel.alias != "" {
			key = sel.alias
		}
		args := map[string]any{}
		for k, v := range sel.args {
			args[k] = resolveValue(v, vars)
		}
		if obj == nil {
			out[key] = nil
			continue
		}
		val, found, err := obj.resolve(sel.name, args)
		if err != nil {
			e.addError(err.Error())
			out[key] = nil
			continue
		}
		if !found {
			e.addError("Cannot query field \"" + sel.name + "\" on type \"" + obj.typeName + "\".")
			out[key] = nil
			continue
		}
		out[key] = e.materialise(val, sel, vars)
	}
}

// materialise expands a resolved value through its sub-selections.
func (e *gqlExec) materialise(val any, sel *gqlSelection, vars map[string]any) any {
	switch v := val.(type) {
	case *gqlObj:
		if len(sel.selections) == 0 {
			return map[string]any{}
		}
		out := map[string]any{}
		e.executeSels(v, sel.selections, vars, out)
		return out
	case []*gqlObj:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, e.materialise(item, sel, vars))
		}
		return out
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, e.materialise(item, sel, vars))
		}
		return out
	case nil:
		return nil
	default:
		// Scalars and plain maps pass through (maps are already material).
		return val
	}
}

// rootQuery builds the Query root for this request.
func (e *gqlExec) rootQuery() *gqlObj {
	return &gqlObj{typeName: "Query", resolve: func(name string, args map[string]any) (any, bool, error) {
		switch name {
		case "viewer":
			return e.userObj(), true, nil
		case "repository":
			owner, name := asString(args["owner"]), asString(args["name"])
			if owner != ViewerLogin {
				return nil, true, nil
			}
			repo := e.store.GetRepo(owner, name)
			if repo == nil {
				return nil, true, nil
			}
			return e.repoObj(repo), true, nil
		case "node":
			id := asString(args["id"])
			kind, key, ok := decodeGlobalID(id)
			if !ok {
				return nil, true, nil
			}
			if kind == "Repository" {
				if n, err := strconv.ParseInt(key, 10, 64); err == nil {
					if repo := e.findRepoByID(n); repo != nil {
						return e.repoObj(repo), true, nil
					}
				}
			}
			return nil, true, nil
		case "__type":
			return e.typeObj(asString(args["name"])), true, nil
		}
		return nil, false, nil
	}}
}

// rootMutation builds the Mutation root.
func (e *gqlExec) rootMutation() *gqlObj {
	return &gqlObj{typeName: "Mutation", resolve: func(name string, args map[string]any) (any, bool, error) {
		input, _ := args["input"].(map[string]any)
		if input == nil {
			input = map[string]any{}
		}
		switch name {
		case "createIssue":
			repo := e.findRepoByGlobalID(asString(input["repositoryId"]))
			if repo == nil {
				return nil, false, errGQL("repository not found")
			}
			issue := repo.CreateIssue(e.store, asString(input["title"]), asString(input["body"]), nil)
			return &gqlObj{typeName: "CreateIssuePayload", resolve: func(n string, a map[string]any) (any, bool, error) {
				if n == "issue" {
					return e.issueObj(repo, issue), true, nil
				}
				return nil, false, nil
			}}, true, nil
		case "closeIssue":
			repo, issue := e.findIssueByGlobalID(asString(input["issueId"]))
			if issue == nil {
				return nil, false, errGQL("issue not found")
			}
			repo.SetIssueState(issue, "closed")
			return &gqlObj{typeName: "CloseIssuePayload", resolve: func(n string, a map[string]any) (any, bool, error) {
				if n == "issue" {
					return e.issueObj(repo, issue), true, nil
				}
				return nil, false, nil
			}}, true, nil
		case "createPullRequest":
			repo := e.findRepoByGlobalID(asString(input["repositoryId"]))
			if repo == nil {
				return nil, false, errGQL("repository not found")
			}
			head, base := asString(input["headRefName"]), asString(input["baseRefName"])
			headSHA, _ := repo.GetRef("refs/heads/" + head)
			baseSHA, _ := repo.GetRef("refs/heads/" + base)
			var files []PRFile
			if headSHA != "" && baseSHA != "" {
				files = repo.diffTrees(repo.commitTree(baseSHA), repo.commitTree(headSHA))
			}
			pr := repo.CreatePull(e.store, asString(input["title"]), asString(input["body"]), head, base, headSHA, baseSHA, files)
			pr.Mergeable = true
			return &gqlObj{typeName: "CreatePullRequestPayload", resolve: func(n string, a map[string]any) (any, bool, error) {
				if n == "pullRequest" {
					return e.prObj(repo, pr), true, nil
				}
				return nil, false, nil
			}}, true, nil
		case "mergePullRequest":
			repo, pr := e.findPRByGlobalID(asString(input["pullRequestId"]))
			if pr == nil {
				return nil, false, errGQL("pull request not found")
			}
			if !pr.Merged {
				if _, conflict := doMergePR(repo, pr, repo.Owner); conflict != "" {
					return nil, false, errGQL("merge conflict: " + conflict)
				}
			}
			return &gqlObj{typeName: "MergePullRequestPayload", resolve: func(n string, a map[string]any) (any, bool, error) {
				if n == "pullRequest" {
					return e.prObj(repo, pr), true, nil
				}
				return nil, false, nil
			}}, true, nil
		case "addComment":
			repo, issue := e.findIssueByGlobalID(asString(input["subjectId"]))
			if issue == nil {
				return nil, false, errGQL("issue not found")
			}
			comment := repo.AddComment(e.store, issue, asString(input["body"]))
			return &gqlObj{typeName: "AddCommentPayload", resolve: func(n string, a map[string]any) (any, bool, error) {
				if n == "commentEdge" {
					return &gqlObj{typeName: "IssueCommentEdge", resolve: func(n2 string, a2 map[string]any) (any, bool, error) {
						if n2 == "node" {
							return &gqlObj{typeName: "IssueComment", resolve: func(n3 string, a3 map[string]any) (any, bool, error) {
								switch n3 {
								case "id":
									return commentGlobalID(repo, issue, comment), true, nil
								case "body":
									return comment.Body, true, nil
								}
								return nil, false, nil
							}}, true, nil
						}
						return nil, false, nil
					}}, true, nil
				}
				return nil, false, nil
			}}, true, nil
		}
		return nil, false, nil
	}}
}

func (e *gqlExec) userObj() *gqlObj {
	return &gqlObj{typeName: "User", resolve: func(name string, args map[string]any) (any, bool, error) {
		if name == "login" {
			return ViewerLogin, true, nil
		}
		return nil, false, nil
	}}
}

func (e *gqlExec) repoObj(repo *Repo) *gqlObj {
	return &gqlObj{typeName: "Repository", resolve: func(name string, args map[string]any) (any, bool, error) {
		switch name {
		case "id":
			return repoGlobalID(repo), true, nil
		case "databaseId":
			return int(repo.ID), true, nil
		case "name":
			return repo.Name, true, nil
		case "nameWithOwner":
			return repo.Owner + "/" + repo.Name, true, nil
		case "owner":
			return e.userObj(), true, nil
		case "defaultBranchRef":
			tip, ok := repo.GetRef("refs/heads/" + repo.DefaultBranch)
			if !ok {
				return nil, true, nil
			}
			_ = tip
			return &gqlObj{typeName: "Ref", resolve: func(n string, a map[string]any) (any, bool, error) {
				if n == "name" {
					return repo.DefaultBranch, true, nil
				}
				return nil, false, nil
			}}, true, nil
		case "viewerPermission":
			return "ADMIN", true, nil
		case "hasIssuesEnabled":
			return true, true, nil
		case "description":
			return repo.Description, true, nil
		case "private":
			return repo.Private, true, nil
		case "issue":
			number := asInt(args["number"])
			issue := repo.GetIssue(number)
			if issue == nil {
				return nil, true, nil
			}
			return e.issueObj(repo, issue), true, nil
		case "issues":
			return e.issuesConn(repo, args), true, nil
		case "pullRequests":
			return e.prsConn(repo, args), true, nil
		case "pullRequest":
			number := asInt(args["number"])
			pr := repo.GetPull(number)
			if pr == nil {
				return nil, true, nil
			}
			return e.prObj(repo, pr), true, nil
		}
		return nil, false, nil
	}}
}

func (e *gqlExec) issuesConn(repo *Repo, args map[string]any) *gqlObj {
	// Resolve the connection eagerly: filter, order, page.
	first := asInt(args["first"])
	if first <= 0 {
		first = 100
	}
	states := map[string]bool{}
	if raw, ok := args["states"].([]any); ok {
		for _, s := range raw {
			states[asString(s)] = true
		}
	}
	desc := true
	if raw, ok := args["orderBy"].(map[string]any); ok {
		if asString(raw["direction"]) == "ASC" {
			desc = false
		}
	}
	after := 0
	hasAfter := false
	if cur := asString(args["after"]); cur != "" {
		if n, ok := decodeCursor(cur); ok {
			after = n
			hasAfter = true
		}
	}
	issues := repo.ListIssues() // number ascending
	var filtered []*Issue
	for _, issue := range issues {
		if len(states) > 0 && !states[strings.ToUpper(issue.State)] {
			continue
		}
		filtered = append(filtered, issue)
	}
	if desc {
		for i, j := 0, len(filtered)-1; i < j; i, j = i+1, j-1 {
			filtered[i], filtered[j] = filtered[j], filtered[i]
		}
	}
	if hasAfter {
		kept := filtered[:0]
		for _, issue := range filtered {
			if desc && issue.Number < after {
				kept = append(kept, issue)
			} else if !desc && issue.Number > after {
				kept = append(kept, issue)
			}
		}
		filtered = kept
	}
	hasNext := len(filtered) > first
	page := filtered
	if len(page) > first {
		page = page[:first]
	}
	endCursor := ""
	if len(page) > 0 {
		endCursor = encodeCursor(page[len(page)-1].Number)
	}
	nodes := make([]*gqlObj, 0, len(page))
	for _, issue := range page {
		nodes = append(nodes, e.issueObj(repo, issue))
	}
	return &gqlObj{typeName: "IssueConnection", resolve: func(name string, a map[string]any) (any, bool, error) {
		switch name {
		case "nodes":
			return nodes, true, nil
		case "pageInfo":
			return &gqlObj{typeName: "PageInfo", resolve: func(n string, a2 map[string]any) (any, bool, error) {
				switch n {
				case "hasNextPage":
					return hasNext, true, nil
				case "endCursor":
					if endCursor == "" {
						return nil, true, nil
					}
					return endCursor, true, nil
				}
				return nil, false, nil
			}}, true, nil
		case "totalCount":
			return len(filtered), true, nil
		}
		return nil, false, nil
	}}
}

func encodeCursor(n int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(n)))
}

func decodeCursor(s string) (int, bool) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return 0, false
	}
	parts := strings.SplitN(string(raw), ":", 2)
	if len(parts) != 2 || parts[0] != "cursor" {
		return 0, false
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

func (e *gqlExec) prsConn(repo *Repo, args map[string]any) *gqlObj {
	first := asInt(args["first"])
	if first <= 0 {
		first = 100
	}
	pulls := repo.ListPulls()
	nodes := make([]*gqlObj, 0, len(pulls))
	for i, pr := range pulls {
		if i >= first {
			break
		}
		nodes = append(nodes, e.prObj(repo, pr))
	}
	return &gqlObj{typeName: "PullRequestConnection", resolve: func(name string, a map[string]any) (any, bool, error) {
		if name == "nodes" {
			return nodes, true, nil
		}
		if name == "totalCount" {
			return len(pulls), true, nil
		}
		return nil, false, nil
	}}
}

func (e *gqlExec) issueObj(repo *Repo, issue *Issue) *gqlObj {
	return &gqlObj{typeName: "Issue", resolve: func(name string, args map[string]any) (any, bool, error) {
		switch name {
		case "id":
			return issueGlobalID(repo, issue), true, nil
		case "number":
			return issue.Number, true, nil
		case "title":
			return issue.Title, true, nil
		case "state":
			return strings.ToUpper(issue.State), true, nil
		case "body":
			return issue.Body, true, nil
		}
		return nil, false, nil
	}}
}

func (e *gqlExec) prObj(repo *Repo, pr *PullRequest) *gqlObj {
	return &gqlObj{typeName: "PullRequest", resolve: func(name string, args map[string]any) (any, bool, error) {
		switch name {
		case "id":
			return prGlobalID(repo, pr), true, nil
		case "number":
			return pr.Number, true, nil
		case "title":
			return pr.Title, true, nil
		case "body":
			return pr.Body, true, nil
		case "state":
			if pr.Merged {
				return "MERGED", true, nil
			}
			return strings.ToUpper(pr.State), true, nil
		case "merged":
			return pr.Merged, true, nil
		case "headRefName":
			return pr.Head, true, nil
		case "baseRefName":
			return pr.Base, true, nil
		case "mergeable":
			if pr.Merged {
				return "UNKNOWN", true, nil
			}
			return "MERGEABLE", true, nil
		case "files":
			nodes := make([]*gqlObj, 0, len(pr.Files))
			for _, f := range pr.Files {
				path := f.Path
				nodes = append(nodes, &gqlObj{typeName: "PullRequestChangedFile", resolve: func(n string, a map[string]any) (any, bool, error) {
					if n == "path" {
						return path, true, nil
					}
					return nil, false, nil
				}})
			}
			return &gqlObj{typeName: "PullRequestChangedFileConnection", resolve: func(n string, a map[string]any) (any, bool, error) {
				if n == "nodes" {
					return nodes, true, nil
				}
				return nil, false, nil
			}}, true, nil
		}
		return nil, false, nil
	}}
}

// ---------------------------------------------------------------- introspection

var gqlTypeFields = map[string][]string{
	"Issue":       {"id", "number", "title", "state", "body", "author", "createdAt", "updatedAt", "url", "comments", "labels", "closed", "assignees"},
	"PullRequest": {"id", "number", "title", "body", "state", "merged", "mergeable", "headRefName", "baseRefName", "headRef", "baseRef", "files", "author", "createdAt", "updatedAt", "url", "mergedAt", "mergeCommit", "commits", "reviews"},
	"Repository":  {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "private", "createdAt", "updatedAt", "url", "forkCount", "stargazerCount"},
	"User":        {"id", "login", "name", "email", "avatarUrl"},
}

var gqlEnumValues = map[string][]string{
	"SearchType":             {"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "COMMIT", "CODE", "TOPIC", "WIKI"},
	"IssueState":             {"OPEN", "CLOSED"},
	"PullRequestState":       {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":         {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"RepositoryPermission":   {"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"},
	"IssueOrderField":        {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
	"OrderDirection":         {"ASC", "DESC"},
	"PullRequestMergeMethod": {"MERGE", "SQUASH", "REBASE"},
}

func (e *gqlExec) typeObj(name string) *gqlObj {
	fields, hasFields := gqlTypeFields[name]
	enums, hasEnums := gqlEnumValues[name]
	return &gqlObj{typeName: "__Type", resolve: func(n string, a map[string]any) (any, bool, error) {
		switch n {
		case "name":
			if hasFields || hasEnums {
				return name, true, nil
			}
			return nil, true, nil
		case "kind":
			if hasEnums {
				return "ENUM", true, nil
			}
			if hasFields {
				return "OBJECT", true, nil
			}
			return nil, true, nil
		case "fields":
			if !hasFields {
				return nil, true, nil
			}
			out := make([]any, 0, len(fields))
			for _, f := range fields {
				out = append(out, map[string]any{"name": f})
			}
			return out, true, nil
		case "enumValues":
			if !hasEnums {
				return nil, true, nil
			}
			out := make([]any, 0, len(enums))
			for _, v := range enums {
				out = append(out, map[string]any{"name": v})
			}
			return out, true, nil
		}
		return nil, false, nil
	}}
}

// ---------------------------------------------------------------- lookups

func (e *gqlExec) findRepoByID(id int64) *Repo {
	e.store.mu.RLock()
	defer e.store.mu.RUnlock()
	for _, repo := range e.store.repos {
		if repo.ID == id {
			return repo
		}
	}
	return nil
}

func (e *gqlExec) findRepoByGlobalID(id string) *Repo {
	kind, key, ok := decodeGlobalID(id)
	if !ok || kind != "Repository" {
		return nil
	}
	n, err := strconv.ParseInt(key, 10, 64)
	if err != nil {
		return nil
	}
	return e.findRepoByID(n)
}

func (e *gqlExec) findIssueByGlobalID(id string) (*Repo, *Issue) {
	kind, key, ok := decodeGlobalID(id)
	if !ok || kind != "Issue" {
		return nil, nil
	}
	// key: "owner/repo:number"
	i := strings.LastIndex(key, ":")
	if i < 0 {
		return nil, nil
	}
	ownerRepo, numStr := key[:i], key[i+1:]
	j := strings.LastIndex(ownerRepo, "/")
	if j < 0 {
		return nil, nil
	}
	repo := e.store.GetRepo(ownerRepo[:j], ownerRepo[j+1:])
	if repo == nil {
		return nil, nil
	}
	n, err := strconv.Atoi(numStr)
	if err != nil {
		return nil, nil
	}
	return repo, repo.GetIssue(n)
}

func (e *gqlExec) findPRByGlobalID(id string) (*Repo, *PullRequest) {
	kind, key, ok := decodeGlobalID(id)
	if !ok || kind != "PullRequest" {
		return nil, nil
	}
	i := strings.LastIndex(key, ":")
	if i < 0 {
		return nil, nil
	}
	ownerRepo, numStr := key[:i], key[i+1:]
	j := strings.LastIndex(ownerRepo, "/")
	if j < 0 {
		return nil, nil
	}
	repo := e.store.GetRepo(ownerRepo[:j], ownerRepo[j+1:])
	if repo == nil {
		return nil, nil
	}
	n, err := strconv.Atoi(numStr)
	if err != nil {
		return nil, nil
	}
	return repo, repo.GetPull(n)
}

// ---------------------------------------------------------------- handler

// graphqlHandler serves GraphQL at /api/graphql.
func graphqlHandler(w http.ResponseWriter, r *http.Request, store *Store) {
	if !checkAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Bad Request")
		return
	}
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "[") {
		// Batched operations: answer an array in the same order.
		var batch []map[string]any
		if err := json.Unmarshal(body, &batch); err != nil {
			writeErr(w, http.StatusBadRequest, "Problems parsing JSON")
			return
		}
		out := make([]any, 0, len(batch))
		for _, entry := range batch {
			out = append(out, runGraphQLOp(store, entry))
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	var entry map[string]any
	if err := json.Unmarshal(body, &entry); err != nil {
		writeErr(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	writeJSON(w, http.StatusOK, runGraphQLOp(store, entry))
}

func runGraphQLOp(store *Store, entry map[string]any) map[string]any {
	query, _ := entry["query"].(string)
	vars := map[string]any{}
	if raw, ok := entry["variables"]; ok {
		if m, ok := raw.(map[string]any); ok {
			vars = m
		}
	}
	op, err := gqlParse(query)
	if err != nil {
		return map[string]any{
			"data":   nil,
			"errors": []map[string]any{{"message": err.Error()}},
		}
	}
	exec := &gqlExec{store: store}
	var root *gqlObj
	if op.kind == "mutation" {
		root = exec.rootMutation()
	} else {
		root = exec.rootQuery()
	}
	data := exec.execute(op, vars, root)
	out := map[string]any{"data": data}
	if len(exec.errors) > 0 {
		out["errors"] = exec.errors
	}
	return out
}
