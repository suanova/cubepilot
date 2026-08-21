package controller

import (
	"context"
	"fmt"
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

// BuiltinAgentName is the preset platform agent (design §5.1: agent-for-cloud
// is the first platform-preset Agent, auto-instantiated per user, non-deletable).
const BuiltinAgentName = "agent-for-cloud"

// BuiltinTaskTemplateName is the preset inspection task template
// (design §3.3.2 the preset inspection template daily-inspection).
const BuiltinTaskTemplateName = "daily-inspection"

// BuiltinCapabilities are the preset domain capabilities the builtin agent
// references (design §3.3.1 domain layer). Generated from the embedded
// SKILL.md files: the agent gets exactly the skills the platform ships.
var BuiltinCapabilities = func() []string {
	names, err := presetCapabilityNames()
	if err != nil {
		return []string{"cluster-inspection"}
	}
	return names
}()

// BuiltinModels returns the preset platform model catalog entries
// (design §3.3: platform preloads deepseek-v4-flash etc; admins add more via
// `kubectl apply` a Model CRD — no running instance is touched).
func BuiltinModels() []*v1alpha1.Model {
	return []*v1alpha1.Model{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "deepseek-v4-flash",
				Labels: map[string]string{
					"app.kubernetes.io/part-of": "cubepilot",
					"cubepilot/builtin":         "true",
				},
			},
			Spec: v1alpha1.ModelSpec{
				DisplayName: "DeepSeek V4 Flash",
				Provider:    v1alpha1.ModelProviderPlatform,
				ModelID:     "deepseek/deepseek-v4-flash",
			},
		},
	}
}

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
			DisplayName:     "平台管理助手",
			Description:     "管理 CubeStack 平台的默认助手（ChatOps + 巡检 + 报告）",
			Runtime:         v1alpha1.RuntimeOpenClaw,
			DefaultModel:    "deepseek-v4-flash",
			AvailableModels: []string{"deepseek-v4-flash"},
			ConfirmPolicy:   v1alpha1.ConfirmPolicyConfirmWrites,
			Model: []v1alpha1.AgentModelSpec{
				{Provider: "Platform", Name: "deepseek-v4-flash"},
			},
			Instructions: "你是 CubeStack 平台的智能助手（agent-for-cloud）。" +
				"通过 kubectl 查询与操作集群资源；只读操作直接执行，" +
				"写操作先说明动作与影响范围再执行。巡检与报告使用结构化输出。",
			Capabilities: BuiltinCapabilities,
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

// BuiltinCapabilityDefinitions is generated from the embedded SKILL.md files
// (see skill_source.go) — one source of truth for preset domain capabilities.

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
			Capabilities: []string{"cluster-inspection"},
			DefaultCron:  "0 2 * * *",
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
// every configured user (auto-instantiated per user, design §3.1 / §5.3),
// reconciling the
// AgentInstance CRs on every tick (idempotent: already-present objects are
// left untouched; missing ones are created).
type BuiltinBootstrapReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Cfg    config.Config
}

// +kubebuilder:rbac:groups=assistant.suanova.io,resources=agents;capabilities;tasktemplates;agentinstances;tasks;taskruns;models,verbs=get;list;watch;create;update;patch;delete
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
	// 0. Model catalog (design §3.3).
	for _, m := range BuiltinModels() {
		if err := r.createIfMissing(ctx, m); err != nil {
			return err
		}
	}
	// 1. Agent definition.
	if err := r.createIfMissing(ctx, BuiltinAgent()); err != nil {
		return err
	}
	// 2. Domain capabilities (generated from embedded SKILL.md).
	caps, err := BuiltinCapabilityDefinitions()
	if err != nil {
		return fmt.Errorf("capability definitions: %w", err)
	}
	for _, cap := range caps {
		if err := r.createIfMissing(ctx, cap); err != nil {
			return err
		}
	}
	// 3. Task template.
	if err := r.createIfMissing(ctx, BuiltinTaskTemplate()); err != nil {
		return err
	}
	// 4. Per-user builtin agent instances (auto-instantiated per user;
	// resident).
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

// InstanceNameFor builds the AgentInstance name for (user, agent) — the
// instance key is user + agent (design §3.2). Both segments are DNS-1123
// sanitized (consistent with the k8s package resource naming).
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
