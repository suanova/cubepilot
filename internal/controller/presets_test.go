package controller

import (
	"maps"
	"slices"
	"sort"
	"testing"
)

// TestBuiltinTaskTemplateNames pins the exact preset set, the way
// skill.TestBuiltinSkillNames does for the embedded skills.
//
// The file form introduces a failure the Go literals could not have: delete one
// YAML file and the build still compiles, still embeds the rest, and ships one
// preset fewer -- silently, because every other assertion in this package
// iterates whatever happened to load.
func TestBuiltinTaskTemplateNames(t *testing.T) {
	want := map[string]bool{
		"cluster-health-check":   true,
		"daily-inspection":       true,
		"gpu-inspection":         true,
		"inference-validation":   true,
		"model-deployment-check": true,
		"resource-analysis":      true,
	}
	presets, err := BuiltinTaskTemplates()
	if err != nil {
		t.Fatalf("load presets: %v", err)
	}

	got := map[string]bool{}
	names := make([]string, 0, len(presets))
	for _, tpl := range presets {
		if got[tpl.Name] {
			t.Errorf("preset %s appears twice", tpl.Name)
		}
		got[tpl.Name] = true
		names = append(names, tpl.Name)
	}
	if !maps.Equal(got, want) {
		t.Errorf("presets = %v, want %v", names, slices.Sorted(maps.Keys(want)))
	}
	// The catalog order is the file order, which is meant to be deterministic
	// without a second place to maintain an ordering.
	if !sort.StringsAreSorted(names) {
		t.Errorf("presets are not in name order: %v", names)
	}
}
