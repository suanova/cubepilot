package cubestackgen

import (
	"fmt"
	"os"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// testCRDDir points from the package dir up to the vendored CubeStack CRDs.
const testCRDDir = "../../../test/e2e/framework/testdata/cubestack-crds"

func TestRenderDocCoversAllKinds(t *testing.T) {
	doc, err := RenderDocFromDir(testCRDDir)
	if err != nil {
		t.Fatalf("RenderDocFromDir: %v", err)
	}
	for _, kind := range []string{"DevEnvironment", "InferenceService", "InferenceRuntimeProfile", "ModelVersion"} {
		if !strings.Contains(doc, "## "+kind+"\n") {
			t.Errorf("render should have a section for %s", kind)
		}
	}
}

func TestRenderDevEnvironmentEssentials(t *testing.T) {
	doc, err := RenderDocFromDir(testCRDDir)
	if err != nil {
		t.Fatalf("RenderDocFromDir: %v", err)
	}
	for _, want := range []string{
		"- resource `devenvironments.ai.cubestack.io`, apiVersion `ai.cubestack.io/v1alpha1`, scope Namespaced",
		"- spec requires: image, resources",
		"- `spec.image` — string · required",
		"- `spec.resources` — object · required",
		"- `spec.resources.cpu` — string",
		"- `spec.resources.gpuCount` — integer (int32) · default: 1 · min 1",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("render should contain %q", want)
		}
	}
}

func TestCommittedCRDReferenceIsFresh(t *testing.T) {
	committed := "../../../internal/skill/skills/cubestack-platform/crd-reference.md"
	want, err := RenderDocFromDir(testCRDDir)
	if err != nil {
		t.Fatalf("RenderDocFromDir: %v", err)
	}
	got, err := os.ReadFile(committed)
	if err != nil {
		t.Fatalf("read committed crd-reference.md: %v (run make update-cubestack-skill)", err)
	}
	if string(got) != want {
		t.Fatalf("committed crd-reference.md is stale\nrun: make update-cubestack-skill")
	}
}

func TestSKILLDevEnvironmentExampleIsValid(t *testing.T) {
	skill, err := os.ReadFile("../../../internal/skill/skills/cubestack-platform/SKILL.md")
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}
	raw, err := extractFencedYAMLContaining(string(skill), "kind: DevEnvironment")
	if err != nil {
		t.Fatalf("extract example: %v", err)
	}
	var obj map[string]any
	if err := yaml.Unmarshal([]byte(raw), &obj); err != nil {
		t.Fatalf("decode example: %v", err)
	}
	crds, err := LoadDir(testCRDDir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	var specSchema apiextensionsv1.JSONSchemaProps
	for _, crd := range crds {
		if crd.Spec.Names.Kind != "DevEnvironment" {
			continue
		}
		specSchema = crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	}
	spec, ok := obj["spec"].(map[string]any)
	if !ok {
		t.Fatalf("example must carry a spec object")
	}
	if errs := validateRequiredAndEnums(specSchema, spec, "spec"); len(errs) > 0 {
		t.Fatalf("example violates the DevEnvironment CRD:\n  %s", strings.Join(errs, "\n  "))
	}
}

// extractFencedYAMLContaining returns the first fenced ``` block whose body
// contains needle (used to locate the known-good example in SKILL.md).
func extractFencedYAMLContaining(md, needle string) (string, error) {
	lines := strings.Split(md, "\n")
	in := false
	var cur []string
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if !in {
				in = true
				cur = nil
				continue
			}
			if text := strings.Join(cur, "\n"); strings.Contains(text, needle) {
				return text, nil
			}
			in = false
			continue
		}
		if in {
			cur = append(cur, line)
		}
	}
	return "", fmt.Errorf("no fenced block containing %q found", needle)
}

// validateRequiredAndEnums checks that every field the schema marks required is
// present, and that any provided leaf matching an enum-valued schema field is
// one of the enum entries. Recurses into nested object values.
func validateRequiredAndEnums(schema apiextensionsv1.JSONSchemaProps, obj map[string]any, path string) []string {
	var errs []string
	for _, req := range schema.Required {
		if _, ok := obj[req]; !ok {
			errs = append(errs, fmt.Sprintf("%s: missing required field %q", path, req))
		}
	}
	for name, val := range obj {
		prop, ok := schema.Properties[name]
		if !ok {
			continue
		}
		if len(prop.Enum) > 0 {
			vs := fmt.Sprint(val)
			legal := false
			for _, e := range prop.Enum {
				if trimRaw(e.Raw) == vs {
					legal = true
					break
				}
			}
			if !legal {
				errs = append(errs, fmt.Sprintf("%s.%s: value %q not in enum %v", path, name, vs, prop.Enum))
			}
		}
		if child, ok := val.(map[string]any); ok {
			errs = append(errs, validateRequiredAndEnums(prop, child, path+"."+name)...)
		}
	}
	return errs
}
