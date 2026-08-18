package controller

import (
	"context"
	"log"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/k8s"
)

// BuiltinAgentName is the preset platform agent (设计 §5.1: agent-for-cloud 是
// 平台预置的第一个 Agent, 每用户自动实例化, 不可删除).
const BuiltinAgentName = "agent-for-cloud"

// BuiltinTaskTemplateName is the preset inspection task template
// (设计 §3.3.2 预置巡检模板 daily-inspection).
const BuiltinTaskTemplateName = "daily-inspection"

// BuiltinCapabilities are the preset domain capabilities the builtin agent
// references (design §3.3.1 domain 层; 模块必须登记).
var BuiltinCapabilities = []string{"cluster-inspection"}

// BuiltinAgent returns the builtin agent-for-cloud definition (design §3.1).
func BuiltinAgent() *v1alpha1.Agent {
	builtin := true
	return &v1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name: BuiltinAgentName,
			Labels: map[string]string{
				"app.kubernetes.io/part-of": "cubepilot",
				"cubepilot/builtin":         "true",
			},
		},
		Spec: v1alpha1.AgentSpec{
			DisplayName: "平台管理助手",
			Description: "管理 CubeStack 平台的默认助手（ChatOps + 巡检 + 报告）",
			Runtime:     v1alpha1.RuntimeOpenClaw,
			Model: []v1alpha1.ModelSpec{
				{Provider: "platform", Name: "deepseek-v4-flash"},
			},
			Instructions: "你是 CubeStack 平台的智能助手（agent-for-cloud）。" +
				"通过 kubectl 查询与操作集群资源；只读操作直接执行，" +
				"写操作先说明动作与影响范围再执行。巡检与报告使用结构化输出。",
			Tools: BuiltinCapabilities,
			Memory: &v1alpha1.MemorySpec{Enabled: true},
			Identity: &v1alpha1.AgentIdentitySpec{
				Mode:  v1alpha1.IdentityModeUser,
				Scope: "project-write",
			},
			Registry: &v1alpha1.AgentRegistrySpec{
				Builtin:    builtin,
				Visibility: "system",
			},
			Quotas: &v1alpha1.QuotaSpec{MaxInstancesPerUser: 1},
		},
	}
}

// BuiltinCapabilityDefinitions returns the preset domain capabilities
// (design §3.3.1 domain 示例: cluster-inspection 按「查节点 → 查 Pod → 查事件
// → 分级」巡检).
func BuiltinCapabilityDefinitions() []*v1alpha1.Capability {
	return []*v1alpha1.Capability{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-inspection"},
			Spec: v1alpha1.CapabilitySpec{
				Type:        v1alpha1.CapabilityDomain,
				Title:       "集群智能巡检",
				Description: "按「查节点 → 查异常 Pod → 查事件 → 分级归因」巡检集群健康",
				OwnerModule: "platform",
				Uses:        []string{"resource-manager", "kubectl-platform"},
				Instructions: `对当前 Kubernetes 集群执行一次只读巡检：
1. 检查节点 Ready 与压力（kubectl get nodes）；
2. 查找异常 Pod（CrashLoopBackOff / Pending / ImagePullBackOff / OOMKilled）；
3. 查看最近集群事件；
按 P0/P1/P2 分级输出结构化报告，每项附证据链（命令 + 输出摘录）。
只读操作，禁止任何写操作；被 RBAC 拒绝时如实说明，不重试被拒操作。`,
			},
		},
	}
}

// BuiltinTaskTemplate returns the preset daily-inspection template
// (design §3.3.2).
func BuiltinTaskTemplate() *v1alpha1.TaskTemplate {
	return &v1alpha1.TaskTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name: BuiltinTaskTemplateName,
			Labels: map[string]string{
				"app.kubernetes.io/part-of": "cubepilot",
				"cubepilot/builtin":         "true",
			},
		},
		Spec: v1alpha1.TaskTemplateSpec{
			DisplayName: "每日集群巡检",
			Description: "预置巡检：节点 / Pod / 存储 / 平台组件 + AI 智能巡检",
			Instruction: `以只读方式巡检集群（get/list/watch/logs）：
1. 检查节点 Ready 与压力（Disk / Mem / PID）
2. 检查 GPU 健康与利用率（nvidia.com/gpu）
3. 检查异常 Pod（CrashLoopBackOff / Pending / ImagePullBackOff / OOM）
4. 检查存储（PVC 使用率）
5. 检查平台组件健康（Harbor / Keycloak / Prometheus）
发现异常附证据链，按 P0/P1/P2 分级；禁止任何写操作。`,
			ParamsSchema: []v1alpha1.ParamSchema{
				{Name: "scope", Type: "string", Default: "all", Enum: []string{"all", "node-pool", "project"}},
			},
			RequiredPermissions: &v1alpha1.RequiredPermissions{
				Level: "cluster-read",
				Note:  "全集群巡检需创建者具备集群级只读权限",
			},
			Defaults: &v1alpha1.TaskTemplateDefaults{
				Trigger: v1alpha1.TaskTriggerCron,
				Cron:    "0 2 * * *",
			},
		},
	}
}

// BuiltinBootstrapReconciler reconciles the builtin platform objects: the
// agent-for-cloud Agent definition, the preset Capabilities and the
// daily-inspection TaskTemplate. It also instantiates the builtin agent for
// every configured user (每用户自动实例化, 设计 §3.1 / §5.3), reconciling the
// AgentInstance CRs on every tick (idempotent: already-present objects are
// left untouched; missing ones are created).
type BuiltinBootstrapReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Cfg    config.Config
}

// +kubebuilder:rbac:groups=assistant.suanova.io,resources=agents;capabilities;tasktemplates;agentinstances;tasks;taskruns,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=assistant.suanova.io,resources=agents/status;agentinstances/status;tasks/status;taskruns/status,verbs=get;update;patch

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
	// 1. Agent definition.
	if err := r.createIfMissing(ctx, BuiltinAgent()); err != nil {
		return err
	}
	// 2. Domain capabilities.
	for _, cap := range BuiltinCapabilityDefinitions() {
		if err := r.createIfMissing(ctx, cap); err != nil {
			return err
		}
	}
	// 3. Task template.
	if err := r.createIfMissing(ctx, BuiltinTaskTemplate()); err != nil {
		return err
	}
	// 4. Per-user builtin agent instances (每用户自动实例化; 常驻).
	for _, user := range r.Cfg.Users {
		inst := &v1alpha1.AgentInstance{
			ObjectMeta: metav1.ObjectMeta{
				Name:   InstanceNameFor(user, BuiltinAgentName),
				Labels: map[string]string{"cubepilot/builtin": "true"},
			},
			Spec: v1alpha1.AgentInstanceSpec{
				AgentRef: BuiltinAgentName,
				Owner:    user,
				Identity: v1alpha1.IdentitySpec{
					Mode: v1alpha1.IdentityModeUser,
					PrincipalRef: v1alpha1.PrincipalRef{
						UserRef: user,
					},
				},
				Lifecycle: &v1alpha1.LifecycleSpec{Strategy: "resident"},
			},
		}
		if err := r.createIfMissing(ctx, inst); err != nil {
			return err
		}
	}
	return nil
}

func (r *BuiltinBootstrapReconciler) createIfMissing(ctx context.Context, obj client.Object) error {
	key := types.NamespacedName{Name: obj.GetName()}
	if obj.GetNamespace() != "" {
		key.Namespace = obj.GetNamespace()
	}
	if err := r.Get(ctx, key, obj.DeepCopyObject().(client.Object)); err == nil {
		return nil // already present — leave untouched (idempotent)
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	if err := r.Create(ctx, obj); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return err
	}
	log.Printf("bootstrap: created %s/%s", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName())
	return nil
}

// InstanceNameFor builds the AgentInstance name for (user, agent) — 实例 key =
// user + agent (设计 §3.2). Both segments are DNS-1123 sanitized (consistent
// with the k8s package resource naming).
func InstanceNameFor(user, agent string) string {
	return k8s.Sanitize(user) + "-" + k8s.Sanitize(agent)
}

// SetupWithManager registers the bootstrap reconciler.
func (r *BuiltinBootstrapReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("builtin-bootstrap").
		For(&v1alpha1.Agent{}).
		Complete(r)
}
