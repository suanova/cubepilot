// Package controller implements the CubePilot platform controllers: the
// AgentInstance controller (the Instance Manager, controller-based) and the
// builtin-resource bootstrap.
//
// The Instance Manager is controller-based -- AgentInstance CRD +
// controller-runtime. spec.runtime distinguishes multiple runtimes. Instances
// are resident: they stay up once started and are never idle-reclaimed, which
// is a fixed property of the platform rather than something the CR declares.
package controller

import (
	"context"
	"fmt"
	"log"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/k8s"
)

// finalizerName protects the instance's data directory PVC until the
// AgentInstance is fully removed (design §3.2 data-directory GC / reclaim).
const finalizerName = "ai.cubestack.io/agentinstance"

// modelConfiguredCondition reports whether the instance's AgentTemplate offers
// at least one usable model (non-empty endpoint, and a credential Secret when
// the model is keyed). It is deliberately decoupled from the pod lifecycle
// (issue #117): an instance can be Ready while no LLM is configured -- the
// Portal uses this condition to nudge the user toward Agent Config.
const (
	modelConfiguredCondition = "ModelConfigured"
	reasonModelConfigured    = "ModelConfigured"
	reasonNoModelConfigured  = "NoModelConfigured"
)

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
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

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
			fmt.Sprintf("runtime %q is not supported (only OpenClaw is available)", agent.Spec.Runtime))
	}

	// Model credential keys are delivered by the supervisor (it reads the
	// credential Secrets and writes them into the pod's emptyDir keys.json that
	// the gateway's file secret provider reads) -- no env injection here.

	// Ensure PVC / Service / Pod exist (provision + self-heal; the resident
	// policy is declared by the spec).
	spec := k8s.AgentSpec{
		Namespace:    r.Cfg.Namespace,
		Image:        r.Cfg.AgentImage,
		PullPolicy:   corev1.PullPolicy(r.Cfg.AgentImagePullPolicy),
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

	// The PVC/Pod/Service names are a pure function of the instance name (they
	// are bounded so a 253-character instance name cannot produce an invalid
	// name); both this path and the finalizer derive them through the same
	// helpers. The bound differs per kind: a PVC/Pod name is a DNS-1123
	// subdomain (253), while a Service name is a DNS-1035 label (63), so the
	// Service gets its own call.
	pvcName := k8s.GeneratedName("data", inst.Name)
	size := inst.EffectiveDataVolumeSize()
	podName := k8s.GeneratedName("agent", inst.Name)
	svcName := k8s.GeneratedServiceName("agent", inst.Name)

	// PVC (data directory; source of truth = instance data directory; design
	// §3.4 the platform holds zero agent data).
	pvc := spec.DataPVCFor(pvcName, inst.Name, size)
	// Service.
	svc := spec.ServiceFor(svcName, inst.Name, podName)
	// Pod.
	pod := spec.PodFor(podName, inst.Name, pvcName, svcName)
	// Record the mounted kubeconfig Secret revisions; the security fingerprint
	// compares this so a same-name Secret update recreates the Pod.
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[k8s.KubeconfigRevisionAnnotation] = kubeconfigRev

	// Bind the three generated objects to this instance before anything reads
	// or writes them. The names are derived from metadata.name -- a field the
	// writer of the AgentInstance chooses -- so a name is a selector the writer
	// controls, not evidence of ownership: without the binding, a writer could
	// point a hand-written instance at a name whose PVC/Pod/Service already
	// exist and have the controller adopt (ensurePVC), replace (ensurePod, on a
	// security-fingerprint mismatch) or delete (finalize) someone else's object.
	// The controller owner reference is the binding, and every ownership check
	// below compares it by UID -- the instance's UID is not writable, while its
	// name is.
	if err := r.setInstanceOwner(&inst, pvc, svc, pod); err != nil {
		return ctrl.Result{}, r.patchStatus(ctx, &inst, v1alpha1.InstanceFailed, "", "owner reference: "+err.Error())
	}

	if err := r.ensurePVC(ctx, pvc); err != nil {
		return ctrl.Result{}, r.patchStatus(ctx, &inst, v1alpha1.InstanceFailed, "", "pvc: "+err.Error())
	}
	if err := r.ensureService(ctx, svc); err != nil {
		return ctrl.Result{}, r.patchStatus(ctx, &inst, v1alpha1.InstanceFailed, "", "service: "+err.Error())
	}
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

	// Model availability is surfaced as a status condition that is decoupled
	// from the pod lifecycle (issue #117): the instance may be Ready while the
	// template has no usable model. The Portal keys its "go configure an LLM"
	// nudge off this condition. Re-evaluated on every reconcile; AgentTemplate /
	// Secret watches below re-trigger it when models or credentials change.
	prevConditions := append([]metav1.Condition(nil), inst.Status.Conditions...)
	hasModel, err := r.modelAvailable(ctx, agent)
	if err != nil {
		return ctrl.Result{}, err
	}
	cond := metav1.Condition{
		Type:               modelConfiguredCondition,
		ObservedGeneration: inst.Generation,
		LastTransitionTime: metav1.Now(),
	}
	if hasModel {
		cond.Status = metav1.ConditionTrue
		cond.Reason = reasonModelConfigured
	} else {
		cond.Status = metav1.ConditionFalse
		cond.Reason = reasonNoModelConfigured
		cond.Message = "no LLM configured - add one in Portal (Agent Config -> LLM Config)"
	}
	meta.SetStatusCondition(&inst.Status.Conditions, cond)

	// Update status only when it changed (avoid write amplification on the
	// periodic requeue).
	if inst.Status.Phase != status || inst.Status.PodName != podName ||
		inst.Status.PVCName != pvcName || inst.Status.ServiceName != svcName ||
		inst.Status.Message != message ||
		!equality.Semantic.DeepEqual(prevConditions, inst.Status.Conditions) {
		inst.Status.Phase = status
		inst.Status.PodName = podName
		inst.Status.PVCName = pvcName
		inst.Status.ServiceName = svcName
		inst.Status.Message = message
		inst.Status.ObservedGeneration = inst.Generation
		if err := r.Status().Update(ctx, &inst); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Failed pods are healed by re-creating them. Delete here and let the next
	// reconcile (requeue + deletion-completion event) create the replacement --
	// a same-name create while the old Pod terminates fails with AlreadyExists.
	if status == v1alpha1.InstanceFailed {
		if err := r.deletePod(ctx, pod); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	// Reconcile periodically to self-heal missing pods (resident self-heal).
	return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
}

// templateFor fetches the AgentTemplate definition by name (nil when missing:
// the builtin bootstrap creates cubepilot before instances, but a
// missing template must not crash the loop).
func (r *AgentInstanceReconciler) templateFor(ctx context.Context, name string) (*v1alpha1.AgentTemplate, error) {
	var tmpl v1alpha1.AgentTemplate
	if err := r.Get(ctx, types.NamespacedName{Namespace: r.Cfg.Namespace, Name: name}, &tmpl); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &tmpl, nil
}

// modelAvailable reports whether the instance's AgentTemplate offers at least
// one usable provider: a non-empty endpoint, and either no credentialRef (a
// public/keyless provider) or an existing credential Secret. A missing template
// or an empty provider list yields false (nothing to serve yet).
func (r *AgentInstanceReconciler) modelAvailable(ctx context.Context, agent *v1alpha1.AgentTemplate) (bool, error) {
	if agent == nil {
		return false, nil
	}
	for i := range agent.Spec.Providers {
		p := &agent.Spec.Providers[i]
		if p.Endpoint == "" || len(p.Models) == 0 {
			continue
		}
		if p.CredentialRef == nil || p.CredentialRef.Name == "" {
			return true, nil
		}
		var sec corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Namespace: r.Cfg.Namespace, Name: p.CredentialRef.Name}, &sec); err != nil {
			if apierrors.IsNotFound(err) {
				continue // keyed provider whose credential is not created yet
			}
			return false, err
		}
		return true, nil
	}
	return false, nil
}

// mapAllToInstances turns any watched AgentTemplate / credential Secret change
// into a reconcile of every AgentInstance, so the ModelConfigured condition is
// refreshed when models or their credential Secrets appear/disappear.
func (r *AgentInstanceReconciler) mapAllToInstances(_ context.Context, _ client.Object) []reconcile.Request {
	var list v1alpha1.AgentInstanceList
	if err := r.List(context.Background(), &list, client.InNamespace(r.Cfg.Namespace)); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: list.Items[i].Namespace, Name: list.Items[i].Name}})
	}
	return reqs
}

// finalize removes the instance's data directory PVC (the data directory is
// reclaimed when the instance is deleted), its Pod and its Service.
//
// The names are derived from the instance name, which the CR's writer chooses,
// so an object sitting under one of them is only removed when it is actually
// this instance's (see ownedByInstance). A name-identical object that is not
// ours is left alone and logged rather than reported as an error: a finalizer
// that returns an error blocks instance deletion forever, and a foreign object
// is not ours to delete.
func (r *AgentInstanceReconciler) finalize(ctx context.Context, inst *v1alpha1.AgentInstance) error {
	pvcName := k8s.GeneratedName("data", inst.Name)
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: r.Cfg.Namespace}}
	if err := r.deleteOwned(ctx, inst, "data pvc", pvc); err != nil {
		return fmt.Errorf("delete data pvc: %w", err)
	}
	podName := k8s.GeneratedName("agent", inst.Name)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: r.Cfg.Namespace}}
	if err := r.deleteOwned(ctx, inst, "agent pod", pod); err != nil {
		return fmt.Errorf("delete agent pod: %w", err)
	}
	// The Service is bounded to the (tighter) DNS-1035 label limit, so it is
	// not necessarily the pod name -- derive it exactly as the create path does.
	svcName := k8s.GeneratedServiceName("agent", inst.Name)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: svcName, Namespace: r.Cfg.Namespace}}
	if err := r.deleteOwned(ctx, inst, "agent service", svc); err != nil {
		return fmt.Errorf("delete agent service: %w", err)
	}
	log.Printf("controller: finalized instance %s (data pvc %s removed)", inst.Name, pvcName)
	return nil
}

// deleteOwned deletes obj -- identified by name/namespace -- only when it
// exists and is owned by inst. A missing object is a no-op; a name-identical
// object owned by someone else is skipped and logged (never an error: the
// caller is a finalizer, and failing on a foreign object would pin the
// instance in deletion forever).
//
// An object that was ours when it was read but changed before the delete landed
// is a different case and is NOT skipped: nothing has been verified about the
// object now at that name, so the delete is refused by the precondition (see
// deletePrecondition) and the error is returned. The caller then re-reads and
// re-vets it, which converges -- unlike the foreign case, this is not a
// permanent condition, so failing here cannot pin the instance.
func (r *AgentInstanceReconciler) deleteOwned(ctx context.Context, inst *v1alpha1.AgentInstance, kind string, obj client.Object) error {
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !ownedByInstance(obj, inst) {
		log.Printf("controller: %s: leaving %s %s alone: it is not owned by this instance (owner uid %q, instance uid %q)",
			inst.Name, kind, obj.GetName(), controllerOwnerUID(obj), inst.UID)
		return nil
	}
	if err := r.Delete(ctx, obj, deletePrecondition(obj)); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// deletePrecondition binds a Delete to the exact object a preceding Get
// returned, by passing that object's UID as a delete precondition. The API
// server answers 409 Conflict when the UID of the object it is about to delete
// no longer matches.
//
// Each delete below is a check-then-use pair: Get the object, verify its
// controller owner is this instance, then delete it -- by name. The name alone
// carries no identity, so an object replaced between the two steps (a different
// object, same name, different UID) would make the *unverified* replacement the
// one that gets deleted, while the ownership check that authorized the delete
// was made against an object that is no longer there. The precondition closes
// that window: the delete lands only while the object is still the one that was
// checked. It is additional to the ownership checks, not a replacement for them
// -- they decide whether to delete at all, it decides whether the object is
// still the one they decided about.
//
// A Conflict from a failed precondition is deliberately not swallowed anywhere
// (fail closed): the object at that name is no longer the object that was
// checked, so the delete must not carry it out, and the caller has to re-read
// and re-vet before acting. The callers' IsNotFound handling is unaffected --
// an object that is already gone satisfies the delete's intent.
func deletePrecondition(obj metav1.Object) client.DeleteOption {
	uid := obj.GetUID()
	return client.Preconditions{UID: &uid}
}

// setInstanceOwner makes inst the controller owner of each object, so the
// objects it creates carry the binding the ownership checks compare. It must
// run before the object is created: an object born unowned can never be
// adopted later (ensurePVC/ensurePod/ensureService refuse a foreign object).
//
// BlockOwnerDeletion is deliberately false. Setting it requires the operator to
// have `update` on the owner's `agentinstances/finalizers` subresource -- the
// OwnerReferencesPermissionEnforcement admission plugin rejects the create with
// "cannot set blockOwnerDeletion if an ownerReference refers to a resource you
// can't set finalizers on" (422) on clusters that enable it, and the chart's
// operator Role does not grant it. Nothing here needs it: blockOwnerDeletion
// only orders *foreground* deletion, while the instance's own finalizer already
// deletes these objects before it lets go of the CR.
func (r *AgentInstanceReconciler) setInstanceOwner(inst *v1alpha1.AgentInstance, objs ...client.Object) error {
	for _, obj := range objs {
		opts := []controllerutil.OwnerReferenceOption{controllerutil.WithBlockOwnerDeletion(false)}
		if err := controllerutil.SetControllerReference(inst, obj, r.Scheme, opts...); err != nil {
			return fmt.Errorf("set controller owner reference on %T %s: %w", obj, obj.GetName(), err)
		}
	}
	return nil
}

// controllerOwnerUID returns the UID of obj's controller owner reference, or
// "" when obj has none.
func controllerOwnerUID(obj metav1.Object) types.UID {
	if ref := metav1.GetControllerOf(obj); ref != nil {
		return ref.UID
	}
	return ""
}

// ownedBy reports whether obj was created by the AgentInstance with this UID.
//
// Ownership is decided by the controller owner reference's UID, never by the
// object's name: the name is what a writer of the CR selects, while the UID is
// assigned by the API server and cannot be set or re-used. An object with no
// controller owner reference (or one pointing at a different instance) is
// foreign -- and an empty UID owns nothing.
func ownedBy(uid types.UID, obj metav1.Object) bool {
	return uid != "" && controllerOwnerUID(obj) == uid
}

// ownedByInstance reports whether obj was created by this AgentInstance.
func ownedByInstance(obj metav1.Object, inst *v1alpha1.AgentInstance) bool {
	return ownedBy(inst.UID, obj)
}

func (r *AgentInstanceReconciler) patchStatus(ctx context.Context, inst *v1alpha1.AgentInstance, phase v1alpha1.InstancePhase, podName, message string) error {
	inst.Status.Phase = phase
	inst.Status.Message = message
	inst.Status.PodName = podName
	inst.Status.ObservedGeneration = inst.Generation
	return r.Status().Update(ctx, inst)
}

// ---- resource helpers (thin wrappers over k8s builders) ----

// ensurePVC creates the data PVC, and adopts an existing one only when it is
// this instance's. A same-named PVC belonging to someone else is not adopted
// (that would hand its contents to the instance, and later to finalize's
// delete): the reconcile fails closed instead, which surfaces as the instance
// going Failed with the reason.
func (r *AgentInstanceReconciler) ensurePVC(ctx context.Context, pvc *corev1.PersistentVolumeClaim) error {
	var existing corev1.PersistentVolumeClaim
	err := r.Get(ctx, types.NamespacedName{Name: pvc.Name, Namespace: pvc.Namespace}, &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, pvc)
	}
	if err != nil {
		return err
	}
	if !ownedBy(controllerOwnerUID(pvc), &existing) {
		return fmt.Errorf("persistentvolumeclaim %s already exists and is not owned by this AgentInstance (owner uid %q, expected %q): refusing to adopt an existing object",
			pvc.Name, controllerOwnerUID(&existing), controllerOwnerUID(pvc))
	}
	return nil
}

// ensureService is ensurePVC's Service counterpart: an existing same-named
// Service that is not this instance's is left untouched and fails the
// reconcile.
func (r *AgentInstanceReconciler) ensureService(ctx context.Context, svc *corev1.Service) error {
	var existing corev1.Service
	err := r.Get(ctx, types.NamespacedName{Name: svc.Name, Namespace: svc.Namespace}, &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, svc)
	}
	if err != nil {
		return err
	}
	if !ownedBy(controllerOwnerUID(svc), &existing) {
		return fmt.Errorf("service %s already exists and is not owned by this AgentInstance (owner uid %q, expected %q): refusing to adopt an existing object",
			svc.Name, controllerOwnerUID(&existing), controllerOwnerUID(svc))
	}
	return nil
}

// instanceSecurity captures the Pod spec fields that are both immutable after
// creation and part of the design §6 minimum-privilege baseline: identity,
// image (and its pull policy), security contexts, resource limits and
// secret-backed volumes (the kubeconfig mounts -- immutable and
// identity-bearing). Config-derived fields (other env, non-secret mounts,
// probes) are deliberately excluded: the supervisor applies config changes in
// place, so those must never trigger a Pod delete.
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
	ImagePullPolicy corev1.PullPolicy
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
		f.Containers[c.Name] = containerSecurity{Image: c.Image, ImagePullPolicy: c.ImagePullPolicy, SecurityContext: c.SecurityContext, Resources: c.Resources}
	}
	for _, c := range pod.Spec.InitContainers {
		f.InitContainers[c.Name] = containerSecurity{Image: c.Image, ImagePullPolicy: c.ImagePullPolicy, SecurityContext: c.SecurityContext, Resources: c.Resources}
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
// It only ever deletes the instance's own Pod: an existing same-named Pod that
// is not ours fails the reconcile instead (see the ownership check below).
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
	// The Pod's name is derived from the instance name, so a writer picks which
	// name this reconcile looks at -- and the drift path below *deletes* what it
	// finds there. Never delete a Pod that is not this instance's: fail closed
	// (the instance goes Failed) instead of replacing someone else's Pod. A
	// foreign Pod that happens to be terminating is not ours to wait on either,
	// hence the check precedes the DeletionTimestamp case.
	if !ownedBy(controllerOwnerUID(pod), &existing) {
		return false, fmt.Errorf("pod %s already exists and is not owned by this AgentInstance (owner uid %q, expected %q): refusing to replace an existing object",
			pod.Name, controllerOwnerUID(&existing), controllerOwnerUID(pod))
	}
	if existing.DeletionTimestamp != nil {
		// Already being deleted (e.g. a heal/drift delete still in progress):
		// report recreate so the caller requeues on the short interval and
		// creates the replacement once the old Pod is gone, rather than waiting
		// for the next periodic 60s requeue.
		return true, nil
	}
	if !equality.Semantic.DeepEqual(securityFingerprint(&existing), securityFingerprint(pod)) {
		// The delete carries the UID of the Pod that was just vetted, so a Pod
		// that replaced it in between is not the one deleted (see
		// deletePrecondition). A Conflict from that precondition is returned, not
		// folded into recreate=true: the replacement is an object nothing is
		// known about, so it must not be waited on as if it were ours being
		// deleted -- the caller fails the reconcile and the next one re-reads.
		if err := r.Delete(ctx, &existing, deletePrecondition(&existing)); err != nil && !apierrors.IsNotFound(err) {
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

// deletePod deletes the Pod named by desired, but only when it is the same
// instance's Pod that ensurePod just vetted. The name alone is not proof of
// ownership: it is derived from the instance name, so a writer can aim this
// delete at a foreign Pod. ensurePod already fails closed on a foreign object,
// which is why this second check cannot fire in practice -- it is here so the
// controller's delete-by-name paths all enforce the same rule.
func (r *AgentInstanceReconciler) deletePod(ctx context.Context, desired *corev1.Pod) error {
	existing, err := r.getPod(ctx, desired.Name)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !ownedBy(controllerOwnerUID(desired), existing) {
		return fmt.Errorf("pod %s is not owned by this AgentInstance (owner uid %q, expected %q): refusing to delete an existing object",
			desired.Name, controllerOwnerUID(existing), controllerOwnerUID(desired))
	}
	// The precondition binds the delete to the Pod that was just checked, so a
	// same-name replacement that landed in between is not deleted unverified
	// (see deletePrecondition); a Conflict is returned and the caller requeues.
	if err := r.Delete(ctx, existing, deletePrecondition(existing)); err != nil && !apierrors.IsNotFound(err) {
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

// SetupWithManager registers the reconciler with the given manager. It also
// watches AgentTemplates and Secrets so the ModelConfigured condition is
// refreshed as soon as models or their credential Secrets change.
func (r *AgentInstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.AgentInstance{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Watches(&v1alpha1.AgentTemplate{}, handler.EnqueueRequestsFromMapFunc(r.mapAllToInstances)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.mapAllToInstances)).
		Complete(r)
}
