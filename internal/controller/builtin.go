package controller

import (
	"context"
	"fmt"
	"log"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/gateway"
	"github.com/suanova/cubepilot/internal/k8s"
	"github.com/suanova/cubepilot/internal/skill"
)

// BuiltinAgentName is the preset platform agent (design §5.1: cubepilot
// is the first platform-preset Agent, auto-instantiated per user, non-deletable).
const BuiltinAgentName = "cubepilot"

// BuiltinTaskTemplateName is the preset nightly inspection template. It is the
// one preset that carries a schedule of its own (design §3.3.2).
const BuiltinTaskTemplateName = "daily-inspection"

// BuiltinProviderName is the provider key of the platform's own LLM. It is a
// name, not a model id: the platform default is one provider serving one model,
// and an admin can add more model ids to it or add providers beside it.
const BuiltinProviderName = "platform"

// Per-user identity ClusterRoles the platform binds each user's ServiceAccount
// to (issue #19), all declared in the chart rbac.yaml:
//
//   - `view`, the built-in read-only ClusterRole. It is namespaced by design and
//     deliberately excludes secrets, which is why it is safe to bind widely --
//     and also why it is not enough on its own: it grants no cluster-scoped
//     resource, so nothing node-level is readable through it.
//   - cubepilot-cluster-read, the cluster-scoped read `view` does not grant
//     (nodes and the rest), with secrets and the kubelet proxies excluded.
//   - cubepilot-user-crds, full ai.cubestack.io CRUD cluster-wide.
//
// The assistant executes kubectl with the user's identity and must operate
// platform CRs in any namespace (generic CRD discovery creates e.g. CubeStack
// DevEnvironments in arbitrary namespaces, not only the install namespace), so
// even though the six platform CRDs are Namespaced (issue #146) the per-user
// bindings stay ClusterRoleBindings. ClusterRoleBindings reference these roles
// by name, so the operator needs only get/bind on them -- and the chart lists
// every name here in that role's resourceNames.
const (
	UserViewClusterRole        = "view"
	UserClusterReadClusterRole = "cubepilot-cluster-read"
	UserCRDsClusterRole        = "cubepilot-user-crds"
)

// userClusterRoles is the set bound per user, in binding order. One list, so a
// role added here is bound, covered by the bootstrap test, and (once its name
// is in the chart's resourceNames) bindable by the operator.
var userClusterRoles = []string{
	UserViewClusterRole,
	UserClusterReadClusterRole,
	UserCRDsClusterRole,
}

// userCRBName builds the per-user ClusterRoleBinding name. The role segment is
// shortened where the full name is redundant next to the "cubepilot-user-"
// prefix, so binding names stay readable.
func userCRBName(user, role string) string {
	short := role
	switch role {
	case UserCRDsClusterRole:
		short = "crds"
	case UserClusterReadClusterRole:
		short = "cluster-read"
	}
	return "cubepilot-user-" + short + "-" + k8s.Sanitize(user) + "-" + k8s.UserIdentityHash(user)
}

// BuiltinSkills are the preset domain skills the builtin agent references
// (design §3.3.1 domain layer): the agent gets exactly the skills the platform
// ships. The content lives with the skill package; the API seeds the Skill
// CRDs at startup.
var BuiltinSkills = skill.BuiltinSkillNames()

// BuiltinProviders returns the preset provider for the builtin AgentTemplate
// (design §3.3: models are inlined in the template -- no standalone Model CRD).
// The platform default provider references the cubepilot-llm credential Secret
// created by setup.sh; its endpoint and model name come from the operator
// config (config.LLMEndpoint / config.LLMModel) and can be edited on the CR
// after install.
func BuiltinProviders(endpoint, modelName string) []v1alpha1.TemplateProviderSpec {
	return []v1alpha1.TemplateProviderSpec{
		{
			Name:          BuiltinProviderName,
			Endpoint:      endpoint,
			CredentialRef: &corev1.LocalObjectReference{Name: "cubepilot-llm"},
			Models:        []string{modelName},
		},
	}
}

// BuiltinAgentTemplate returns the builtin cubepilot template
// (design §3.1), with the platform default model at the given endpoint and
// model name.
func BuiltinAgentTemplate(endpoint, modelName string) *v1alpha1.AgentTemplate {
	return &v1alpha1.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name: BuiltinAgentName,
			Labels: map[string]string{
				"app.kubernetes.io/part-of": "cubepilot",
				"cubepilot/builtin":         "true",
			},
		},
		Spec: v1alpha1.AgentTemplateSpec{
			DisplayName:    "Platform Management Assistant",
			Description:    "Default assistant for managing the CubeStack platform (ChatOps + inspection + reporting)",
			Runtime:        v1alpha1.RuntimeOpenClaw,
			DefaultModel:   gateway.ModelKey(BuiltinProviderName, modelName),
			Providers:      BuiltinProviders(endpoint, modelName),
			ApprovalPolicy: v1alpha1.ApprovalPolicyAllowlist,
			Instructions: "You are the intelligent assistant of the CubeStack platform (CubePilot)." +
				"Use kubectl to query and operate cluster resources; run read-only operations directly, " +
				"and state the action and its blast radius before running write operations. Inspection and reporting use structured output.",
			Skills: BuiltinSkills,
		},
	}
}

// BuiltinTaskTemplates returns every preset task template, in the order the
// Portal's Templates tab lists them: the nightly sweep first, then the
// on-demand checks. The presets are starting points rather than a closed set --
// an operator can add TaskTemplate CRs beside them, and a preset is only ever
// created when it is missing.
//
// daily-inspection is the one preset with a schedule of its own.
// cluster-health-check is deliberately the lighter, on-demand triage of the
// same cluster, so the two are not duplicates.
//
// An upgrade pre-check is deliberately not among them: it needs cluster-scoped
// reads the agent's identity does not carry (nodes, CRDs) plus a target release
// to check against, so shipping it as a preset would only produce runs that
// report Forbidden.
func BuiltinTaskTemplates() []*v1alpha1.TaskTemplate {
	return []*v1alpha1.TaskTemplate{
		dailyInspectionTemplate(),
		clusterHealthCheckTemplate(),
		gpuInspectionTemplate(),
		inferenceValidationTemplate(),
		modelDeploymentCheckTemplate(),
		resourceAnalysisTemplate(),
	}
}

// builtinTaskTemplateMeta is the ObjectMeta every preset shares. The namespace
// is filled in by the bootstrap reconciler from the operator config.
func builtinTaskTemplateMeta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name: name,
		Labels: map[string]string{
			"app.kubernetes.io/part-of": "cubepilot",
			"cubepilot/builtin":         "true",
		},
	}
}

// dailyInspectionTemplate is the nightly full sweep (design §3.3.2): everything
// the platform checks on its own, with an evidence chain per finding.
func dailyInspectionTemplate() *v1alpha1.TaskTemplate {
	return &v1alpha1.TaskTemplate{
		ObjectMeta: builtinTaskTemplateMeta(BuiltinTaskTemplateName),
		Spec: v1alpha1.TaskTemplateSpec{
			DisplayName: "Daily Cluster Inspection",
			Description: "Preset inspection: nodes / Pods / storage / platform components + AI smart inspection",
			Instruction: `Inspect the cluster in read-only mode (get/list/watch/logs):
1. Check node Ready status and pressure (Disk / Mem / PID)
2. Check GPU health and utilization (nvidia.com/gpu)
3. Check abnormal Pods (CrashLoopBackOff / Pending / ImagePullBackOff / OOM)
4. Check storage (PVC usage)
5. Check platform component health (Harbor / Keycloak / Prometheus)
Attach an evidence chain to any finding, classify by P0/P1/P2; no write operations allowed.
Inspection scope: {{scope}} within {{target}} -- all covers every node and namespace, node-pool covers one node pool, project covers one project namespace; target names that pool or namespace, or is all to cover every one of them.`,
			ParamsSchema: []v1alpha1.ParamSchema{
				{Name: "scope", Default: "all", Enum: []string{"all", "node-pool", "project"}},
				{Name: "target", Default: "all"},
			},
			RequiredPermissions: &v1alpha1.RequiredPermissions{
				Level: "cluster-read",
				Note:  "Full-cluster inspection requires the creator to have cluster-level read permission",
			},
			Skills:      []string{"cluster-inspection"},
			DefaultCron: "0 2 * * *",
		},
	}
}

// clusterHealthCheckTemplate triages what is wrong right now, without the
// nightly sweep's storage and platform-component depth. It carries no default
// schedule: it is the one you run when something already looks wrong.
func clusterHealthCheckTemplate() *v1alpha1.TaskTemplate {
	return &v1alpha1.TaskTemplate{
		ObjectMeta: builtinTaskTemplateMeta("cluster-health-check"),
		Spec: v1alpha1.TaskTemplateSpec{
			DisplayName: "Cluster Health Check",
			Description: "On-demand triage: node conditions, control plane, abnormal Pods and warning events, most severe first",
			Instruction: `Triage the cluster as it is right now, read-only (get/list/watch/logs):
1. Nodes: Ready condition, plus MemoryPressure / DiskPressure / PIDPressure / NetworkUnavailable
2. Control plane: apiserver / scheduler / controller-manager / etcd Pods Running, and how often each restarted
3. Abnormal Pods in every namespace (CrashLoopBackOff / Pending / ImagePullBackOff / OOMKilled / Error)
4. Recent Warning events, newest first, with the count of each
5. Add-ons the cluster cannot run without: CNI, CoreDNS, ingress, GPU device plugin
Report only what is wrong right now, most severe first, each with the command output that shows it.
Inspection scope: {{scope}} within {{target}} -- all covers every node and namespace, node-pool covers one node pool, project covers one project namespace; target names that pool or namespace, or is all to cover every one of them. No write operations allowed.`,
			ParamsSchema: []v1alpha1.ParamSchema{
				{Name: "scope", Default: "all", Enum: []string{"all", "node-pool", "project"}},
				{Name: "target", Default: "all"},
			},
			RequiredPermissions: &v1alpha1.RequiredPermissions{
				Level: "cluster-read",
				Note:  "Node conditions and cluster-wide events require cluster-level read permission",
			},
			Skills: []string{"cluster-inspection"},
		},
	}
}

// gpuInspectionTemplate inspects GPU nodes and the workloads holding their
// GPUs, going past the single GPU line of the nightly sweep.
//
// The instruction carries the method rather than delegating it to a skill: the
// resource name has to be discovered rather than assumed, and the request sum
// is a jq trap worth stating outright. Both are the difference between a report
// and a plausible-looking wrong one.
func gpuInspectionTemplate() *v1alpha1.TaskTemplate {
	return &v1alpha1.TaskTemplate{
		ObjectMeta: builtinTaskTemplateMeta("gpu-inspection"),
		Spec: v1alpha1.TaskTemplateSpec{
			DisplayName: "GPU Node Inspection",
			Description: "Per GPU node: inventory, allocatable vs. allocated, device plugin health, stuck allocations and hardware errors",
			Instruction: `Inspect every GPU node and the workloads holding its GPUs, read-only (get/list/watch/logs).

Find the GPU extended resource name this cluster actually uses before anything else -- vendors differ, and reading a Metax cluster through an nvidia name reports zero cards:
  kubectl get nodes -o json | jq -r '.items[].status.allocatable | keys[]' | grep -i gpu | sort -u
Call that name $RES below. On a mixed cluster it prints one name per vendor: run the per-resource steps once for each, and report per resource rather than summing unrelated accelerators into one figure.

1. Inventory: the nodes where .status.allocatable[$RES] is set, and how many GPUs each carries
2. Allocated: sum the container requests for $RES per node. Quantities arrive as JSON strings and jq's add concatenates them, so convert with tonumber first -- two containers asking for "1" each otherwise report "11". GPUs are extended resources, so their quantities are always whole numbers. A node whose requests exceed its allocatable is over-committed
3. Device plugin: the device-plugin Pod on each GPU node is present, Running and not restarting; read its log for registration failures
4. Scheduling: GPU node taints, and whether an unschedulable node is holding GPUs nothing can use
5. Stuck allocations: Pods Pending on insufficient GPU, and Pods holding a GPU while not Running
6. Hardware errors: GPU errors reported as node events, or by a node problem detector / DCGM exporter if the cluster runs one. If neither is present this check cannot be made -- say so instead of inferring hardware health from Pods being Running
Vendor: {{vendor}}. Report per node, with the command output behind each figure, classify by P0/P1/P2, and flag every GPU that is allocated but not usable. State plainly which checks could not be made from this identity. No write operations allowed.`,
			ParamsSchema: []v1alpha1.ParamSchema{
				{Name: "vendor", Default: "all", Enum: []string{"all", "nvidia", "metax"}},
			},
			RequiredPermissions: &v1alpha1.RequiredPermissions{
				Level: "cluster-read",
				Note:  "Node status and Pods in every namespace must be readable; the device plugin runs in a platform namespace",
			},
			Skills:      []string{"cluster-inspection", "kubectl-platform"},
			DefaultCron: "0 3 * * *",
		},
	}
}

// inferenceValidationTemplate proves a deployed service actually serves, rather
// than only that its Pods are Running.
//
// The instruction carries the method: a service that is up but answers nothing
// looks identical to a healthy one until a request is actually sent, so the
// checks are ordered to stop at the first real failure.
func inferenceValidationTemplate() *v1alpha1.TaskTemplate {
	return &v1alpha1.TaskTemplate{
		ObjectMeta: builtinTaskTemplateMeta("inference-validation"),
		Spec: v1alpha1.TaskTemplateSpec{
			DisplayName: "Inference Service Validation",
			Description: "Validate a running InferenceService end to end: references bound, replicas ready, endpoint answering, one real request",
			Instruction: `Validate the InferenceService resources and prove they actually serve, read-only against the cluster plus one inference request. Stop at the first step that fails and report that step, not its symptoms.

1. References: the service's modelRef (ModelVersion) and profileRef (InferenceRuntimeProfile) both exist and resolve
2. Workload: every role the profile declares has its ready replicas. Read the Pod's State and Last State first -- OOMKilled and CrashLoopBackOff point at the model or the engine, a Pending Pod at capacity
3. Readiness: the profile's readinessPolicy is satisfied before the service is treated as up
4. Endpoint: the Service exposes the profile's endpoint portName. A mismatch looks exactly like a dead service
5. Functional: send one real request using route.modelName and record the latency. Read the response, not only the status code -- a 200 carrying an error body, an empty choices, or a stream that never ends is a failure
6. Route: whether route.publish makes the service reachable from outside

Reaching the endpoint takes an address published by route.publish, or the Service DNS name if you are inside the cluster, or -- failing both -- running a Pod. That last one is a write: state the command and its blast radius and wait for approval. If it is not approved, report the functional check as not performed, never as a pass.
Namespace: {{namespace}}. Report per service: pass, or the first failing step with the output that shows it. A service that is Running but answers nothing is a failure, not a pass.`,
			ParamsSchema: []v1alpha1.ParamSchema{
				{Name: "namespace", Default: "all"},
			},
			RequiredPermissions: &v1alpha1.RequiredPermissions{
				Level: "cluster-read",
				Note:  "Reading model and profile CRs plus the workload in the service's namespace requires read permission there",
			},
			Skills:      []string{"cubestack-platform"},
			DefaultCron: "0 4 * * *",
		},
	}
}

// modelDeploymentCheckTemplate is the pre-flight: whether a model can be
// deployed at all, before anything is applied.
func modelDeploymentCheckTemplate() *v1alpha1.TaskTemplate {
	return &v1alpha1.TaskTemplate{
		ObjectMeta: builtinTaskTemplateMeta("model-deployment-check"),
		Spec: v1alpha1.TaskTemplateSpec{
			DisplayName: "Model Deployment Check",
			Description: "Pre-flight a deployment before it is applied: model storage reachable, runtime profile accepts the model, GPU capacity available",
			Instruction: `Check whether a model can be deployed, before anything is applied, read-only. Model filter: {{model}} (all = every ModelVersion).
1. Model: a ModelVersion exists with architecture, quantization, version and storage.strategy set, and that storage backend (HostPath / Dynamic / Static / S3) is actually reachable from the cluster
2. Profile: an InferenceRuntimeProfile whose accelerator.vendor and accelerator.models match a GPU model the cluster really has, and whose modelRequirements.architectures and modelRequirements.quantization admit this model
3. Engine: the profile's engine.name and engine.version are versions this cluster can pull
4. Capacity: enough free GPUs of that model on nodes the profile's scheduling rules and the target namespace can use
5. Data path: for S3 storage the referenced endpoint and credential Secret are present; for HostPath the path exists on the target nodes
6. Quota: the namespace's ResourceQuota and LimitRange admit the resources the profile declares
Report pass / fail / unknown per check with the evidence, and for each failure state exactly what has to change for it to pass. No write operations allowed.`,
			ParamsSchema: []v1alpha1.ParamSchema{
				{Name: "model", Default: "all"},
			},
			RequiredPermissions: &v1alpha1.RequiredPermissions{
				Level: "cluster-read",
				Note:  "ModelVersion and InferenceRuntimeProfile are cluster-scoped; the namespace's quota and storage also have to be readable",
			},
			Skills: []string{"cubestack-platform"},
		},
	}
}

// resourceAnalysisTemplate reports capacity and utilization over a window, and
// ends on the capacity risks rather than the raw figures.
func resourceAnalysisTemplate() *v1alpha1.TaskTemplate {
	return &v1alpha1.TaskTemplate{
		ObjectMeta: builtinTaskTemplateMeta("resource-analysis"),
		Spec: v1alpha1.TaskTemplateSpec{
			DisplayName: "Cluster Resource Analysis",
			Description: "Capacity and utilization over a window: headroom per node pool, top consumers, idle GPUs and fragmentation",
			Instruction: `Analyse cluster capacity and utilization, read-only, and report what the platform team has to act on. Window: {{window}}.
1. Committed vs. allocatable: per node and per node pool, the sum of container requests and limits for CPU, memory and GPU against what each node allocates
2. Headroom: how many more Pods of the common shape each pool takes before it runs out of requests
3. Top consumers: the namespaces and workloads requesting the most CPU / memory / GPU, and those requesting a lot while using little
4. GPU: GPUs allocated to workloads that are not using them, and GPU nodes running no GPU workload at all
5. Fragmentation: nodes whose free capacity is too small to fit anything useful, and Pods Pending for capacity rather than for a missing scheduler
6. Trend: if the platform's Prometheus is reachable, repeat the key figures across the window and say what is growing; if it is not, say the trend part could not be measured
Report every figure with the command or query that produced it, and finish with the concrete capacity risks. No write operations allowed.`,
			ParamsSchema: []v1alpha1.ParamSchema{
				{Name: "window", Default: "7d", Enum: []string{"24h", "7d", "30d"}},
			},
			RequiredPermissions: &v1alpha1.RequiredPermissions{
				Level: "cluster-read",
				Note:  "Node capacity and every namespace's Pods must be readable; the trend part additionally needs Prometheus access",
			},
			Skills:      []string{"cluster-inspection", "kubectl-platform"},
			DefaultCron: "0 8 * * 1",
		},
	}
}

// BuiltinBootstrapReconciler reconciles the builtin platform objects: the
// cubepilot Agent definition, the preset Skills and the preset TaskTemplates.
// It also instantiates the builtin agent for
// every configured user (auto-instantiated per user, design §3.1 / §5.3),
// reconciling the
// AgentInstance CRs on every tick (idempotent: already-present objects are
// left untouched; missing ones are created).
type BuiltinBootstrapReconciler struct {
	client.Client
	// APIReader reads straight from the API server, past the manager's cache.
	// Writing the per-user kubeconfig Secret needs it: see ensureSecretData.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Cfg       config.Config
}

// +kubebuilder:rbac:groups=ai.cubestack.io,resources=agenttemplates;skills;tasktemplates;agentinstances;tasks;taskruns,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ai.cubestack.io,resources=agentinstances/status;skills/status;tasks/status;taskruns/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=serviceaccounts;secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=get;list;watch;bind,resourceNames=view;cubepilot-cluster-read;cubepilot-user-crds

// Reconcile ensures the builtin objects exist (create-if-missing).
func (r *BuiltinBootstrapReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	if err := r.ensureBuiltin(ctx); err != nil {
		log.Printf("bootstrap: %v", err)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// Ensure runs the full bootstrap once (called at startup).
func (r *BuiltinBootstrapReconciler) Ensure(ctx context.Context) error {
	return r.ensureBuiltin(ctx)
}

func (r *BuiltinBootstrapReconciler) ensureBuiltin(ctx context.Context) error {
	// 1. AgentTemplate definition (with inline providers, design §3.1/§3.3).
	// The platform default provider is included only when an endpoint AND model
	// name are configured (CUBEPILOT_LLM_ENDPOINT / CUBEPILOT_LLM_MODEL); an
	// empty pair ships the template provider-less and LLMs are added from the
	// Portal (Agent Config -> LLM Config). "有就是有，没有就是没有": the provider
	// list is fixed at template creation and never re-synced afterwards.
	agent := BuiltinAgentTemplate(r.Cfg.LLMEndpoint, r.Cfg.LLMModel)
	if r.Cfg.LLMEndpoint == "" || r.Cfg.LLMModel == "" {
		agent.Spec.Providers = nil
		agent.Spec.DefaultModel = ""
	}
	agent.Namespace = r.Cfg.Namespace
	if err := r.createIfMissing(ctx, agent); err != nil {
		return err
	}
	// 2. Task templates: the preset catalog, created where missing. One that is
	// already present is left exactly as it is, so an operator's edit to a
	// preset is never overwritten -- a preset deleted on purpose does come back
	// on the next tick, which is the create-if-missing contract.
	for _, taskTemplate := range BuiltinTaskTemplates() {
		taskTemplate.Namespace = r.Cfg.Namespace
		if err := r.createIfMissing(ctx, taskTemplate); err != nil {
			return err
		}
	}
	// 3. Per-user builtin agent instances (auto-instantiated per user;
	// resident). Reject identities that would collide on the derived per-user
	// name before provisioning (e.g. "zhang.wei" vs "Zhang Wei").
	seenIdentity := map[string]string{}
	for _, user := range r.Cfg.Users {
		saName := k8s.UserServiceAccountName(user)
		if prev, dup := seenIdentity[saName]; dup {
			return fmt.Errorf("users %q and %q collide on identity %s", prev, user, saName)
		}
		seenIdentity[saName] = user
	}
	for _, user := range r.Cfg.Users {
		// Platform-generated per-user identity first: SA + view/CRD ClusterRole
		// bindings + a kubeconfig Secret the agent mounts as its default
		// credentials (issue #19). Zero operator/admin-supplied kubeconfig. The
		// identity must exist before the AgentInstance is created: the
		// AgentInstance controller requires the per-user kubeconfig Secret
		// before it creates the Pod (issue #100 -- no placeholder-identity Pod),
		// so creating the instance first would leave it waiting on an identity
		// this pass only mints afterwards.
		if err := r.ensurePerUserKubeconfigAccess(ctx, user); err != nil {
			return err
		}
		inst := &v1alpha1.AgentInstance{
			ObjectMeta: metav1.ObjectMeta{
				Name:      InstanceNameFor(user, BuiltinAgentName),
				Namespace: r.Cfg.Namespace,
				Labels:    map[string]string{"cubepilot/builtin": "true"},
			},
			Spec: v1alpha1.AgentInstanceSpec{
				TemplateRef: BuiltinAgentName,
				Owner:       user,
			},
		}
		if err := r.createIfMissing(ctx, inst); err != nil {
			return err
		}
	}
	return nil
}

// ensurePerUserKubeconfigAccess mints the per-user identity (issue #19): a
// namespaced ServiceAccount, ClusterRoleBindings to `view` and
// `cubepilot-user-crds`, and a kubeconfig Secret (SA token inlined) under
// k8s.UserKubeconfigSecretFor so the AgentInstance controller's existing
// dual-kubeconfig mount picks it up unchanged. Idempotent; when the token
// Secret's token is not yet populated (API server fills it asynchronously) it
// returns an error so the reconcile requeues rather than writing a broken
// kubeconfig.
func (r *BuiltinBootstrapReconciler) ensurePerUserKubeconfigAccess(ctx context.Context, user string) error {
	saName := k8s.UserServiceAccountName(user)
	builtinLabels := map[string]string{"cubepilot/builtin": "true"}
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: r.Cfg.Namespace, Labels: builtinLabels},
	}
	if err := r.createIfMissing(ctx, sa); err != nil {
		return err
	}

	for _, role := range userClusterRoles {
		crb := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: userCRBName(user, role), Labels: builtinLabels},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role},
			Subjects: []rbacv1.Subject{{
				Kind:      "ServiceAccount",
				Name:      saName,
				Namespace: r.Cfg.Namespace,
			}},
		}
		if err := r.createIfMissing(ctx, crb); err != nil {
			return err
		}
	}

	// Token: a legacy service-account-token Secret (the API server writes the
	// token back asynchronously). Read the token; if not yet present, requeue.
	tokenSecretName := saName + "-token"
	tok := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        tokenSecretName,
			Namespace:   r.Cfg.Namespace,
			Labels:      builtinLabels,
			Annotations: map[string]string{corev1.ServiceAccountNameKey: saName},
		},
		Type: corev1.SecretTypeServiceAccountToken,
	}
	if err := r.createIfMissing(ctx, tok); err != nil {
		return err
	}
	var got corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: r.Cfg.Namespace, Name: tokenSecretName}, &got); err != nil {
		return err
	}
	token := string(got.Data[corev1.ServiceAccountTokenKey])
	if token == "" {
		return fmt.Errorf("per-user token for %s not ready yet (token secret %s)", user, tokenSecretName)
	}

	// Kubeconfig Secret consumed by the AgentInstance controller (PR #94). It is
	// rewritten when the token it was minted from changed, so a recreated token
	// Secret refreshes the kubeconfig instead of leaving a stale one behind.
	kc := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: k8s.UserKubeconfigSecretFor(user), Namespace: r.Cfg.Namespace, Labels: builtinLabels},
		Data:       map[string][]byte{"config": k8s.PerUserKubeconfigYAML(token)},
	}
	if err := ensureSecretData(ctx, r.Client, r.APIReader, kc); err != nil {
		return err
	}
	return nil
}

func (r *BuiltinBootstrapReconciler) createIfMissing(ctx context.Context, obj client.Object) error {
	key := types.NamespacedName{Name: obj.GetName()}
	if obj.GetNamespace() != "" {
		key.Namespace = obj.GetNamespace()
	}
	if err := r.Get(ctx, key, obj.DeepCopyObject().(client.Object)); err == nil {
		return nil // already present -- leave untouched (idempotent)
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	if err := r.Create(ctx, obj); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return err
	}
	log.Printf("bootstrap: created %s/%s", r.kindOf(obj), obj.GetName())
	return nil
}

// kindOf resolves an object's Kind through the scheme.
//
// GetObjectKind().GroupVersionKind().Kind is empty for the objects bootstrapped
// here: they are built as typed Go literals with no TypeMeta, so the method
// returns "" and the log line reads "bootstrap: created /admin-cubepilot". The
// scheme knows the type even when the object does not.
func (r *BuiltinBootstrapReconciler) kindOf(obj client.Object) string {
	if kinds, _, err := r.Scheme.ObjectKinds(obj); err == nil && len(kinds) > 0 {
		return kinds[0].Kind
	}
	return "unknown"
}

// InstanceNameFor builds the AgentInstance name for (user, agent) -- the
// instance key is user + agent (design §3.2). Both segments are DNS-1123
// sanitized (consistent with the k8s package resource naming).
func InstanceNameFor(user, agent string) string {
	return k8s.Sanitize(user) + "-" + k8s.Sanitize(agent)
}

// SetupWithManager registers the bootstrap reconciler.
func (r *BuiltinBootstrapReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("builtin-bootstrap").
		For(&v1alpha1.AgentTemplate{}).
		Complete(r)
}
