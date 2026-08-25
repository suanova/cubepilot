// Package controller implements the CubePilot platform controllers. The
// ModelReconciler maintains the platform LLM model catalog (design §3.3):
// external entries are connectivity-probed (endpoint + credentialRef) and
// their status.phase is set to Available / Unreachable; platform-provided
// entries are always Available. Templates and instances reference models by
// name, so a new model becomes selectable as soon as it is probed Available --
// without touching any running instance.
package controller

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/config"
)

// probeTimeout bounds each connectivity probe (external endpoints may hang).
const probeTimeout = 5 * time.Second

// probeClient is shared across reconciles: short timeout, no redirects, and
// insecure-skip-verify off by default (self-signed platform endpoints can
// opt in via the model label below).
var probeClient = &http.Client{
	Timeout: probeTimeout,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// ModelReconciler maintains Model availability (design §3.3): it probes
// external endpoints on registration and on a periodic requeue, and writes
// status.phase = Available / Unreachable.
type ModelReconciler struct {
	client.Client
	Cfg config.Config
}

// +kubebuilder:rbac:groups=assistant.suanova.io,resources=models,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=assistant.suanova.io,resources=models/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile probes one Model and updates its status. Provider semantics
// (design §3.3): platform = platform-managed inference (either a builtin
// runtime model -- no endpoint -- or a manually deployed endpoint the admin
// registered here); external = OpenAI-compatible endpoint with a
// platform-managed credential. Anything with an endpoint is probed the same
// way; only a builtin platform model with no endpoint is trusted Available.
func (r *ModelReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	var model v1alpha1.Model
	if err := r.Get(ctx, req.NamespacedName, &model); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	switch model.Spec.Provider {
	case "", v1alpha1.ModelProviderPlatform:
		// platform: endpoint empty = builtin runtime model (nothing to probe);
		// endpoint set = manually deployed inference service, probe it.
		if strings.TrimSpace(model.Spec.Endpoint) == "" {
			return ctrl.Result{RequeueAfter: 15 * time.Minute}, r.patchStatus(ctx, &model, v1alpha1.ModelAvailable, "platform-provided model (builtin)")
		}
		return r.probeEndpoint(ctx, &model)
	case v1alpha1.ModelProviderExternal:
		if strings.TrimSpace(model.Spec.Endpoint) == "" {
			return ctrl.Result{}, r.patchStatus(ctx, &model, v1alpha1.ModelUnreachable, "external provider requires endpoint")
		}
		return r.probeEndpoint(ctx, &model)
	default:
		return ctrl.Result{}, r.patchStatus(ctx, &model, v1alpha1.ModelUnreachable, fmt.Sprintf("unsupported provider %q", model.Spec.Provider))
	}
}

// probeEndpoint probes a model's OpenAI-compatible endpoint. A credentialRef
// is required for external providers; platform models may probe without one
// (endpoint without auth).
func (r *ModelReconciler) probeEndpoint(ctx context.Context, model *v1alpha1.Model) (ctrl.Result, error) {
	// Resolve the platform-managed credential if one is referenced (external
	// requires it; platform may omit it for unauthenticated endpoints).
	key := ""
	if model.Spec.CredentialRef != "" {
		k, err := r.secretKey(ctx, model.Spec.CredentialRef)
		if err != nil {
			return ctrl.Result{}, r.patchStatus(ctx, model, v1alpha1.ModelUnreachable, "credential: "+err.Error())
		}
		key = k
	} else if model.Spec.Provider == v1alpha1.ModelProviderExternal {
		return ctrl.Result{}, r.patchStatus(ctx, model, v1alpha1.ModelUnreachable, "external provider requires credentialRef")
	}

	skipTLS := model.Labels["cubepilot/skip-tls-verify"] == "true"
	if err := probeModelEndpoint(ctx, model.Spec.Endpoint, key, skipTLS); err != nil {
		return ctrl.Result{RequeueAfter: time.Minute}, r.patchStatus(ctx, model, v1alpha1.ModelUnreachable, err.Error())
	}
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, r.patchStatus(ctx, model, v1alpha1.ModelAvailable, "endpoint reachable")
}

// secretKey resolves credentialRef (namespace/name or name, defaulting to the
// platform namespace) and returns the Secret's apiKey value.
func (r *ModelReconciler) secretKey(ctx context.Context, ref string) (string, error) {
	ns, name := r.Cfg.Namespace, ref
	if i := strings.Index(ref, "/"); i >= 0 {
		ns, name = ref[:i], ref[i+1:]
	}
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &secret); err != nil {
		return "", err
	}
	key, ok := secret.Data["apiKey"]
	if !ok || len(key) == 0 {
		return "", fmt.Errorf("secret %s/%s has no apiKey key", ns, name)
	}
	return string(key), nil
}

// probeModelEndpoint checks that the OpenAI-compatible endpoint is reachable
// with the given bearer credential (GET {endpoint}/models). Any HTTP 2xx/3xx
// response counts as reachable; 401/403 means the credential is wrong and the
// model must not be offered.
func probeModelEndpoint(ctx context.Context, endpoint, apiKey string, skipTLS bool) error {
	base := strings.TrimRight(endpoint, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	if err != nil {
		return fmt.Errorf("probe request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	client := probeClient
	if skipTLS {
		client = &http.Client{
			Timeout:   probeTimeout,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, // #nosec G402 -- explicit opt-in via label
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("endpoint unreachable: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("endpoint probe returned %d (credential or endpoint problem)", resp.StatusCode)
	}
	return nil
}

func (r *ModelReconciler) patchStatus(ctx context.Context, model *v1alpha1.Model, phase v1alpha1.ModelPhase, message string) (err error) {
	if model.Status.Phase == phase && model.Status.Message == message {
		return nil // no write amplification on the periodic requeue
	}
	patch := client.MergeFrom(model.DeepCopy())
	model.Status.Phase = phase
	model.Status.Message = message
	now := metav1.Now()
	model.Status.LastProbeTime = &now
	model.Status.ObservedGeneration = model.Generation
	return r.Status().Patch(ctx, model, patch)
}

var _ reconcile.Reconciler = (*ModelReconciler)(nil)

// SetupWithManager registers the reconciler with the given manager.
func (r *ModelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("model").
		For(&v1alpha1.Model{}).
		Complete(r)
}
