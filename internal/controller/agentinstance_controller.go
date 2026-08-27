// Package controller implements the CubePilot platform controllers
// (design doc CubePilot-Cloud-for-Agents-Design.md §4.1): the AgentInstance
// controller (the Instance Manager, controller-based) and the builtin-resource
// bootstrap.
//
// Design §4.1: the Instance Manager is controller-based -- AgentInstance CRD +
// controller-runtime (v0.2 §13 chosen implementation); spec.runtime
// distinguishes multiple runtimes; resident and reclaim policies are declared
// by the CR spec.
package controller

import (
	"context"
	"fmt"
	"log"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/k8s"
)

// finalizerName protects the instance's data directory PVC until the
// AgentInstance is fully removed (design §3.2 data-directory GC / reclaim).
const finalizerName = "ai.cubestack.io/agentinstance"

// AgentInstanceReconciler reconciles AgentInstance objects: it ensures the
// per-user agent Pod + Service + data PVC exist and are healthy, and updates
// the instance status (phase / podName / pvcName / conditions). This is the
// controller-runtime incarnation of the Instance Manager (design §4.1).
type AgentInstanceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Cfg    config.Config
}

// +kubebuilder:rbac:groups=ai.cubestack.io,resources=agentinstances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ai.cubestack.io,resources=agentinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ai.cubestack.io,resources=agenttemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups=ai.cubestack.io,resources=skills,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods;services;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives one AgentInstance toward its desired state.
func (r *AgentInstanceReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	var inst v1alpha1.AgentInstance
	if err := r.Get(ctx, req.NamespacedName, &inst); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Deletion: run the finalizer (drop the data PVC) then release.
	if !inst.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&inst, finalizerName) {
			if err := r.finalize(ctx, &inst); err != nil {
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(&inst, finalizerName)
			if err := r.Update(ctx, &inst); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(&inst, finalizerName) {
		controllerutil.AddFinalizer(&inst, finalizerName)
		if err := r.Update(ctx, &inst); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Resolve the AgentTemplate definition.
	agent, err := r.templateFor(ctx, inst.Spec.TemplateRef)
	if err != nil {
		return ctrl.Result{}, r.patchStatus(ctx, &inst, v1alpha1.InstanceFailed, "", "template definition: "+err.Error())
	}

	// Runtime must be supported by this controller.
	if agent != nil && agent.Spec.Runtime != "" && agent.Spec.Runtime != v1alpha1.RuntimeOpenClaw {
		return ctrl.Result{}, r.patchStatus(ctx, &inst, v1alpha1.InstanceFailed, "",
			fmt.Sprintf("runtime %q not supported by phase-one controller", agent.Spec.Runtime))
	}

	// Ensure PVC / Service / Pod exist (provision + self-heal; the resident
	// policy is declared by the spec).
	spec := k8s.AgentSpec{
		Namespace:    r.Cfg.Namespace,
		Image:        r.Cfg.AgentImage,
		GatewayToken: r.Cfg.GatewayToken,
		Port:         int32(r.Cfg.AgentPort),
		AgentUser:    inst.Spec.Owner,
	}
	pvcName, size := inst.EffectiveDataVolume()
	podName := k8s.ResourceName("agent", inst.Name)
	svcName := podName

	// PVC (data directory; source of truth = instance data directory; design
	// §3.4 the platform holds zero agent data).
	pvc := spec.DataPVCFor(pvcName, inst.Name, size)
	if err := r.ensurePVC(ctx, pvc); err != nil {
		return ctrl.Result{}, r.patchStatus(ctx, &inst, v1alpha1.InstanceFailed, "", "pvc: "+err.Error())
	}
	// Service.
	svc := spec.ServiceFor(svcName, inst.Name, podName)
	if err := r.ensureService(ctx, svc); err != nil {
		return ctrl.Result{}, r.patchStatus(ctx, &inst, v1alpha1.InstanceFailed, "", "service: "+err.Error())
	}
	// Pod.
	pod := spec.PodFor(podName, inst.Name, pvcName, svcName)
	if err := r.ensurePod(ctx, pod); err != nil {
		return ctrl.Result{}, r.patchStatus(ctx, &inst, v1alpha1.InstanceFailed, "", "pod: "+err.Error())
	}

	// Observe the pod state for status.
	status := v1alpha1.InstanceCreating
	var message string
	switch current, err := r.getPod(ctx, podName); {
	case err == nil:
		switch {
		case podReady(current):
			status = v1alpha1.InstanceReady
			message = "instance ready"
		case isFailed(current):
			status = v1alpha1.InstanceFailed
			message = "pod failed; controller will heal"
		default:
			status = v1alpha1.InstanceCreating
			message = "provisioning"
		}
	case apierrors.IsNotFound(err):
		message = "pod not found; creating"
	default:
		return ctrl.Result{}, err
	}

	// Update status only when it changed (avoid write amplification on the
	// periodic requeue; LastActivity is refreshed on state transitions).
	if inst.Status.Phase != status || inst.Status.PodName != podName ||
		inst.Status.PVCName != pvcName || inst.Status.ServiceName != svcName ||
		inst.Status.Message != message {
		inst.Status.Phase = status
		inst.Status.PodName = podName
		inst.Status.PVCName = pvcName
		inst.Status.ServiceName = svcName
		inst.Status.Message = message
		inst.Status.ObservedGeneration = inst.Generation
		now := metav1.Now()
		inst.Status.LastActivity = &now
		if err := r.Status().Update(ctx, &inst); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Failed pods are healed by re-creating them; requeue to re-observe.
	if status == v1alpha1.InstanceFailed {
		if err := r.deletePod(ctx, podName); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.ensurePod(ctx, pod); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	// Reconcile periodically to self-heal missing pods (resident self-heal).
	return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
}

// templateFor fetches the AgentTemplate definition by name (nil when missing:
// the builtin bootstrap creates agent-for-cloud before instances, but a
// missing template must not crash the loop).
func (r *AgentInstanceReconciler) templateFor(ctx context.Context, name string) (*v1alpha1.AgentTemplate, error) {
	var tmpl v1alpha1.AgentTemplate
	if err := r.Get(ctx, types.NamespacedName{Name: name}, &tmpl); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &tmpl, nil
}

// finalize removes the instance's data directory PVC (the data directory is
// reclaimed when the instance is deleted).
func (r *AgentInstanceReconciler) finalize(ctx context.Context, inst *v1alpha1.AgentInstance) error {
	pvcName, _ := inst.EffectiveDataVolume()
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: r.Cfg.Namespace}}
	if err := r.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete data pvc: %w", err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: k8s.ResourceName("agent", inst.Name), Namespace: r.Cfg.Namespace}}
	if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete agent pod: %w", err)
	}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: k8s.ResourceName("agent", inst.Name), Namespace: r.Cfg.Namespace}}
	if err := r.Delete(ctx, svc); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete agent service: %w", err)
	}
	log.Printf("controller: finalized instance %s (data pvc %s removed)", inst.Name, pvcName)
	return nil
}

func (r *AgentInstanceReconciler) patchStatus(ctx context.Context, inst *v1alpha1.AgentInstance, phase v1alpha1.InstancePhase, podName, message string) error {
	inst.Status.Phase = phase
	inst.Status.Message = message
	inst.Status.PodName = podName
	inst.Status.ObservedGeneration = inst.Generation
	now := metav1.Now()
	inst.Status.LastActivity = &now
	return r.Status().Update(ctx, inst)
}

// ---- resource helpers (thin wrappers over k8s builders) ----

func (r *AgentInstanceReconciler) ensurePVC(ctx context.Context, pvc *corev1.PersistentVolumeClaim) error {
	var existing corev1.PersistentVolumeClaim
	err := r.Get(ctx, types.NamespacedName{Name: pvc.Name, Namespace: pvc.Namespace}, &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, pvc)
	}
	return err
}

func (r *AgentInstanceReconciler) ensureService(ctx context.Context, svc *corev1.Service) error {
	var existing corev1.Service
	err := r.Get(ctx, types.NamespacedName{Name: svc.Name, Namespace: svc.Namespace}, &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, svc)
	}
	return err
}

func (r *AgentInstanceReconciler) ensurePod(ctx context.Context, pod *corev1.Pod) error {
	var existing corev1.Pod
	err := r.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, pod)
	}
	return err
}

func (r *AgentInstanceReconciler) getPod(ctx context.Context, name string) (*corev1.Pod, error) {
	var pod corev1.Pod
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: r.Cfg.Namespace}, &pod)
	if err != nil {
		return nil, err
	}
	return &pod, nil
}

func (r *AgentInstanceReconciler) deletePod(ctx context.Context, name string) error {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Cfg.Namespace}}
	if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func isFailed(pod *corev1.Pod) bool {
	if pod.Status.Phase == corev1.PodFailed {
		return true
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if !cs.Ready && cs.RestartCount >= 3 {
			return true
		}
	}
	return false
}

// SetupWithManager registers the reconciler with the given manager.
func (r *AgentInstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.AgentInstance{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Complete(r)
}
