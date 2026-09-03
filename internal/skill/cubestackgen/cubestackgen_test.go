package cubestackgen

import (
	"fmt"
	"os"
	"path/filepath"
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

// TestLoadDirRejectsSchemaLessCRD verifies LoadDir returns a file-specific
// error (not a panic) for a CRD lacking versions[0]'s OpenAPI schema.
func TestLoadDirRejectsSchemaLessCRD(t *testing.T) {
	dir := t.TempDir()
	bad := "apiVersion: apiextensions.k8s.io/v1\n" +
		"kind: CustomResourceDefinition\n" +
		"metadata:\n  name: no.schema.io\n" +
		"spec:\n  group: no.schema.io\n  names:\n    kind: NoSchema\n    plural: noschemas\n"
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte(bad), 0o644); err != nil {
		t.Fatalf("write bad CRD: %v", err)
	}
	if _, err := LoadDir(dir); err == nil {
		t.Fatal("expected an error for a CRD without a versions[0] OpenAPI schema")
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
	found := false
	for _, crd := range crds {
		if crd.Spec.Names.Kind != "DevEnvironment" {
			continue
		}
		specSchema = crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
		found = true
	}
	if !found {
		t.Fatalf("no DevEnvironment CRD under %s; the example cannot be validated", testCRDDir)
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

// scalarTypeMismatch reports whether a provided scalar value disagrees with the
// schema leaf type (string / boolean / integer). JSON/YAML numbers decode into
// float64; an integer-typed leaf must hold an integral value.
func scalarTypeMismatch(prop apiextensionsv1.JSONSchemaProps, val any) bool {
	switch prop.Type {
	case "string":
		_, ok := val.(string)
		return !ok
	case "boolean":
		_, ok := val.(bool)
		return !ok
	case "integer":
		f, ok := val.(float64)
		return !ok || f != float64(int64(f))
	case "number":
		_, ok := val.(float64)
		return !ok
	}
	return false
}

// validateRequiredAndEnums checks that every field the schema marks required is
// present, that any provided scalar matches the schema leaf type, and that any
// provided leaf matching an enum-valued schema field is one of the enum
// entries. Recurses into nested object values.
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
		if child, ok := val.(map[string]any); ok {
			errs = append(errs, validateRequiredAndEnums(prop, child, path+"."+name)...)
			continue
		}
		if prop.Type != "object" && prop.Type != "array" && prop.Type != "" && scalarTypeMismatch(prop, val) {
			errs = append(errs, fmt.Sprintf("%s.%s: value of type %T is not schema type %q", path, name, val, prop.Type))
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
	}
	return errs
}
