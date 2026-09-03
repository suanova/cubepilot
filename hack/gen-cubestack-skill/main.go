// Command gen-cubestack-skill regenerates the builtin cubestack-platform
// skill's crd-reference.md from the vendored CubeStack CRD YAMLs. Wired to
// `make update-cubestack-skill`; the committed output is guarded by the
// freshness test in internal/skill/cubestackgen.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/suanova/cubepilot/internal/skill/cubestackgen"
)

func main() {
	crdDir := flag.String("crd-dir", "test/e2e/framework/testdata/cubestack-crds",
		"directory holding the vendored CubeStack CRD YAMLs")
	out := flag.String("out", "internal/skill/skills/cubestack-platform/crd-reference.md",
		"path to write the generated crd-reference.md")
	flag.Parse()

	doc, err := cubestackgen.RenderDocFromDir(*crdDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gen-cubestack-skill: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, []byte(doc), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "gen-cubestack-skill: write %s: %v\n", *out, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d bytes)\n", *out, len(doc))
}
