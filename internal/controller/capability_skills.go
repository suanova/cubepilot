package controller

import (
	"context"
	"fmt"
	"log"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/k8s"
)

// skillsMountPath is where the ConfigMap volume is mounted inside the
// sync-capability-skills init container.
const skillsMountPath = "/capability-skills"

// CapabilitySkillsReconciler keeps the capability-skills ConfigMap in sync
// with the Capability CRDs and restarts agent Pods when the rendered skills
// change (OpenClaw loads skills at gateway startup, so a rollout is required
// for new/updated capabilities to take effect).
type CapabilitySkillsReconciler struct {
	client.Client
	Cfg config.Config
}

// +kubebuilder:rbac:groups=assistant.suanova.io,resources=capabilities,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete

// Reconcile renders all capabilities and reconciles the ConfigMap, then
// restarts agent Pods if the rendered content changed.
func (r *CapabilitySkillsReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	if err := r.sync(ctx); err != nil {
		log.Printf("capability-skills: %v", err)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// sync renders the capability catalog into the ConfigMap and, on change,
// deletes all agent Pods so they restart with the new skills.
func (r *CapabilitySkillsReconciler) sync(ctx context.Context) error {
	var caps v1alpha1.CapabilityList
	if err := r.List(ctx, &caps); err != nil {
		return fmt.Errorf("list capabilities: %w", err)
	}

	data := map[string]string{}
	for i := range caps.Items {
		cap := &caps.Items[i]
		if cap.Spec.Type != v1alpha1.CapabilityDomain {
			continue // atomic capabilities are platform overlays, not skills
		}
		skill, err := RenderSkill(cap)
		if err != nil {
			return fmt.Errorf("render %s: %w", cap.Name, err)
		}
		data[cap.Name] = skill
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      k8s.SkillsConfigMapName,
			Namespace: r.Cfg.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/part-of": "cubepilot",
				"cubepilot/rendered":        "capabilities",
			},
		},
		Data: data,
	}

	var existing corev1.ConfigMap
	err := r.Get(ctx, client.ObjectKey{Name: k8s.SkillsConfigMapName, Namespace: r.Cfg.Namespace}, &existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := r.Create(ctx, cm); err != nil {
			return fmt.Errorf("create %s: %w", k8s.SkillsConfigMapName, err)
		}
		log.Printf("capability-skills: created %s (%d skills)", k8s.SkillsConfigMapName, len(data))
		return r.restartAgentPods(ctx)
	case err != nil:
		return err
	}

	if reflect.DeepEqual(existing.Data, data) {
		return nil // no change — skills are current
	}
	existing.Data = data
	if err := r.Update(ctx, &existing); err != nil {
		return fmt.Errorf("update %s: %w", k8s.SkillsConfigMapName, err)
	}
	log.Printf("capability-skills: updated %s (%d skills)", k8s.SkillsConfigMapName, len(data))
	return r.restartAgentPods(ctx)
}

// restartAgentPods deletes all agent Pods so the AgentInstance controller
// recreates them with the fresh skills (OpenClaw loads skills at startup).
func (r *CapabilitySkillsReconciler) restartAgentPods(ctx context.Context) error {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(r.Cfg.Namespace), client.MatchingLabels{k8s.AgentLabelApp: "true"}); err != nil {
		return fmt.Errorf("list agent pods: %w", err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("restart pod %s: %w", pod.Name, err)
		}
	}
	if len(pods.Items) > 0 {
		log.Printf("capability-skills: restarted %d agent pod(s) to load new skills", len(pods.Items))
	}
	return nil
}

// RenderSkill converts a domain Capability into an OpenClaw SKILL.md. The
// skill name equals the Capability name so platform accounting and runtime
// skills share one identity (design §3.3.1). The body is the capability's
// instructions; the description feeds the frontmatter.
func RenderSkill(cap *v1alpha1.Capability) (string, error) {
	if cap.Name == "" {
		return "", fmt.Errorf("capability has no name")
	}
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "name: %s\n", cap.Name)
	if cap.Spec.Description != "" {
		fmt.Fprintf(&b, "description: %s\n", cap.Spec.Description)
	}
	if len(cap.Spec.Uses) > 0 {
		fmt.Fprintf(&b, "metadata:\n  openclaw:\n    requires:\n")
		for _, u := range cap.Spec.Uses {
			fmt.Fprintf(&b, "      - %s\n", u)
		}
	}
	b.WriteString("---\n\n")
	if cap.Spec.Title != "" {
		fmt.Fprintf(&b, "# %s\n\n", cap.Spec.Title)
	}
	if cap.Spec.Instructions != "" {
		b.WriteString(strings.TrimSpace(cap.Spec.Instructions))
		b.WriteString("\n")
	}
	return b.String(), nil
}

// SetupWithManager registers the capability-skills reconciler.
func (r *CapabilitySkillsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("capability-skills").
		For(&v1alpha1.Capability{}).
		Complete(r)
}
