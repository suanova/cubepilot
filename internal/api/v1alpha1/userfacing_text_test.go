package v1alpha1

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every artifact below is published to a user, so an internal reference in one
// is a leak:
//
//   - the CRD YAML carries the OpenAPI descriptions controller-gen derived from
//     this package's doc comments, and `kubectl explain` shows them verbatim;
//   - Helm keeps the chart templates' `#` comments in `helm template` and
//     `helm get manifest` output;
//   - the embedded SKILL.md files are baked into the agent image and read by the
//     agent (and listed in the skill catalog);
//   - README.md is the repository's public front page.
//
// This package's own doc comments are covered through the generated CRD YAML
// rather than directly: the YAML is what actually ships, and scanning it also
// catches the case that is easy to miss -- a comment on a comment-less
// struct-typed field or on a slice item type is published as the field's
// description.
//
// The generated internal/skill/skills/cubestack-platform/crd-reference.md is
// deliberately not scanned: it mirrors the vendored CubeStack CRDs, whose
// descriptions this repo does not author.
var shippedText = []struct {
	name string
	glob string
}{
	{"crd-schemas", "config/crd/bases/*.yaml"},
	{"chart-crd-schemas", "deploy/charts/cubepilot-chart/crds/*.yaml"},
	{"chart-manifests", "deploy/charts/cubepilot-chart/templates/*.yaml"},
	{"chart-values", "deploy/charts/cubepilot-chart/values.yaml"},
	{"embedded-skills", "internal/skill/skills/*/SKILL.md"},
	{"readme", "README.md"},
}

// internalRefs are the internal-bookkeeping shapes that must not reach a user:
// issue/PR numbers, feature-requirement IDs, milestone tags, roadmap-phase
// labels, and design-doc section references.
var internalRefs = []*regexp.Regexp{
	// "issue #123", "issues 45", "PR #7".
	regexp.MustCompile(`(?i)\b(issue|pr)s?\s*#?\s*[0-9]+`),
	// A bare #123 reference.
	regexp.MustCompile(`#[0-9]{2,4}\b`),
	// FR-M2-004, NFR-011, REQ-3.
	regexp.MustCompile(`\b(FR|NFR|REQ)-[A-Za-z0-9]+(-[A-Za-z0-9]+)*`),
	// Milestone tags: M4, M5.
	regexp.MustCompile(`\bM[0-9]+\b`),
	// Roadmap phases: "phase one", "Phase 2", "phase-two".
	regexp.MustCompile(`(?i)\bphase[ -]*(one|two|three|four|[0-9])\b`),
	// Design-doc cross-references: "design §3.2", "§4.5".
	regexp.MustCompile(`§\s*[0-9]`),
}

// TestShippedTextHasNoInternalReferences guards the rule in AGENTS.md
// ("User-Facing Copy"): text that ships carries no internal bookkeeping.
func TestShippedTextHasNoInternalReferences(t *testing.T) {
	root := filepath.Join("..", "..", "..")

	for _, artifact := range shippedText {
		t.Run(artifact.name, func(t *testing.T) {
			paths, err := filepath.Glob(filepath.Join(root, artifact.glob))
			if err != nil {
				t.Fatalf("glob %s: %v", artifact.glob, err)
			}
			// A guard that scans nothing passes forever; fail loudly instead.
			if len(paths) == 0 {
				t.Fatalf("no files matched %s -- has the artifact moved?", artifact.glob)
			}
			for _, path := range paths {
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read %s: %v", path, err)
				}
				for i, line := range strings.Split(string(raw), "\n") {
					for _, re := range internalRefs {
						if match := re.FindString(line); match != "" {
							rel, _ := filepath.Rel(root, path)
							t.Errorf("%s:%d: shipped text exposes %q\n  %s",
								rel, i+1, match, strings.TrimSpace(line))
						}
					}
				}
			}
		})
	}
}
