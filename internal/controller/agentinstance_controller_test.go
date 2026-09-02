package controller

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/k8s"
)

const (
	testInstanceName = "zhang-wei-agent-for-cloud"
	testPodName      = "agent-zhang-wei-agent-for-cloud"
	testPVCName      = "data-zhang-wei-agent-for-cloud"
	testNamespace    = "cubepilot"
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
		ObjectMeta: metav1.ObjectMeta{Name: "agent-for-cloud"},
		Spec:       v1alpha1.AgentTemplateSpec{Runtime: v1alpha1.RuntimeOpenClaw},
	}
}

func testInstance() *v1alpha1.AgentInstance {
	return &v1alpha1.AgentInstance{
		ObjectMeta: metav1.ObjectMeta{Name: testInstanceName},
		Spec: v1alpha1.AgentInstanceSpec{
			TemplateRef: "agent-for-cloud",
			Owner:       "zhang.wei",
			Identity: v1alpha1.IdentitySpec{
				Mode:         v1alpha1.IdentityModeUser,
				PrincipalRef: v1alpha1.PrincipalRef{UserRef: "zhang.wei"},
			},
		},
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
	// status (no write amplification on the periodic requeue).
	firstActivity := inst.Status.LastActivity
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
	if firstActivity == nil || inst2.Status.LastActivity == nil || !inst2.Status.LastActivity.Equal(firstActivity) {
		t.Error("status rewritten on no-change reconcile (LastActivity changed)")
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
	if err := cl.Create(context.Background(), failedPod); err != nil {
		t.Fatalf("create failed pod: %v", err)
	}
	provisionInstance(r, t)

	var inst v1alpha1.AgentInstance
	if err := cl.Get(context.Background(), types.NamespacedName{Name: testInstanceName}, &inst); err != nil {
		t.Fatal(err)
	}
	if inst.Status.Phase != v1alpha1.InstanceFailed {
		t.Errorf("phase = %q, want %q", inst.Status.Phase, v1alpha1.InstanceFailed)
	}

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

// TestEnsurePodRecreatesOnSecurityDrift verifies a Pod whose immutable security
// spec (image, security context, resource limits) drifted is deleted and
// recreated so it converges on the current baseline -- e.g. an instance Pod
// created before the operator gained the design §6 baseline
// (readOnlyRootFilesystem, resource limits) is rolled onto it. Mirrors the
// failed-Pod healing path.
func TestEnsurePodRecreatesOnSecurityDrift(t *testing.T) {
	spec := agentSpec()
	desired := spec.PodFor(testPodName, testInstanceName, testPVCName, testPodName)

	// A pod created before the baseline landed: writable init-container root
	// filesystem and no resource limits on the supervisor.
	stale := desired.DeepCopy()
	stale.Spec.InitContainers[0].SecurityContext.ReadOnlyRootFilesystem = nil
	stale.Spec.Containers[0].Resources = corev1.ResourceRequirements{}

	r, cl := newTestReconciler(t)
	if err := cl.Create(context.Background(), stale); err != nil {
		t.Fatalf("seed stale pod: %v", err)
	}
	if err := r.ensurePod(context.Background(), desired); err != nil {
		t.Fatalf("ensurePod: %v", err)
	}

	var got corev1.Pod
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testPodName}, &got); err != nil {
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

// TestEnsurePodDoesNotRecreateOnConfigDrift guards the in-place config reload
// design: config-derived spec fields (env) must never trigger a Pod delete --
// the supervisor applies config changes in place, so the Pod (and its
// sessions/PVC/IP) has to survive.
func TestEnsurePodDoesNotRecreateOnConfigDrift(t *testing.T) {
	spec := agentSpec()
	desired := spec.PodFor(testPodName, testInstanceName, testPVCName, testPodName)
	existing := desired.DeepCopy()
	for i := range existing.Spec.Containers[0].Env {
		if existing.Spec.Containers[0].Env[i].Name == "OPENCLAW_HOME" {
			existing.Spec.Containers[0].Env[i].Value = "/home/other"
		}
	}
	existing.UID = types.UID("seed-uid")

	r, cl := newTestReconciler(t)
	if err := cl.Create(context.Background(), existing); err != nil {
		t.Fatalf("seed pod: %v", err)
	}
	if err := r.ensurePod(context.Background(), desired); err != nil {
		t.Fatalf("ensurePod: %v", err)
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
