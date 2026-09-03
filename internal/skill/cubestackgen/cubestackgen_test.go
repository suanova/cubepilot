package cubestackgen

import (
	"strings"
	"testing"
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
