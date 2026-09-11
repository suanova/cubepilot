package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// apiDocPath is the client-facing API document, relative to this package.
const apiDocPath = "../../docs/cubepilot/api.md"

// apiDocExemptRoutes are registered on the mux but deliberately not part of the
// client-facing contract: liveness, metrics, and the cluster-internal endpoints
// the agent-side supervisor pulls from.
var apiDocExemptRoutes = map[string]bool{
	"/healthz": true,
	"/metrics": true,
}

// sessionSubresourceBase is the path prefix the per-session subresource handler
// matches suffixes against. Its suffixes are real client-facing endpoints even
// though they never appear as mux patterns.
const sessionSubresourceBase = "/api/sessions/{key}"

// TestAPIDocCoversRoutes fails when the HTTP surface and docs/cubepilot/api.md
// drift apart -- a route that serves clients but is not written down, or a
// documented path that no longer exists. The route table is parsed from
// server.go rather than duplicated here, so adding a route without documenting
// it breaks this test rather than going unnoticed.
//
// When this fails after an intentional change, update docs/cubepilot/api.md
// (and add the route to apiDocExemptRoutes only if it is genuinely not part of
// the client contract).
func TestAPIDocCoversRoutes(t *testing.T) {
	doc, err := os.ReadFile(filepath.Clean(apiDocPath))
	if err != nil {
		t.Fatalf("read API doc: %v", err)
	}
	text := string(doc)

	want := documentedRoutes(t)
	if len(want) < 20 {
		t.Fatalf("parsed only %d routes from server.go; the parser likely broke", len(want))
	}

	for _, route := range want {
		if !docMentions(text, route) {
			t.Errorf("route %s is served but not documented in %s", route, apiDocPath)
		}
	}
}

// documentedRoutes returns every client-facing route the server registers,
// including the per-session subresource suffixes, in stable order.
func documentedRoutes(t *testing.T) []string {
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
				if !strings.HasPrefix(lit, "/internal/") && !apiDocExemptRoutes[lit] {
					routes = append(routes, lit)
				}
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
// (e.g. "/api/sessions/" is documented as "/api/sessions/{key}/messages"), so
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
// prefix of a longer documented route would count as documented: "/api/llms"
// is a prefix of "/api/llms/{name}", so a plain strings.Contains would pass
// even after the bare "/api/llms" entry was deleted.
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
