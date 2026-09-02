package framework

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

//go:embed testdata/cubestack-crds/*.yaml
var cubestackCRDs embed.FS

// The four ai.cubestack.io CRDs are vendored from the suanova/cubestack
// operator (operator/config/crd/bases) so the suite can provision them as a
// test precondition (issue #86). They are NOT part of the cubepilot platform
// deployment: in production the CubeStack operator provides them, so the test
// must be safe on a cluster that already has them.
//
// InstallCubestackCRDs provisions any of the four CubeStack CRDs that are
// ABSENT, and returns the names this call created. CRDs already present in the
// cluster are left untouched and are NOT returned (so cleanup never deletes
// something the test did not create). Existing-but-stale CRDs are used as-is —
// refresh the vendored copies with `make update-crds` for fresh clusters.
func (f *Framework) InstallCubestackCRDs(ctx context.Context) ([]string, error) {
	var created []string
	entries, err := fs.ReadDir(cubestackCRDs, "testdata/cubestack-crds")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		raw, err := cubestackCRDs.ReadFile("testdata/cubestack-crds/" + e.Name())
		if err != nil {
			return nil, err
		}
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := yaml.Unmarshal(raw, crd); err != nil {
			return nil, fmt.Errorf("decode %s: %w", e.Name(), err)
		}
		_, err = f.ApiExtClient.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, crd, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			continue // pre-existing: leave it, don't track for cleanup
		}
		if err != nil {
			return nil, fmt.Errorf("create %s: %w", crd.Name, err)
		}
		created = append(created, crd.Name)
	}
	return created, nil
}

// DeleteCubestackCRDs removes only the CRDs named in created (tolerating
// absence). CRDs that pre-existed the test are never deleted.
func (f *Framework) DeleteCubestackCRDs(ctx context.Context, created []string) error {
	for _, name := range created {
		err := f.ApiExtClient.ApiextensionsV1().CustomResourceDefinitions().
			Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete %s: %w", name, err)
		}
	}
	return nil
}
