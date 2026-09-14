package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// apiDocPath is the client-facing API document, relative to this package.
const apiDocPath = "../../docs/cubepilot/api.md"

// apiDocExemptRoutes are registered on the mux but deliberately not part of the
// client-facing contract: liveness, metrics, and the catch-all that turns
// unmatched paths into the API's JSON 404.
var apiDocExemptRoutes = map[string]bool{
	"/healthz": true,
	"/metrics": true,
	"/":        true, // catch-all 404 handler, not an endpoint
}

// sessionSubresourceBase is the path prefix the per-session subresource handler
// matches suffixes against. Its suffixes are real client-facing endpoints even
// though they never appear as mux patterns.
const sessionSubresourceBase = "/api/v1/sessions/{key}"

// TestClientRoutesAreVersioned fails when a client-facing route is registered
// outside apiPrefix. The version is the contract's freeze point: a route that
// quietly lands at /api/... would be outside it, and every client that believes
// it speaks v1 would have to special-case that one path.
//
// Adding a route under a NEW prefix is a deliberate act (a v2 surface) and
// should update this test alongside it.
func TestClientRoutesAreVersioned(t *testing.T) {
	for _, route := range registeredRoutes(t) {
		if strings.HasPrefix(route, "/internal/") || apiDocExemptRoutes[route] {
			continue // cluster-internal and liveness routes are deliberately unversioned
		}
		if !strings.HasPrefix(route, apiPrefix+"/") {
			t.Errorf("route %s is outside %s; client routes must carry the version prefix", route, apiPrefix)
		}
	}
}

// TestAPIDocCoversRoutes fails when a route serves clients but is not written
// down in docs/cubepilot/api.md. The route table is parsed from server.go
// rather than duplicated here, so adding a route without documenting it breaks
// this test rather than going unnoticed. The opposite direction -- a documented
// path that no longer exists -- is TestAPIDocHasNoStalePaths.
//
// When this fails after an intentional change, update docs/cubepilot/api.md
// (and add the route to apiDocExemptRoutes only if it is genuinely not part of
// the client contract).
func TestAPIDocCoversRoutes(t *testing.T) {
	text := readAPIDoc(t)

	var want []string
	for _, route := range registeredRoutes(t) {
		if strings.HasPrefix(route, "/internal/") || apiDocExemptRoutes[route] {
			continue
		}
		want = append(want, route)
	}
	if len(want) < 20 {
		t.Fatalf("parsed only %d routes from server.go; the parser likely broke", len(want))
	}

	for _, route := range want {
		if !docMentions(text, route) {
			t.Errorf("route %s is served but not documented in %s", route, apiDocPath)
		}
	}
}

// TestAPIDocHasNoStalePaths is the other half of the drift check: every path the
// document names must still be served. Documentation that survives the removal
// of its endpoint is as misleading as an undocumented endpoint -- a client
// codes against a path that answers 404.
//
// Every registered route counts here, including the cluster-internal ones: the
// doc is allowed to describe them (section 6.5 does), it just is not required to.
func TestAPIDocHasNoStalePaths(t *testing.T) {
	text := readAPIDoc(t)
	known := registeredRoutes(t)
	if len(known) < 20 {
		t.Fatalf("parsed only %d routes from server.go; the parser likely broke", len(known))
	}

	for _, candidate := range docPathCandidates(text) {
		if !isServed(known, candidate) {
			t.Errorf("path %s appears in %s but is not served; remove it or restore the route", candidate, apiDocPath)
		}
	}
}

// docPathCandidateRe matches the start of an API path in the prose. Path
// parameters are brace-delimited in both the route table and the doc, so
// "{...}" is part of the token.
var docPathCandidateRe = regexp.MustCompile(`/(?:api|internal)/[A-Za-z0-9_{}/.-]*`)

// docPathCandidates extracts every API path the document mentions. Subtree
// roots ("/api/v1/") and wildcards ("/api/v1/*") are not paths to a route, so they
// are dropped rather than matched.
func docPathCandidates(doc string) []string {
	var out []string
	seen := map[string]bool{}
	for _, loc := range docPathCandidateRe.FindAllStringIndex(doc, -1) {
		start, end := loc[0], loc[1]
		// A match inside a longer path is a source-file reference, not an API
		// path: "web/src/api/types.ts" contains "/api/v1/types.ts".
		if start > 0 && isPathByte(doc[start-1]) {
			continue
		}
		cand := strings.TrimRight(doc[start:end], ".,;:")
		if cand == "" || seen[cand] || strings.HasSuffix(cand, "/") || strings.Contains(cand, "*") {
			continue
		}
		seen[cand] = true
		out = append(out, cand)
	}
	return out
}

// isPathByte reports whether b can continue a path, so a match preceded by one
// is part of a longer path rather than the start of an API path.
func isPathByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '/' || b == '-' || b == '_' || b == '.':
		return true
	}
	return false
}

// isServed reports whether a documented path is covered by a registered route:
// either it is the route itself, or the route is a subtree base ("/api/v1/tasks/")
// that the path sits under ("/api/v1/tasks/{id}/run").
//
// Session paths are the exception. "/api/v1/sessions/" is a mux catch-all whose
// dispatcher answers 404 for anything that is not one of its known suffixes, so
// the generic subtree rule would accept a typo such as
// "/api/v1/sessions/{key}/question/pendng" as documented. Those paths are matched
// against the concrete subresources instead.
func isServed(known []string, candidate string) bool {
	if strings.HasPrefix(candidate, "/api/v1/sessions/") {
		return hasSessionSuffix(known, candidate)
	}
	for _, route := range known {
		if candidate == route {
			return true
		}
		if strings.HasSuffix(route, "/") && strings.HasPrefix(candidate, route) {
			return true
		}
	}
	return false
}

// hasSessionSuffix reports whether candidate names one of the session
// dispatcher's concrete subresources. Matching is by suffix because that is how
// handleSessionSubresource routes, so extra segments are absorbed into the
// session key: "/api/v1/sessions/a/b/messages" is served and reads as session "a/b".
func hasSessionSuffix(known []string, candidate string) bool {
	for _, route := range known {
		suffix, ok := strings.CutPrefix(route, sessionSubresourceBase)
		if !ok || !strings.HasPrefix(suffix, "/") {
			continue
		}
		if strings.HasSuffix(candidate, suffix) {
			return true
		}
	}
	return false
}

func readAPIDoc(t *testing.T) string {
	t.Helper()
	doc, err := os.ReadFile(filepath.Clean(apiDocPath))
	if err != nil {
		t.Fatalf("read API doc: %v", err)
	}
	return string(doc)
}

// registeredRoutes returns every route the server registers, including the
// per-session subresource suffixes, which are matched by suffix rather than
// registered as mux patterns.
func registeredRoutes(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "server.go", nil, 0)
	if err != nil {
		t.Fatalf("parse server.go: %v", err)
	}

	var routes, suffixes []string
	ast.Inspect(file, func(n ast.Node) bool {
		// mux.HandleFunc("<pattern>", ...)
		if call, ok := n.(*ast.CallExpr); ok {
			if lit := stringArg(call, "HandleFunc", 0); lit != "" {
				routes = append(routes, lit)
			}
			// strings.HasSuffix(r.URL.Path, "<suffix>") inside the subresource router
			if lit := stringArg(call, "HasSuffix", 1); lit != "" && strings.HasPrefix(lit, "/") {
				suffixes = append(suffixes, lit)
			}
		}
		return true
	})

	for _, suffix := range suffixes {
		routes = append(routes, sessionSubresourceBase+suffix)
	}
	return routes
}

// stringArg returns the call's arg at index i when it is a string literal and
// the call's callee is the named function.
func stringArg(call *ast.CallExpr, funcName string, i int) string {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != funcName {
		return ""
	}
	if i >= len(call.Args) {
		return ""
	}
	lit, ok := call.Args[i].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return ""
	}
	return s
}

// docMentions reports whether the doc names a route. A mux pattern ending in
// "/" is the base of a subtree whose concrete paths are written out in full
// (e.g. "/api/v1/sessions/" is documented as "/api/v1/sessions/{key}/messages"), so
// the trimmed base counts as a mention.
func docMentions(doc, route string) bool {
	if hasToken(doc, route) {
		return true
	}
	if base := strings.TrimSuffix(route, "/"); base != route {
		return hasToken(doc, base)
	}
	return false
}

// hasToken reports whether doc contains route as a complete token -- that is,
// not immediately followed by "/". Without this, a route that is merely the
// prefix of a longer documented route would count as documented: "/api/v1/llms"
// is a prefix of "/api/v1/llms/{name}", so a plain strings.Contains would pass
// even after the bare "/api/v1/llms" entry was deleted.
func hasToken(doc, route string) bool {
	for i := 0; ; {
		j := strings.Index(doc[i:], route)
		if j < 0 {
			return false
		}
		end := i + j + len(route)
		if end >= len(doc) || doc[end] != '/' {
			return true
		}
		i = end
	}
}
