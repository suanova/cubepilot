package controller

import (
	"context"
	"log"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/gateway"
	"github.com/suanova/cubepilot/internal/k8s"
)

// OpenClawConfigReconciler renders the shared openclaw.json from the
// AgentTemplate inline providers (+ referenced credential Secrets) and
// reconciles it into the openclaw-config Secret, preserving the gateway token
// (issue #6).
type OpenClawConfigReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Cfg    config.Config
}

// +kubebuilder:rbac:groups=ai.cubestack.io,resources=agenttemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch

// Reconcile renders and reconciles the openclaw-config Secret.
func (r *OpenClawConfigReconciler) Reconcile(ctx context.Context, _ reconcile.Request) (ctrl.Result, error) {
	var tpls v1alpha1.AgentTemplateList
	if err := r.List(ctx, &tpls, client.InNamespace(r.Cfg.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	// Every template is merged into one gateway config, so a provider name must
	// be unique across all of them -- +listMapKey=name only scopes the name to
	// its own template. Two providers named alike collide in the rendered
	// models.providers map (the later entry overwrites the earlier one) while the
	// allowlist keeps refs from both, which would route one template's model to
	// another template's endpoint and credential. So the merge is resolved here
	// instead: name order decides, the first provider to claim a name wins, and a
	// later provider with a taken name is skipped whole -- its models must not
	// reach the allowlist either, which skipping the provider achieves.
	templates := make([]*v1alpha1.AgentTemplate, 0, len(tpls.Items))
	for i := range tpls.Items {
		templates = append(templates, &tpls.Items[i])
	}
	sort.Slice(templates, func(i, j int) bool { return templates[i].Name < templates[j].Name })
	claimed := map[string]string{} // provider name -> the template that claimed it

	var providers []gateway.Provider
	var primary string
	for _, t := range templates {
		for _, pr := range t.Spec.Providers {
			if pr.Endpoint == "" || len(pr.Models) == 0 {
				continue
			}
			if owner, taken := claimed[pr.Name]; taken {
				log.Printf("openclaw-config: provider %q of agent template %q is skipped: the name is already taken by agent template %q", pr.Name, t.Name, owner)
				continue
			}
			p := gateway.Provider{Key: pr.Name, BaseURL: pr.Endpoint, Models: pr.Models}
			if pr.CredentialRef != nil && pr.CredentialRef.Name != "" {
				var sec corev1.Secret
				if err := r.Get(ctx, types.NamespacedName{Namespace: r.Cfg.Namespace, Name: pr.CredentialRef.Name}, &sec); err != nil {
					log.Printf("openclaw-config: provider %q credential %q not ready (%v), skipping", pr.Name, pr.CredentialRef.Name, err)
					continue
				}
				// Reference the credential by name only: the rendered config
				// carries a file SecretRef into the emptyDir keys.json the
				// supervisor writes from the Secret. The literal key never lands
				// in the config or the PVC.
				p.APIKey = k8s.EnvNameForProvider(pr.Name)
			}
			// Claimed only once the provider is really rendered: a provider
			// dropped above (missing credential) does not take the name away
			// from a later template that can serve it.
			claimed[pr.Name] = t.Name
			if primary == "" {
				for _, id := range pr.Models {
					if gateway.ModelKey(pr.Name, id) == t.Spec.DefaultModel {
						primary = t.Spec.DefaultModel
						break
					}
				}
			}
			providers = append(providers, p)
		}
	}
	// The Models guard is not redundant with the loop's skip: the skip makes it
	// unreachable today, but the field is a slice now, so an index without the
	// guard is a panic waiting for the next caller.
	if primary == "" && len(providers) > 0 && len(providers[0].Models) > 0 {
		primary = gateway.ModelKey(providers[0].Key, providers[0].Models[0])
	}

	token, err := gateway.EnsureGatewayToken(ctx, r.Client, r.Cfg.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	jsonBytes, err := gateway.Render(token, primary, providers)
	if err != nil {
		return ctrl.Result{}, err
	}

	key := types.NamespacedName{Namespace: r.Cfg.Namespace, Name: k8s.ConfigSecretName}
	var sec corev1.Secret
	err = r.Get(ctx, key, &sec)
	if apierrors.IsNotFound(err) {
		sec = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: k8s.ConfigSecretName, Namespace: r.Cfg.Namespace},
			Data:       map[string][]byte{"gatewayToken": []byte(token), "openclaw.json": jsonBytes},
		}
		return ctrl.Result{}, r.Create(ctx, &sec)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if string(sec.Data["openclaw.json"]) != string(jsonBytes) {
		sec.Data["openclaw.json"] = jsonBytes
		if err := r.Update(ctx, &sec); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler on AgentTemplate + Secret events.
func (r *OpenClawConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("openclaw-config").
		For(&v1alpha1.AgentTemplate{}).
		Watches(&corev1.Secret{}, &handler.EnqueueRequestForObject{}).
		Complete(r)
}
