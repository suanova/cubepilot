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
	"k8s.io/apimachinery/pkg/api/equality"
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

	// Model credential keys are delivered by the supervisor (it reads the
	// credential Secrets and writes them into the pod's emptyDir keys.json that
	// the gateway's file secret provider reads) -- no env injection here.

	// Ensure PVC / Service / Pod exist (provision + self-heal; the resident
	// policy is declared by the spec).
	spec := k8s.AgentSpec{
		Namespace:    r.Cfg.Namespace,
		Image:        r.Cfg.AgentImage,
		GatewayToken: r.Cfg.GatewayToken,
		Port:         int32(r.Cfg.AgentPort),
		AgentUser:    inst.Spec.Owner,
	}
	// Dual-kubeconfig (design §5.3 / issue #19 Option B): the agent's default
	// kubectl must run with the USER's own credentials, so the per-user
	// kubeconfig Secret is a hard prerequisite. It is minted asynchronously by
	// the builtin bootstrap (SA token -> kubeconfig Secret). While it is absent
	// we requeue instead of provisioning the Pod with the shared agent-kubeconfig
	// (SA) as its default: a Pod born with a placeholder identity would be
	// recreated once the user Secret lands, restarting a freshly Ready agent and
	// cutting in-flight chats (issue #100). A non-NotFound lookup error is
	// transient or an RBAC gap and must requeue, not silently degrade.
	userSecretName := k8s.UserKubeconfigSecretFor(inst.Spec.Owner)
	var userSecret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: userSecretName, Namespace: r.Cfg.Namespace}, &userSecret); err != nil {
		if apierrors.IsNotFound(err) {
			log.Printf("controller: %s: per-user kubeconfig secret %s not provisioned yet; waiting for identity before creating the pod", inst.Name, userSecretName)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		return ctrl.Result{}, fmt.Errorf("lookup per-user kubeconfig secret %s: %w", userSecretName, err)
	}
	spec.UserKubeconfigSecret = userSecretName
	// The platform (discovery) kubeconfig Secret is provisioned by setup.sh /
	// chart; its resourceVersion lets an in-place content change (credential
	// rotation, same name) recreate the Pod -- SubPath mounts do not refresh.
	var platformSecret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: k8s.KubeconfigSecretName, Namespace: r.Cfg.Namespace}, &platformSecret); err != nil {
		return ctrl.Result{}, fmt.Errorf("lookup platform kubeconfig secret %s: %w", k8s.KubeconfigSecretName, err)
	}
	kubeconfigRev := userSecretName + "@" + userSecret.ResourceVersion + "|" + k8s.KubeconfigSecretName + "@" + platformSecret.ResourceVersion

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
	// Record the mounted kubeconfig Secret revisions; the security fingerprint
	// compares this so a same-name Secret update recreates the Pod.
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[k8s.KubeconfigRevisionAnnotation] = kubeconfigRev
	recreate, err := r.ensurePod(ctx, pod)
	if err != nil {
		return ctrl.Result{}, r.patchStatus(ctx, &inst, v1alpha1.InstanceFailed, "", "pod: "+err.Error())
	}
	if recreate {
		// Drift delete in flight; never create into a terminating pod. Requeue
		// shortly -- the deletion-completion watch event also re-triggers -- and
		// the next reconcile creates the replacement.
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
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

	// Failed pods are healed by re-creating them. Delete here and let the next
	// reconcile (requeue + deletion-completion event) create the replacement --
	// a same-name create while the old Pod terminates fails with AlreadyExists.
	if status == v1alpha1.InstanceFailed {
		if err := r.deletePod(ctx, podName); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
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

// instanceSecurity captures the Pod spec fields that are both immutable after
// creation and part of the design §6 minimum-privilege baseline: identity,
// image, security contexts, resource limits and secret-backed volumes (the
// kubeconfig mounts -- immutable and identity-bearing). Config-derived fields
// (other env, non-secret mounts, probes) are deliberately excluded: the
// supervisor applies config changes in place, so those must never trigger a
// Pod delete.
type instanceSecurity struct {
	ServiceAccountName string
	PodSecurityContext *corev1.PodSecurityContext
	Containers         map[string]containerSecurity
	InitContainers     map[string]containerSecurity
	SecretVolumes      map[string]string
	// KubeconfigRevision is the agent Pod's kubeconfig Secret resourceVersion
	// digest (annotation), so an in-place Secret content change recreates the
	// Pod even though the Secret name is unchanged (SubPath mounts do not
	// refresh).
	KubeconfigRevision string
}

type containerSecurity struct {
	Image           string
	SecurityContext *corev1.SecurityContext
	Resources       corev1.ResourceRequirements
}

// securityFingerprint extracts the immutable security subset of a Pod spec for
// drift comparison (see instanceSecurity).
func securityFingerprint(pod *corev1.Pod) instanceSecurity {
	f := instanceSecurity{
		ServiceAccountName: pod.Spec.ServiceAccountName,
		PodSecurityContext: pod.Spec.SecurityContext,
		Containers:         map[string]containerSecurity{},
		InitContainers:     map[string]containerSecurity{},
		SecretVolumes:      map[string]string{},
		KubeconfigRevision: pod.Annotations[k8s.KubeconfigRevisionAnnotation],
	}
	for _, c := range pod.Spec.Containers {
		f.Containers[c.Name] = containerSecurity{Image: c.Image, SecurityContext: c.SecurityContext, Resources: c.Resources}
	}
	for _, c := range pod.Spec.InitContainers {
		f.InitContainers[c.Name] = containerSecurity{Image: c.Image, SecurityContext: c.SecurityContext, Resources: c.Resources}
	}
	for _, v := range pod.Spec.Volumes {
		if v.Secret != nil {
			f.SecretVolumes[v.Name] = v.Secret.SecretName
		}
	}
	return f
}

// ensurePod creates a missing Pod, and deletes an existing one whose immutable
// security spec drifted (e.g. the operator was upgraded with a stricter design
// §6 baseline, or a mounted kubeconfig Secret rotated) so it converges --
// mirroring the failed-Pod healing path. Only the security fingerprint is
// compared, so a config change never deletes the Pod (the supervisor reloads in
// place; the Pod and its sessions/PVC/IP must survive).
//
// A drifted Pod is deleted but NOT re-created here: the returned recreate flag
// tells the caller to requeue. Re-creating in the same reconcile races the
// asynchronous deletion -- a same-name create while the old Pod is terminating
// fails with AlreadyExists, which used to strand the instance in a Failed heal
// loop ("pod: object is being deleted ... already exists"). The deletion
// completion event plus the requeue bring the replacement up on a later
// reconcile.
func (r *AgentInstanceReconciler) ensurePod(ctx context.Context, pod *corev1.Pod) (recreate bool, err error) {
	var existing corev1.Pod
	err = r.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, &existing)
	if apierrors.IsNotFound(err) {
		return false, r.Create(ctx, pod)
	}
	if err != nil {
		return false, err
	}
	if existing.DeletionTimestamp != nil {
		// Already being deleted (e.g. a heal/drift delete still in progress):
		// report recreate so the caller requeues on the short interval and
		// creates the replacement once the old Pod is gone, rather than waiting
		// for the next periodic 60s requeue.
		return true, nil
	}
	if !equality.Semantic.DeepEqual(securityFingerprint(&existing), securityFingerprint(pod)) {
		if err := r.Delete(ctx, &existing); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
		return true, nil
	}
	return false, nil
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
