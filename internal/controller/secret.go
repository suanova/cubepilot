package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ensureSecretData makes the named Secret hold exactly want's data, creating it
// when absent. Nothing is written when the data already matches, so a reconcile
// that changed nothing costs no API call.
//
// The whole data map is replaced rather than merged: a Secret written here is
// owned by the caller that declares it, and the two callers (the rendered
// gateway config, the per-user kubeconfig) each declare every key they expect.
// want's metadata is used as-is on create and left alone on update -- an
// existing Secret keeps the labels it was created with.
//
// A create that loses -- to a concurrent writer, or to a caller's client that
// has not observed a create that did land -- falls through to a read of the
// persisted object, so the desired data is written onto it instead of being
// dropped for a later reconcile to pick up. That read goes through reader,
// which must not be cache-backed: a cached client can miss the object on every
// read it makes, including this one, and then the write is lost after all. Pass
// the manager's API reader (mgr.GetAPIReader()) for it.
func ensureSecretData(ctx context.Context, cl client.Client, reader client.Reader, want *corev1.Secret) error {
	key := types.NamespacedName{Namespace: want.Namespace, Name: want.Name}
	var sec corev1.Secret
	err := cl.Get(ctx, key, &sec)
	switch {
	case apierrors.IsNotFound(err):
		if err := cl.Create(ctx, want); err == nil {
			return nil
		} else if !apierrors.IsAlreadyExists(err) {
			return err
		}
		if err := reader.Get(ctx, key, &sec); err != nil {
			return err
		}
	case err != nil:
		return err
	}
	if equality.Semantic.DeepEqual(sec.Data, want.Data) {
		return nil
	}
	sec.Data = want.Data
	return cl.Update(ctx, &sec)
}
