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

// cubestackCRDNames are the four ai.cubestack.io CRDs vendored from the
// suanova/cubestack operator (operator/config/crd/bases) so the suite can
// install them deterministically (issue #86).
var cubestackCRDNames = []string{
	"devenvironments.ai.cubestack.io",
	"inferenceservices.ai.cubestack.io",
	"inferenceruntimeprofiles.ai.cubestack.io",
	"modelversions.ai.cubestack.io",
}

// InstallCubestackCRDs applies the four cubestack CRDs from the embedded
// testdata. Idempotent: an already-existing CRD is a no-op.
func (f *Framework) InstallCubestackCRDs(ctx context.Context) error {
	entries, err := fs.ReadDir(cubestackCRDs, "testdata/cubestack-crds")
	if err != nil {
		return err
	}
	for _, e := range entries {
		raw, err := cubestackCRDs.ReadFile("testdata/cubestack-crds/" + e.Name())
		if err != nil {
			return err
		}
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := yaml.Unmarshal(raw, crd); err != nil {
			return fmt.Errorf("decode %s: %w", e.Name(), err)
		}
		_, err = f.ApiExtClient.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, crd, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create %s: %w", crd.Name, err)
		}
	}
	return nil
}

// DeleteCubestackCRDs removes the four cubestack CRDs (tolerating absence).
func (f *Framework) DeleteCubestackCRDs(ctx context.Context) error {
	for _, name := range cubestackCRDNames {
		err := f.ApiExtClient.ApiextensionsV1().CustomResourceDefinitions().
			Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}
