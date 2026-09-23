package controller

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
)

// taskTemplateDir holds the preset task templates as data: one YAML file per
// preset, named after the CR it becomes. The embedded preset Skills under
// internal/skill/skills are maintained the same way, so the two kinds of preset
// in this repo have one shape rather than two.
const taskTemplateDir = "presets/tasktemplates"

// taskTemplateFS embeds the preset directory. `all:` is required for a
// recursive match, and an empty match is a build error -- which is the point:
// a preset that is not embedded does not silently vanish, the build breaks.
//
//go:embed all:presets/tasktemplates/*.yaml
var taskTemplateFS embed.FS

// BuiltinTaskTemplates returns every preset task template, in the order the
// Portal's Templates tab lists them: the file order, which is the CR name's
// alphabetical order. Sorting is not a preference -- it is what makes the list
// deterministic without a second place to maintain an ordering.
//
// An upgrade pre-check is deliberately not among them: it needs cluster-scoped
// reads the agent's identity used to lack (nodes, CRDs) plus a target release to
// check against, so shipping it as a preset would only produce runs that report
// Forbidden.
//
// The presets are starting points rather than a closed set -- an operator can add
// TaskTemplate CRs beside them, and a preset is only ever created when it is
// missing.
func BuiltinTaskTemplates() ([]*v1alpha1.TaskTemplate, error) {
	entries, err := fs.ReadDir(taskTemplateFS, taskTemplateDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", taskTemplateDir, err)
	}
	files := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			files = append(files, e.Name())
		}
	}
	if len(files) == 0 {
		// The go:embed pattern above would have failed the build, so reaching
		// this means the directory moved. Fail rather than seed nothing.
		return nil, fmt.Errorf("no task templates embedded from %s", taskTemplateDir)
	}
	sort.Strings(files)

	out := make([]*v1alpha1.TaskTemplate, 0, len(files))
	for _, file := range files {
		raw, err := taskTemplateFS.ReadFile(path.Join(taskTemplateDir, file))
		if err != nil {
			return nil, err
		}
		tpl := &v1alpha1.TaskTemplate{}
		// Strict, and that is load-bearing: an unknown key is an error rather
		// than a silently dropped field. It is what the Go literals this
		// replaced got from the compiler, and it catches both directions of a
		// rename -- the type renamed and the file not, or the reverse. It also
		// rejects a key written twice, which plain YAML resolves last-one-wins
		// without a word.
		if err := yaml.UnmarshalStrict(raw, tpl); err != nil {
			return nil, fmt.Errorf("parse %s/%s: %w", taskTemplateDir, file, err)
		}
		// The file name is the CR name. Checked rather than derived so a copy
		// renamed on one side only is an error, not a preset that overwrites its
		// neighbour.
		if want := strings.TrimSuffix(file, ".yaml"); tpl.Name != want {
			return nil, fmt.Errorf("%s/%s: metadata.name is %q, want %q", taskTemplateDir, file, tpl.Name, want)
		}
		// Labels here, not in the files: "came from the preset directory" is a
		// property of the file's location, so it belongs in one place rather
		// than repeated per file where it could be got wrong or drift apart.
		tpl.Labels = map[string]string{
			"app.kubernetes.io/part-of": "cubepilot",
			"cubepilot/builtin":         "true",
		}
		out = append(out, tpl)
	}
	return out, nil
}
