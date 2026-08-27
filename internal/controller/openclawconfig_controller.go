package controller

import (
	"context"
	"log"

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
// AgentTemplate inline models (+ referenced credential Secrets) and reconciles
// it into the openclaw-config Secret, preserving the gateway token (issue #6).
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
	if err := r.List(ctx, &tpls); err != nil {
		return ctrl.Result{}, err
	}
	var providers []gateway.Provider
	var primary string
	for i := range tpls.Items {
		t := &tpls.Items[i]
		for _, m := range t.Spec.Models {
			if m.Endpoint == "" {
				continue
			}
			p := gateway.Provider{Key: m.Name, BaseURL: m.Endpoint, Model: m.Name}
			if m.CredentialRef != nil && m.CredentialRef.Name != "" {
				var sec corev1.Secret
				if err := r.Get(ctx, types.NamespacedName{Namespace: r.Cfg.Namespace, Name: m.CredentialRef.Name}, &sec); err != nil {
					log.Printf("openclaw-config: model %q credential %q not ready (%v), skipping", m.Name, m.CredentialRef.Name, err)
					continue
				}
				p.APIKey = string(sec.Data["apiKey"])
			}
			if t.Spec.DefaultModel == m.Name && primary == "" {
				primary = m.Name + "/" + m.Name
			}
			providers = append(providers, p)
		}
	}
	if primary == "" && len(providers) > 0 {
		primary = providers[0].Key + "/" + providers[0].Model
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
