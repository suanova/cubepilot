package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/k8s"
)

const (
	testInstanceName = "zhang-wei-cubepilot"
	testPodName      = "agent-zhang-wei-cubepilot"
	testPVCName      = "data-zhang-wei-cubepilot"
	testNamespace    = "cubepilot"

	// testInstanceUID is the API-server-assigned identity the controller's
	// ownership checks compare generated objects against. The fake client keeps
	// whatever UID an object is seeded with, so resources marked with
	// ownByTestInstance are recognized as this instance's own.
	testInstanceUID = types.UID("0f8f0d5a-4a0e-4f4e-9d6a-1f2c3b4a5c6d")
)

func testAgentCfg() config.Config {
	return config.Config{
		Namespace:    testNamespace,
		AgentImage:   "harbor.isuanova.com/suanova/cubepilot-openclaw:test",
		GatewayToken: "test-gateway-token",
		AgentPort:    18789,
	}
}

func testTemplate() *v1alpha1.AgentTemplate {
	return &v1alpha1.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "cubepilot", Namespace: testNamespace},
		Spec:       v1alpha1.AgentTemplateSpec{Runtime: v1alpha1.RuntimeOpenClaw},
	}
}

func testInstance() *v1alpha1.AgentInstance {
	return &v1alpha1.AgentInstance{
		ObjectMeta: metav1.ObjectMeta{Name: testInstanceName, UID: testInstanceUID},
		Spec: v1alpha1.AgentInstanceSpec{
			TemplateRef: "cubepilot",
			Owner:       "zhang.wei",
		},
	}
}

// ownByTestInstance marks objs as owned by the test instance the way Reconcile
// binds the resources it creates: a controller owner reference to the
// AgentInstance, with BlockOwnerDeletion left false (see
// AgentInstanceReconciler.setInstanceOwner). A resource seeded without it is
// foreign -- which is what the ownership tests deliberately exercise.
func ownByTestInstance(t *testing.T, scheme *runtime.Scheme, objs ...client.Object) {
	t.Helper()
	owner := testInstance()
	for _, obj := range objs {
		if err := controllerutil.SetControllerReference(owner, obj, scheme, controllerutil.WithBlockOwnerDeletion(false)); err != nil {
			t.Fatalf("set controller reference on %T %s: %v", obj, obj.GetName(), err)
		}
	}
}

// agentSpec returns the k8s.AgentSpec the controller uses to build resources
// for the test instance (mirrors AgentInstanceReconciler.Reconcile).
func agentSpec() k8s.AgentSpec {
	return k8s.AgentSpec{
		Namespace:    testNamespace,
		Image:        testAgentCfg().AgentImage,
		GatewayToken: testAgentCfg().GatewayToken,
		Port:         int32(testAgentCfg().AgentPort),
		AgentUser:    "zhang.wei",
	}
}

func newTestReconciler(t *testing.T, objs ...client.Object) (*AgentInstanceReconciler, client.Client) {
	t.Helper()
	scheme := testScheme(t)
	// The operator reconciles the per-user kubeconfig Secrets (issue #19
	// Option B); seed the platform (agent-kubeconfig) and the test owner's
	// per-user Secret so Reconcile does not requeue on a missing Secret.
	secrets := []client.Object{
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: k8s.KubeconfigSecretName, Namespace: testNamespace},
			Data:       map[string][]byte{"config": []byte("platform-kubeconfig")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: k8s.UserKubeconfigSecretFor("zhang.wei"), Namespace: testNamespace},
			Data:       map[string][]byte{"config": []byte("user-kubeconfig")},
		},
	}
	objs = append(objs, secrets...)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.AgentInstance{}, &v1alpha1.AgentTemplate{}).
		WithObjects(objs...).
		Build()
	r := &AgentInstanceReconciler{Client: cl, Scheme: scheme, Cfg: testAgentCfg()}
	return r, cl
}

func reconcileInstance(r *AgentInstanceReconciler, t *testing.T) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: testInstanceName},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

// provisionInstance reconciles twice: the first pass only adds the
// AgentInstance finalizer (early return), the second actually provisions.

func provisionInstance(r *AgentInstanceReconciler, t *testing.T) {
	t.Helper()
	reconcileInstance(r, t)
	reconcileInstance(r, t)
}

// kubeconfigRevForTest mirrors Reconcile's kubeconfig-revision annotation: it
// reads the seeded kubeconfig Secrets (fake client assigns resourceVersion 999)
// so a test-seeded "already converged" Pod carries the exact annotation the
// controller computes.
func kubeconfigRevForTest(t *testing.T, cl client.Client) string {
	t.Helper()
	var us, ps corev1.Secret
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: k8s.UserKubeconfigSecretFor("zhang.wei")}, &us); err != nil {
		t.Fatalf("get user kubeconfig secret: %v", err)
	}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: k8s.KubeconfigSecretName}, &ps); err != nil {
		t.Fatalf("get platform kubeconfig secret: %v", err)
	}
	return k8s.UserKubeconfigSecretFor("zhang.wei") + "@" + us.ResourceVersion + "|" + k8s.KubeconfigSecretName + "@" + ps.ResourceVersion
}

// TestAgentInstanceReconcileProvisions verifies one Reconcile creates the
// data PVC, the gateway Service and the agent Pod, reports the instance as
// Creating while the pod is provisioning, and that a second Reconcile is
// idempotent (no duplicate resources, no status write amplification).
func TestAgentInstanceReconcileProvisions(t *testing.T) {
	r, cl := newTestReconciler(t, testTemplate(), testInstance())
	provisionInstance(r, t)

	// PVC / Service / Pod created.
	var pvc corev1.PersistentVolumeClaim
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testPVCName}, &pvc); err != nil {
		t.Errorf("data pvc not created: %v", err)
	}
	var svc corev1.Service
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testPodName}, &svc); err != nil {
		t.Errorf("gateway service not created: %v", err)
	}
	var pod corev1.Pod
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testPodName}, &pod); err != nil {
		t.Errorf("pod not created: %v", err)
	}

	// Pod not ready yet -> Creating, with the resource names in status.
	var inst v1alpha1.AgentInstance
	if err := cl.Get(context.Background(), types.NamespacedName{Name: testInstanceName}, &inst); err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if inst.Status.Phase != v1alpha1.InstanceCreating {
		t.Errorf("phase = %q, want %q", inst.Status.Phase, v1alpha1.InstanceCreating)
	}
	if inst.Status.PodName != testPodName || inst.Status.PVCName != testPVCName || inst.Status.ServiceName != testPodName {
		t.Errorf("status names = pod %q pvc %q svc %q", inst.Status.PodName, inst.Status.PVCName, inst.Status.ServiceName)
	}

	// Idempotent: a second Reconcile must not duplicate resources or rewrite
	// status (no write amplification on the periodic requeue). resourceVersion
	// is the direct evidence -- a status write bumps it -- where the old
	// assertion inferred the same thing from a field the write happened to set.
	rvBefore := inst.ResourceVersion
	reconcileInstance(r, t)
	var pods corev1.PodList
	if err := cl.List(context.Background(), &pods); err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 1 {
		t.Errorf("pods after re-reconcile = %d, want 1 (idempotent)", len(pods.Items))
	}
	var inst2 v1alpha1.AgentInstance
	if err := cl.Get(context.Background(), types.NamespacedName{Name: testInstanceName}, &inst2); err != nil {
		t.Fatal(err)
	}
	if inst2.ResourceVersion != rvBefore {
		t.Errorf("status rewritten on no-change reconcile (resourceVersion %s -> %s)", rvBefore, inst2.ResourceVersion)
	}
}

// TestAgentInstanceReconcileReady verifies a Ready Pod transitions the
// instance to status.phase = Ready (AC: creating an instance -> Ready).
func TestAgentInstanceReconcileReady(t *testing.T) {
	r, cl := newTestReconciler(t, testTemplate(), testInstance())
	spec := agentSpec()
	spec.UserKubeconfigSecret = k8s.UserKubeconfigSecretFor("zhang.wei")
	readyPod := spec.PodFor(testPodName, testInstanceName, testPVCName, testPodName)
	readyPod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	// Match the kubeconfig-revision annotation Reconcile sets so the seeded
	// "already converged" pod is not seen as drifted and recreated.
	readyPod.Annotations = map[string]string{k8s.KubeconfigRevisionAnnotation: kubeconfigRevForTest(t, cl)}
	ownByTestInstance(t, r.Scheme, readyPod)
	if err := cl.Create(context.Background(), readyPod); err != nil {
		t.Fatalf("create ready pod: %v", err)
	}
	provisionInstance(r, t)

	var inst v1alpha1.AgentInstance
	if err := cl.Get(context.Background(), types.NamespacedName{Name: testInstanceName}, &inst); err != nil {
		t.Fatal(err)
	}
	if inst.Status.Phase != v1alpha1.InstanceReady {
		t.Errorf("phase = %q, want %q", inst.Status.Phase, v1alpha1.InstanceReady)
	}
	if inst.Status.Message != "instance ready" {
		t.Errorf("message = %q, want %q", inst.Status.Message, "instance ready")
	}
}

// makeReadyPod seeds an already-converged Ready Pod for the test instance (mirrors
// TestAgentInstanceReconcileReady) so Reconcile reports phase Ready.
func makeReadyPod(t *testing.T, r *AgentInstanceReconciler, cl client.Client) {
	t.Helper()
	spec := agentSpec()
	spec.UserKubeconfigSecret = k8s.UserKubeconfigSecretFor("zhang.wei")
	p := spec.PodFor(testPodName, testInstanceName, testPVCName, testPodName)
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	p.Annotations = map[string]string{k8s.KubeconfigRevisionAnnotation: kubeconfigRevForTest(t, cl)}
	ownByTestInstance(t, r.Scheme, p)
	if err := cl.Create(context.Background(), p); err != nil {
		t.Fatalf("create ready pod: %v", err)
	}
	provisionInstance(r, t)
}

// TestModelConfiguredCondition verifies the ModelConfigured status condition
// (issue #117): it is decoupled from the pod lifecycle -- a Ready instance
// reports False while its template has no usable provider, and True once a keyed
// provider's credential Secret exists. A keyed provider without its Secret (or
// an empty provider list) reports False with a user-facing message.
func TestModelConfiguredCondition(t *testing.T) {
	keyedModel := func() *v1alpha1.AgentTemplate {
		tpl := testTemplate()
		tpl.Spec.Providers = []v1alpha1.TemplateProviderSpec{{
			Name:          "platform",
			Endpoint:      "https://api.deepseek.com",
			CredentialRef: &corev1.LocalObjectReference{Name: "cubepilot-llm"},
			Models:        []string{"deepseek-v4-flash"},
		}}
		return tpl
	}
	getCond := func(t *testing.T, cl client.Client) *metav1.Condition {
		t.Helper()
		var inst v1alpha1.AgentInstance
		if err := cl.Get(context.Background(), types.NamespacedName{Name: testInstanceName}, &inst); err != nil {
			t.Fatal(err)
		}
		return meta.FindStatusCondition(inst.Status.Conditions, modelConfiguredCondition)
	}

	t.Run("ready without any model reports not-configured", func(t *testing.T) {
		r, cl := newTestReconciler(t, testTemplate(), testInstance())
		makeReadyPod(t, r, cl)
		cond := getCond(t, cl)
		if cond == nil {
			t.Fatal("ModelConfigured condition missing")
		}
		if cond.Status != metav1.ConditionFalse || cond.Reason != reasonNoModelConfigured {
			t.Errorf("condition = %s (%s), want False (%s)", cond.Status, cond.Reason, reasonNoModelConfigured)
		}
		if cond.Message == "" {
			t.Error("no-model condition should carry a user-facing message")
		}
	})

	t.Run("keyed model with credential secret reports configured", func(t *testing.T) {
		cred := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "cubepilot-llm", Namespace: testNamespace},
			Data:       map[string][]byte{"apiKey": []byte("sk-test")},
		}
		r, cl := newTestReconciler(t, keyedModel(), testInstance(), cred)
		makeReadyPod(t, r, cl)
		cond := getCond(t, cl)
		if cond == nil {
			t.Fatal("ModelConfigured condition missing")
		}
		if cond.Status != metav1.ConditionTrue || cond.Reason != reasonModelConfigured {
			t.Errorf("condition = %s (%s), want True (%s)", cond.Status, cond.Reason, reasonModelConfigured)
		}
	})

	t.Run("keyed model without its secret reports not-configured", func(t *testing.T) {
		r, cl := newTestReconciler(t, keyedModel(), testInstance())
		makeReadyPod(t, r, cl)
		cond := getCond(t, cl)
		if cond == nil {
			t.Fatal("ModelConfigured condition missing")
		}
		if cond.Status != metav1.ConditionFalse || cond.Reason != reasonNoModelConfigured {
			t.Errorf("condition = %s (%s), want False (%s)", cond.Status, cond.Reason, reasonNoModelConfigured)
		}
	})

	t.Run("condition flips when a model is added after Ready", func(t *testing.T) {
		r, cl := newTestReconciler(t, testTemplate(), testInstance())
		makeReadyPod(t, r, cl)
		if cond := getCond(t, cl); cond == nil || cond.Status != metav1.ConditionFalse {
			t.Fatalf("expected False before any model, got %v", cond)
		}
		// Add a keyed model + its credential Secret, then reconcile again.
		var tpl v1alpha1.AgentTemplate
		if err := cl.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "cubepilot"}, &tpl); err != nil {
			t.Fatal(err)
		}
		tpl.Spec.Providers = []v1alpha1.TemplateProviderSpec{{
			Name:          "platform",
			Endpoint:      "https://api.deepseek.com",
			CredentialRef: &corev1.LocalObjectReference{Name: "cubepilot-llm"},
			Models:        []string{"deepseek-v4-flash"},
		}}
		if err := cl.Update(context.Background(), &tpl); err != nil {
			t.Fatalf("update template with provider: %v", err)
		}
		cred := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "cubepilot-llm", Namespace: testNamespace},
			Data:       map[string][]byte{"apiKey": []byte("sk-test")},
		}
		if err := cl.Create(context.Background(), cred); err != nil {
			t.Fatalf("create credential secret: %v", err)
		}
		reconcileInstance(r, t)
		if cond := getCond(t, cl); cond == nil || cond.Status != metav1.ConditionTrue {
			t.Errorf("condition after adding model = %v, want True", cond)
		}
	})
}

// TestAgentInstanceWaitsForUserIdentity verifies the controller does NOT
// provision the Pod with a placeholder (shared SA) identity while the per-user
// kubeconfig Secret is still being minted by the bootstrap (issue #19/#96). A
// Pod born without the user Secret used to be deleted ~60s later, once the
// Secret landed, and restarted -- cutting any in-flight chat (issue #100).
// Instead the reconcile waits (requeue) and the first-and-only Pod mounts the
// user kubeconfig, so the periodic reconcile sees no drift.
func TestAgentInstanceWaitsForUserIdentity(t *testing.T) {
	scheme := testScheme(t)
	// The per-user kubeconfig Secret is deliberately absent: the builtin
	// bootstrap mints it asynchronously (SA token -> kubeconfig Secret).
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.AgentInstance{}, &v1alpha1.AgentTemplate{}).
		WithObjects(
			testTemplate(),
			testInstance(),
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: k8s.KubeconfigSecretName, Namespace: testNamespace},
				Data:       map[string][]byte{"config": []byte("platform-kubeconfig")},
			},
		).
		Build()
	r := &AgentInstanceReconciler{Client: cl, Scheme: scheme, Cfg: testAgentCfg()}

	// Reconcile #1 adds the finalizer; #2 finds no per-user kubeconfig Secret
	// and must requeue (wait for the identity), not create a fallback Pod.
	reconcileInstance(r, t)
	res, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: testInstanceName},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Error("expected a requeue while the per-user kubeconfig Secret is absent")
	}
	// No instance resources are created while the identity is missing.
	for name, kind := range map[string]client.Object{
		testPodName: &corev1.Pod{},
		testPVCName: &corev1.PersistentVolumeClaim{},
	} {
		if err := cl.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: name}, kind); !apierrors.IsNotFound(err) {
			t.Errorf("%T %s created while per-user kubeconfig Secret absent (err=%v); must wait, not fall back", kind, name, err)
		}
	}

	// The bootstrap mints the per-user kubeconfig Secret; the next reconcile
	// provisions the Pod with the user identity.
	if err := cl.Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: k8s.UserKubeconfigSecretFor("zhang.wei"), Namespace: testNamespace},
		Data:       map[string][]byte{"config": []byte("user-kubeconfig")},
	}); err != nil {
		t.Fatalf("create per-user kubeconfig secret: %v", err)
	}
	reconcileInstance(r, t)

	var first corev1.Pod
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testPodName}, &first); err != nil {
		t.Fatalf("pod not created once the identity is provisioned: %v", err)
	}
	defaultSecret := ""
	for _, v := range first.Spec.Volumes {
		if v.Name == "kubeconfig" && v.Secret != nil {
			defaultSecret = v.Secret.SecretName
		}
	}
	if defaultSecret != k8s.UserKubeconfigSecretFor("zhang.wei") {
		t.Errorf("default kubeconfig volume secret = %q, want the per-user Secret", defaultSecret)
	}

	// The periodic reconcile (~60s cadence) must see no drift: the annotation
	// recorded when the pod was created still matches the Secrets, so the Ready
	// pod is left alone -- no issue #100 redundant restart.
	reconcileInstance(r, t)
	var second corev1.Pod
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testPodName}, &second); err != nil {
		t.Fatalf("pod deleted by a no-change reconcile (issue #100 drift): %v", err)
	}
	if second.UID != first.UID {
		t.Errorf("pod recreated on the periodic reconcile (uid %s -> %s); first provision must converge without a restart", first.UID, second.UID)
	}
}

// TestAgentInstanceReconcileSelfHeals verifies a Failed Pod is deleted and
// recreated by the controller (AC: pod failure self-heals; PVC persists).
func TestAgentInstanceReconcileSelfHeals(t *testing.T) {
	r, cl := newTestReconciler(t, testTemplate(), testInstance())
	spec := agentSpec()
	spec.UserKubeconfigSecret = k8s.UserKubeconfigSecretFor("zhang.wei")
	failedPod := spec.PodFor(testPodName, testInstanceName, testPVCName, testPodName)
	failedPod.Status.Phase = corev1.PodFailed
	// Match the kubeconfig-revision annotation Reconcile sets; the pod is
	// healed because it is Failed (explicit delete+recreate), not for drift.
	failedPod.Annotations = map[string]string{k8s.KubeconfigRevisionAnnotation: kubeconfigRevForTest(t, cl)}
	ownByTestInstance(t, r.Scheme, failedPod)
	if err := cl.Create(context.Background(), failedPod); err != nil {
		t.Fatalf("create failed pod: %v", err)
	}
	// Reconcile #1 adds the finalizer; #2 sees the Failed pod, deletes it and
	// requeues WITHOUT re-creating in the same reconcile (deletion is async; a
	// same-name create would race the terminating pod with AlreadyExists). The
	// instance stays Failed while the pod is gone.
	provisionInstance(r, t)

	var inst v1alpha1.AgentInstance
	if err := cl.Get(context.Background(), types.NamespacedName{Name: testInstanceName}, &inst); err != nil {
		t.Fatal(err)
	}
	if inst.Status.Phase != v1alpha1.InstanceFailed {
		t.Errorf("phase = %q, want %q while the failed pod is being replaced", inst.Status.Phase, v1alpha1.InstanceFailed)
	}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testPodName}, &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Errorf("failed pod should be deleted and not re-created in the same reconcile (err=%v)", err)
	}

	// A later reconcile creates the replacement.
	reconcileInstance(r, t)
	var pod corev1.Pod
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testPodName}, &pod); err != nil {
		t.Fatalf("fresh pod missing after heal: %v", err)
	}
	if pod.Status.Phase == corev1.PodFailed {
		t.Error("failed pod was not replaced")
	}

	var pvc corev1.PersistentVolumeClaim
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testPVCName}, &pvc); err != nil {
		t.Errorf("data pvc missing after heal: %v", err)
	}
}

// TestAgentInstanceReconcileRemovesDataPVCOnDelete verifies the finalizer
// drops the data PVC (and the pod/service) when the instance is deleted
// (design §3.2 data-directory GC / reclaim).
func TestAgentInstanceReconcileRemovesDataPVCOnDelete(t *testing.T) {
	now := metav1.Now()
	inst := testInstance()
	inst.DeletionTimestamp = &now
	inst.Finalizers = []string{finalizerName}

	spec := agentSpec()
	pvc := spec.DataPVCFor(testPVCName, testInstanceName, "1Gi")
	pod := spec.PodFor(testPodName, testInstanceName, testPVCName, testPodName)
	svc := spec.ServiceFor(testPodName, testInstanceName, testPodName)
	ownByTestInstance(t, testScheme(t), pvc, pod, svc)

	r, cl := newTestReconciler(t, inst, pvc, pod, svc)
	reconcileInstance(r, t)

	var gotPVC corev1.PersistentVolumeClaim
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testPVCName}, &gotPVC); !apierrors.IsNotFound(err) {
		t.Errorf("data pvc not reclaimed (err=%v)", err)
	}
	var gotPod corev1.Pod
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testPodName}, &gotPod); !apierrors.IsNotFound(err) {
		t.Errorf("agent pod not removed (err=%v)", err)
	}
	var gotSvc corev1.Service
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testPodName}, &gotSvc); !apierrors.IsNotFound(err) {
		t.Errorf("agent service not removed (err=%v)", err)
	}

	// Once the last finalizer is removed the object is deleted (the API server
	// reclaims it); the fake client emulates this.
	var gotInst v1alpha1.AgentInstance
	if err := cl.Get(context.Background(), types.NamespacedName{Name: testInstanceName}, &gotInst); !apierrors.IsNotFound(err) {
		t.Errorf("instance not reclaimed after finalize (err=%v)", err)
	}
}

// TestAgentInstanceFinalizeTouchesOnlyItsOwnObjects verifies the finalizer's
// ownership rule: the generated names come from metadata.name, which the
// writer of the AgentInstance picks, so a name-identical object that is not
// this instance's must be left alone -- and must not fail the delete either
// (an erroring finalizer would pin the instance in deletion forever). The
// instance's own objects are still reclaimed.
func TestAgentInstanceFinalizeTouchesOnlyItsOwnObjects(t *testing.T) {
	for _, tc := range []struct {
		name  string
		owned bool
	}{
		{"leaves foreign objects alone", false},
		{"reclaims its own objects", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := metav1.Now()
			inst := testInstance()
			inst.DeletionTimestamp = &now
			inst.Finalizers = []string{finalizerName}

			spec := agentSpec()
			pvc := spec.DataPVCFor(testPVCName, testInstanceName, "1Gi")
			pod := spec.PodFor(testPodName, testInstanceName, testPVCName, testPodName)
			svc := spec.ServiceFor(testPodName, testInstanceName, testPodName)
			if tc.owned {
				ownByTestInstance(t, testScheme(t), pvc, pod, svc)
			}

			r, cl := newTestReconciler(t, inst, pvc, pod, svc)
			reconcileInstance(r, t)

			for _, obj := range []struct {
				name string
				obj  client.Object
			}{
				{testPVCName, &corev1.PersistentVolumeClaim{}},
				{testPodName, &corev1.Pod{}},
				{testPodName, &corev1.Service{}},
			} {
				err := cl.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: obj.name}, obj.obj)
				if missing := apierrors.IsNotFound(err); missing != tc.owned {
					t.Errorf("%T %s: missing = %v, want %v (err=%v)", obj.obj, obj.name, missing, tc.owned, err)
				}
			}

			// Released either way: a foreign object is not ours to delete, and
			// skipping it is not a reason to block the instance's deletion.
			if err := cl.Get(context.Background(), types.NamespacedName{Name: testInstanceName}, &v1alpha1.AgentInstance{}); !apierrors.IsNotFound(err) {
				t.Errorf("instance not reclaimed after finalize (err=%v)", err)
			}
		})
	}
}

// TestAgentInstanceReconcileDoesNotAdoptForeignPVC verifies the ownership rule
// on the data PVC: the name is derived from metadata.name, which the writer of
// the AgentInstance chooses, so a PVC already sitting under that name that is
// not this instance's must never be adopted. Adopting it would hand a
// stranger's volume to the instance and let finalize delete it later; the
// reconcile fails closed instead -- the instance goes Failed, the PVC keeps its
// owner and contents, and nothing is provisioned onto it.
func TestAgentInstanceReconcileDoesNotAdoptForeignPVC(t *testing.T) {
	ctx := context.Background()
	// A PVC belonging to something else: no controller owner reference.
	foreign := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: testPVCName, Namespace: testNamespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("5Gi")},
			},
		},
	}
	r, cl := newTestReconciler(t, testTemplate(), testInstance(), foreign)
	provisionInstance(r, t)

	var got corev1.PersistentVolumeClaim
	if err := cl.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: testPVCName}, &got); err != nil {
		t.Fatalf("foreign pvc was deleted: %v", err)
	}
	if len(got.OwnerReferences) != 0 {
		t.Errorf("foreign pvc was adopted: ownerReferences = %v", got.OwnerReferences)
	}
	if size := got.Spec.Resources.Requests[corev1.ResourceStorage]; size.String() != "5Gi" {
		t.Errorf("foreign pvc was modified: storage = %s, want 5Gi", size.String())
	}

	// Nothing may be provisioned onto a volume the instance does not own.
	if err := cl.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: testPodName}, &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Errorf("pod created on a foreign data pvc (err=%v)", err)
	}

	var inst v1alpha1.AgentInstance
	if err := cl.Get(ctx, types.NamespacedName{Name: testInstanceName}, &inst); err != nil {
		t.Fatal(err)
	}
	if inst.Status.Phase != v1alpha1.InstanceFailed {
		t.Errorf("phase = %q, want %q", inst.Status.Phase, v1alpha1.InstanceFailed)
	}
	if !strings.Contains(inst.Status.Message, "not owned") {
		t.Errorf("status message = %q, want it to name the ownership failure so an operator can see why", inst.Status.Message)
	}
}

// TestAgentInstanceReconcileDoesNotReplaceForeignPod verifies the ownership rule
// on the Pod path, which is the destructive one: ensurePod deletes an existing
// Pod whose security fingerprint drifted. A Pod under the generated name that is
// not this instance's must not be deleted, even when it looks drifted -- the
// reconcile fails closed instead.
func TestAgentInstanceReconcileDoesNotReplaceForeignPod(t *testing.T) {
	ctx := context.Background()
	// A foreign Pod that *would* be deleted as drift: writable init-container
	// root filesystem and no supervisor resource limits.
	spec := agentSpec()
	foreign := spec.PodFor(testPodName, testInstanceName, testPVCName, testPodName)
	foreign.Spec.InitContainers[0].SecurityContext.ReadOnlyRootFilesystem = nil
	foreign.Spec.Containers[0].Resources = corev1.ResourceRequirements{}
	foreign.UID = types.UID("foreign-pod-uid")

	r, cl := newTestReconciler(t, testTemplate(), testInstance(), foreign)
	provisionInstance(r, t)

	var got corev1.Pod
	if err := cl.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: testPodName}, &got); err != nil {
		t.Fatalf("foreign pod was deleted: %v", err)
	}
	if got.UID != foreign.UID {
		t.Errorf("foreign pod was replaced (uid %s -> %s)", foreign.UID, got.UID)
	}
	if len(got.OwnerReferences) != 0 {
		t.Errorf("foreign pod was adopted: ownerReferences = %v", got.OwnerReferences)
	}

	var inst v1alpha1.AgentInstance
	if err := cl.Get(ctx, types.NamespacedName{Name: testInstanceName}, &inst); err != nil {
		t.Fatal(err)
	}
	if inst.Status.Phase != v1alpha1.InstanceFailed {
		t.Errorf("phase = %q, want %q", inst.Status.Phase, v1alpha1.InstanceFailed)
	}
	if !strings.Contains(inst.Status.Message, "not owned") {
		t.Errorf("status message = %q, want it to name the ownership failure so an operator can see why", inst.Status.Message)
	}
}

// TestAgentInstanceReconcileDoesNotAdoptForeignService verifies the same rule on
// the Service path: an existing same-named Service that is not this instance's
// is left as it is (it is never rewritten to select this instance's Pod) and the
// reconcile fails closed.
func TestAgentInstanceReconcileDoesNotAdoptForeignService(t *testing.T) {
	ctx := context.Background()
	foreign := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: testPodName, Namespace: testNamespace},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "someone-else"}},
	}

	r, cl := newTestReconciler(t, testTemplate(), testInstance(), foreign)
	provisionInstance(r, t)

	var got corev1.Service
	if err := cl.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: testPodName}, &got); err != nil {
		t.Fatalf("foreign service was deleted: %v", err)
	}
	if len(got.OwnerReferences) != 0 {
		t.Errorf("foreign service was adopted: ownerReferences = %v", got.OwnerReferences)
	}
	if got.Spec.Selector["app"] != "someone-else" {
		t.Errorf("foreign service was rewritten: selector = %v", got.Spec.Selector)
	}

	var inst v1alpha1.AgentInstance
	if err := cl.Get(ctx, types.NamespacedName{Name: testInstanceName}, &inst); err != nil {
		t.Fatal(err)
	}
	if inst.Status.Phase != v1alpha1.InstanceFailed {
		t.Errorf("phase = %q, want %q", inst.Status.Phase, v1alpha1.InstanceFailed)
	}
	if !strings.Contains(inst.Status.Message, "not owned") {
		t.Errorf("status message = %q, want it to name the ownership failure so an operator can see why", inst.Status.Message)
	}
}

// TestEnsurePodRecreatesOnSecurityDrift verifies a Pod whose immutable security
// spec (image, security context, resource limits) drifted is deleted and
// recreated so it converges on the current baseline -- e.g. an instance Pod
// created before the operator gained the design §6 baseline
// (readOnlyRootFilesystem, resource limits) is rolled onto it. Mirrors the
// failed-Pod healing path.
func TestEnsurePodRecreatesOnSecurityDrift(t *testing.T) {
	spec := agentSpec()
	desired := spec.PodFor(testPodName, testInstanceName, testPVCName, testPodName)

	r, cl := newTestReconciler(t)
	ownByTestInstance(t, r.Scheme, desired)

	// A pod created before the baseline landed: writable init-container root
	// filesystem and no resource limits on the supervisor.
	stale := desired.DeepCopy()
	stale.Spec.InitContainers[0].SecurityContext.ReadOnlyRootFilesystem = nil
	stale.Spec.Containers[0].Resources = corev1.ResourceRequirements{}

	if err := cl.Create(context.Background(), stale); err != nil {
		t.Fatalf("seed stale pod: %v", err)
	}
	ctx := context.Background()
	recreate, err := r.ensurePod(ctx, desired)
	if err != nil {
		t.Fatalf("ensurePod: %v", err)
	}
	if !recreate {
		t.Fatal("expected recreate=true on security drift")
	}
	// The drifted pod is deleted but NOT re-created in the same call (the old
	// pod is still terminating; a same-name create would race it with
	// AlreadyExists). A later reconcile creates the replacement.
	if err := cl.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: testPodName}, &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("drift should delete without immediate re-create; got err=%v", err)
	}
	if recreate, err := r.ensurePod(ctx, desired); err != nil || recreate {
		t.Fatalf("ensurePod (recreate): err=%v recreate=%v", err, recreate)
	}

	var got corev1.Pod
	if err := cl.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: testPodName}, &got); err != nil {
		t.Fatalf("get pod after drift: %v", err)
	}
	if got.Spec.InitContainers[0].SecurityContext == nil ||
		got.Spec.InitContainers[0].SecurityContext.ReadOnlyRootFilesystem == nil ||
		!*got.Spec.InitContainers[0].SecurityContext.ReadOnlyRootFilesystem {
		t.Error("pod not rolled onto baseline: init container root not read-only")
	}
	if got.Spec.Containers[0].Resources.Limits == nil {
		t.Error("pod not rolled onto baseline: supervisor resource limits missing")
	}
}

// TestEnsurePodKubeconfigDriftDeleteDoesNotCreateInSameCall pins the observed
// bug (issue #98 chat e2e): on a fresh provision the mounted kubeconfig Secret's
// resourceVersion settles right after the Pod is created, so the first periodic
// reconcile sees a security-fingerprint drift. ensurePod must delete the pod and
// report recreate without immediately creating a same-name pod (AlreadyExists
// while the old one terminates), which used to strand the instance in a Failed
// heal loop.
func TestEnsurePodKubeconfigDriftDeleteDoesNotCreateInSameCall(t *testing.T) {
	ctx := context.Background()
	nsn := types.NamespacedName{Namespace: testNamespace, Name: testPodName}
	old := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testPodName,
			Annotations: map[string]string{k8s.KubeconfigRevisionAnnotation: "stale-rev"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "supervisor", Image: "img"}}},
	}
	desired := old.DeepCopy()
	desired.Annotations[k8s.KubeconfigRevisionAnnotation] = "rotated-rev"

	r, cl := newTestReconciler(t)
	// Both the existing and the desired pod must be the instance's own, or
	// ensurePod fails closed instead of reaching the drift comparison.
	ownByTestInstance(t, r.Scheme, old, desired)
	if err := cl.Create(ctx, old); err != nil {
		t.Fatalf("seed pod: %v", err)
	}

	recreate, err := r.ensurePod(ctx, desired)
	if err != nil {
		t.Fatalf("ensurePod: %v", err)
	}
	if !recreate {
		t.Fatal("expected recreate=true on kubeconfig fingerprint drift")
	}
	if err := cl.Get(ctx, nsn, &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("drift should delete without immediate re-create; got err=%v", err)
	}

	recreate, err = r.ensurePod(ctx, desired)
	if err != nil {
		t.Fatalf("ensurePod (create): %v", err)
	}
	if recreate {
		t.Fatal("expected recreate=false when creating a missing pod")
	}
	var got corev1.Pod
	if err := cl.Get(ctx, nsn, &got); err != nil {
		t.Fatalf("replacement pod not created: %v", err)
	}
	if got.Annotations[k8s.KubeconfigRevisionAnnotation] != "rotated-rev" {
		t.Errorf("replacement annotation = %q, want rotated-rev", got.Annotations[k8s.KubeconfigRevisionAnnotation])
	}
}

// TestEnsurePodDoesNotRecreateOnConfigDrift guards the in-place config reload
// design: config-derived spec fields (env) must never trigger a Pod delete --
// the supervisor applies config changes in place, so the Pod (and its
// sessions/PVC/IP) has to survive.
func TestEnsurePodDoesNotRecreateOnConfigDrift(t *testing.T) {
	spec := agentSpec()
	desired := spec.PodFor(testPodName, testInstanceName, testPVCName, testPodName)

	r, cl := newTestReconciler(t)
	ownByTestInstance(t, r.Scheme, desired)

	existing := desired.DeepCopy()
	for i := range existing.Spec.Containers[0].Env {
		if existing.Spec.Containers[0].Env[i].Name == "OPENCLAW_HOME" {
			existing.Spec.Containers[0].Env[i].Value = "/home/other"
		}
	}
	existing.UID = types.UID("seed-uid")

	if err := cl.Create(context.Background(), existing); err != nil {
		t.Fatalf("seed pod: %v", err)
	}
	recreate, err := r.ensurePod(context.Background(), desired)
	if err != nil {
		t.Fatalf("ensurePod: %v", err)
	}
	if recreate {
		t.Error("config drift must not recreate the pod")
	}

	var got corev1.Pod
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testPodName}, &got); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if got.UID != existing.UID {
		t.Errorf("pod was recreated on config drift (uid %s -> %s)", existing.UID, got.UID)
	}
}

// TestSecurityFingerprintKubeconfigRevision verifies an in-place kubeconfig
// Secret content change (same Secret name, new resourceVersion) changes the
// security fingerprint, so ensurePod recreates the Pod despite SubPath mounts.
func TestSecurityFingerprintKubeconfigRevision(t *testing.T) {
	build := func(rev string) *corev1.Pod {
		spec := agentSpec()
		spec.UserKubeconfigSecret = k8s.UserKubeconfigSecretFor("zhang.wei")
		pod := spec.PodFor(testPodName, testInstanceName, testPVCName, testPodName)
		pod.Annotations = map[string]string{k8s.KubeconfigRevisionAnnotation: rev}
		return pod
	}
	if reflect.DeepEqual(securityFingerprint(build("u@1|p@1")), securityFingerprint(build("u@2|p@1"))) {
		t.Error("fingerprint unchanged when the user kubeconfig resourceVersion changes")
	}
	if !reflect.DeepEqual(securityFingerprint(build("u@2|p@1")), securityFingerprint(build("u@2|p@1"))) {
		t.Error("fingerprint should be stable for the same kubeconfig revision")
	}
}

// TestAgentInstanceFinalizeReclaimsGeneratedPVCOnly verifies the finalizer
// reclaims exactly the platform-generated data-<instance> PVC and nothing else:
// an unrelated PVC in the same namespace survives, and carrying a dataVolume in
// the spec does not change which PVC is removed. The name is generated from the
// instance name, so no spec value can select it.
func TestAgentInstanceFinalizeReclaimsGeneratedPVCOnly(t *testing.T) {
	now := metav1.Now()
	inst := testInstance()
	inst.DeletionTimestamp = &now
	inst.Finalizers = []string{finalizerName}
	inst.Spec.DataVolume = &v1alpha1.DataVolumeSpec{Size: "2Gi"}

	spec := agentSpec()
	generated := spec.DataPVCFor(testPVCName, testInstanceName, "2Gi")
	other := spec.DataPVCFor("data-somebody-else", "somebody-else", "1Gi")
	ownByTestInstance(t, testScheme(t), generated)

	r, cl := newTestReconciler(t, inst, generated, other)
	reconcileInstance(r, t)

	var gotGenerated corev1.PersistentVolumeClaim
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testPVCName}, &gotGenerated); !apierrors.IsNotFound(err) {
		t.Errorf("generated data pvc not reclaimed (err=%v)", err)
	}
	var gotOther corev1.PersistentVolumeClaim
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "data-somebody-else"}, &gotOther); err != nil {
		t.Errorf("unrelated pvc was deleted or unreadable (err=%v)", err)
	}
}

// TestAgentInstanceDataVolumeSizeReachesPVC verifies a configured
// dataVolume.size is applied to the data PVC (the default is 1Gi).
func TestAgentInstanceDataVolumeSizeReachesPVC(t *testing.T) {
	inst := testInstance()
	inst.Spec.DataVolume = &v1alpha1.DataVolumeSpec{Size: "2Gi"}

	r, cl := newTestReconciler(t, testTemplate(), inst)
	reconcileInstance(r, t)
	reconcileInstance(r, t)

	var pvc corev1.PersistentVolumeClaim
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testPVCName}, &pvc); err != nil {
		t.Fatalf("data pvc not created: %v", err)
	}
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "2Gi" {
		t.Errorf("pvc storage request = %s, want 2Gi", got.String())
	}
}

// TestGeneratedNameBoundedAndDeterministic pins the name bound. AgentInstance
// metadata.name is accepted up to 253 characters, so "data-<name>" can be 258
// -- a name the API server rejects, and which the finalizer then tries to
// delete under that same impossible name. Generated names must be truncated,
// yet stay distinct for distinct inputs (truncation alone collides) and
// unchanged for names that already fit, so existing resources keep their names.
func TestGeneratedNameBoundedAndDeterministic(t *testing.T) {
	// Inputs that fit are returned unchanged: this is exactly what the
	// controller derived before the bound existed.
	if got := k8s.GeneratedName("data", testInstanceName); got != testPVCName {
		t.Errorf("GeneratedName(data, %s) = %q, want %q", testInstanceName, got, testPVCName)
	}
	if got := k8s.GeneratedName("agent", testInstanceName); got != testPodName {
		t.Errorf("GeneratedName(agent, %s) = %q, want %q", testInstanceName, got, testPodName)
	}

	// 253 characters is the longest metadata.name Kubernetes accepts.
	long := strings.Repeat("a", 253)
	for _, prefix := range []string{"data", "agent"} {
		got := k8s.GeneratedName(prefix, long)
		if len(got) > k8s.MaxResourceNameLen {
			t.Errorf("GeneratedName(%s, 253-char name) = %d characters, want <= %d", prefix, len(got), k8s.MaxResourceNameLen)
		}
		if errs := validation.IsDNS1123Subdomain(got); len(errs) > 0 {
			t.Errorf("GeneratedName(%s, 253-char name) = %q is not a valid DNS-1123 subdomain: %v", prefix, got, errs)
		}
		if got != k8s.GeneratedName(prefix, long) {
			t.Errorf("GeneratedName(%s, 253-char name) is not deterministic", prefix)
		}
		// Distinct long inputs must not collapse onto one name.
		other := k8s.GeneratedName(prefix, strings.Repeat("a", 252)+"b")
		if other == got {
			t.Errorf("two distinct 253-char names both produced %q", got)
		}
		// The readable head of the input survives the cut.
		if !strings.HasPrefix(got, prefix+"-aaa") {
			t.Errorf("GeneratedName(%s, 253-char name) = %q, want it to keep the input's head", prefix, got)
		}
	}
}

// TestAgentInstanceLongNameProvisionsAndReclaims runs the whole lifecycle for a
// 253-character instance name -- the longest the API server accepts -- and
// asserts that the create path and the finalizer agree on the bounded names:
// the finalizer reclaims the very PVC/Service the reconcile created. If the two
// derived the name differently, the instance would leak its data PVC.
func TestAgentInstanceLongNameProvisionsAndReclaims(t *testing.T) {
	ctx := context.Background()
	longName := strings.Repeat("a", 253)
	inst := testInstance()
	inst.Name = longName

	r, cl := newTestReconciler(t, testTemplate(), inst)
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: longName},
		}); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}

	pvcName := k8s.GeneratedName("data", longName)
	podName := k8s.GeneratedName("agent", longName)
	if len(pvcName) > k8s.MaxResourceNameLen || len(podName) > k8s.MaxResourceNameLen {
		t.Fatalf("generated names are unbounded: pvc %d, pod %d", len(pvcName), len(podName))
	}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: pvcName}, &corev1.PersistentVolumeClaim{}); err != nil {
		t.Fatalf("data pvc not created under the bounded name %s: %v", pvcName, err)
	}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: podName}, &corev1.Service{}); err != nil {
		t.Fatalf("gateway service not created under the bounded name %s: %v", podName, err)
	}

	if err := r.finalize(ctx, inst); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: pvcName}, &corev1.PersistentVolumeClaim{}); !apierrors.IsNotFound(err) {
		t.Errorf("finalize did not reclaim the data pvc the reconcile created (err=%v)", err)
	}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: podName}, &corev1.Service{}); !apierrors.IsNotFound(err) {
		t.Errorf("finalize did not reclaim the service the reconcile created (err=%v)", err)
	}
}
