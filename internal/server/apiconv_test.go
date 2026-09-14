package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/suanova/cubepilot/internal/config"
)

// These checks enforce the parts of docs/cubepilot/api-conventions.md that a
// test can hold. The rest of that document is judgement -- vocabulary, shape
// choice, status-code semantics -- and is marked there as unenforced, so a
// reader can tell which rules have teeth.

// TestClientRoutesRejectUnsupportedMethods fails when a route answers 2xx to a
// method no endpoint uses. A handler without a method check accepts anything,
// which is how four endpoints came to answer 200 to POST, PUT and DELETE alike
// before the v1 freeze.
//
// The assertion is "not 2xx" rather than "405" on purpose: a handler that
// consults its Kubernetes client before the method switch answers 503, and that
// is still a rejection.
func TestClientRoutesRejectUnsupportedMethods(t *testing.T) {
	h := New(config.Config{DefaultUser: "tester"}, nil, nil, nil, nil).Handler()

	for _, route := range registeredRoutes(t) {
		if strings.HasPrefix(route, "/internal/") || apiDocExemptRoutes[route] {
			continue
		}
		// PATCH is no endpoint's method, so reaching a handler with it proves the
		// handler never looked at the method.
		path := pathParamReplacer.Replace(route)
		req := httptest.NewRequest(http.MethodPatch, path, nil)
		req.Header.Set("X-CubePilot-User", "tester")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code >= 200 && rec.Code < 300 {
			t.Errorf("route %s answered %d to PATCH; a client route must reject an unsupported method", route, rec.Code)
		}
	}
}

// pathParamReplacer turns a route pattern into a requestable path.
var pathParamReplacer = strings.NewReplacer(
	"{key}", "probe-key",
	"{name}", "probe-name",
	"{id}", "probe-id",
	"{user}", "probe-user",
)

// wireTagSources are the packages that define CubePilot's own JSON wire types.
// Deliberately not the whole tree: internal/openclaw mirrors the
// OpenAI-compatible protocol, whose `tool_calls` / `finish_reason` are that
// protocol's names and must not be "corrected".
var wireTagSources = []string{
	".",
	filepath.Join("..", "runtime"),
}

var jsonTagRe = regexp.MustCompile(`json:"([^",]+)`)

// TestWireJSONTagsAreCamelCase fails on a snake_case json tag in our own wire
// types. The platform had `session_id` alongside `sessionId` for the same
// concept in two payloads; a rule that lives only in a document would not have
// caught that, and will not catch the next one.
func TestWireJSONTagsAreCamelCase(t *testing.T) {
	for _, dir := range wireTagSources {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			src, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			for _, m := range jsonTagRe.FindAllStringSubmatch(string(src), -1) {
				if strings.Contains(m[1], "_") {
					t.Errorf("%s: json tag %q is snake_case; our wire fields are lowerCamelCase", name, m[1])
				}
			}
		}
	}
}

// TestUnknownPathAnswersJSON fails when an unmatched path falls through to Go's
// default plain-text "404 page not found". One status code with two body
// formats forces every client to special-case it.
func TestUnknownPathAnswersJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/no-such-endpoint", nil)
	rec := httptest.NewRecorder()
	New(config.Config{DefaultUser: "tester"}, nil, nil, nil, nil).Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("unknown path Content-Type = %q, want JSON", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"error"`) {
		t.Errorf("unknown path body = %q, want a JSON error object", body)
	}
}
