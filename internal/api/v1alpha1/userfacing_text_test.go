package v1alpha1

import (
	"io/fs"
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
//   - the preset TaskTemplates are seeded as CRs, and their displayName,
//     description and instruction are what the Portal's Templates tab shows and
//     what every run of the task is prompted with;
//   - README.md is the repository's public front page;
//   - api.md and api-conventions.md are the contract for anyone integrating
//     against the API, and the bruno collection is the walkthrough they follow.
//
// This package's own doc comments are covered through the generated CRD YAML
// rather than directly: the YAML is what actually ships, and scanning it also
// catches the case that is easy to miss -- a comment on a comment-less
// struct-typed field or on a slice item type is published as the field's
// description.
//
// Deliberately not scanned:
//
//   - the generated internal/skill/skills/cubestack-platform/crd-reference.md,
//     which mirrors the vendored CubeStack CRDs whose descriptions this repo
//     does not author;
//   - docs/cubepilot/cubepilot-design.md, implementation-status.md, docs/notes/
//     and docs/superpowers/, which are working documents (design, status, review
//     notes, specs and plans) rather than pages a user of the product reads.
var shippedText = []struct {
	name      string
	glob      string
	recursive bool
}{
	{"crd-schemas", "config/crd/bases/*.yaml", false},
	{"chart-crd-schemas", "deploy/charts/cubepilot-chart/crds/*.yaml", false},
	{"chart-manifests", "deploy/charts/cubepilot-chart/templates/*.yaml", false},
	{"chart-values", "deploy/charts/cubepilot-chart/values.yaml", false},
	{"embedded-skills", "internal/skill/skills/*/SKILL.md", false},
	{"preset-task-templates", "internal/controller/presets/tasktemplates/*.yaml", false},
	{"readme", "README.md", false},
	{"api-doc", "docs/cubepilot/api.md", false},
	{"api-conventions", "docs/cubepilot/api-conventions.md", false},
	{"bruno-collection", "bruno", true},
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
			paths, err := artifactFiles(root, artifact.glob, artifact.recursive)
			if err != nil {
				t.Fatalf("collect %s: %v", artifact.glob, err)
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
				// Match against the whole file rather than line by line: the
				// patterns use \s, which Go's regexp lets span a newline, so a
				// reference wrapped across two lines would slip past a per-line
				// scan. The line number is derived from the match offset.
				text := string(raw)
				rel, _ := filepath.Rel(root, path)
				for _, re := range internalRefs {
					for _, loc := range re.FindAllStringIndex(text, -1) {
						lineNo, line := textLine(text, loc[0])
						t.Errorf("%s:%d: shipped text exposes %q\n  %s",
							rel, lineNo, text[loc[0]:loc[1]], line)
					}
				}
			}
		})
	}
}

// textLine returns the 1-based line number holding the byte offset and the text
// of that line, for reporting a match that may itself span lines.
func textLine(text string, offset int) (int, string) {
	lineNo := 1 + strings.Count(text[:offset], "\n")
	start := strings.LastIndex(text[:offset], "\n") + 1
	end := strings.Index(text[offset:], "\n")
	if end < 0 {
		end = len(text)
	} else {
		end += offset
	}
	return lineNo, strings.TrimSpace(text[start:end])
}

// artifactFiles resolves a shipped-text entry to files: a recursive walk of a
// directory, or a glob (which may resolve to a single literal path).
func artifactFiles(root, pattern string, recursive bool) ([]string, error) {
	target := filepath.Join(root, pattern)
	if !recursive {
		return filepath.Glob(target)
	}
	var paths []string
	err := filepath.WalkDir(target, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			paths = append(paths, path)
		}
		return nil
	})
	return paths, err
}
