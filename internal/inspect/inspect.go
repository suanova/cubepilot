// Package inspect drives the minimal M4 scheduled-task flow: a fixed inspection
// prompt run through the agent loop, producing a natural-language report.
package inspect

import (
	"context"
	"strings"

	"github.com/suanova/cubepilot/internal/openclaw"
)

const prompt = `请对当前 Kubernetes 集群执行一次基础巡检：
1. 查看节点状态（kubectl get nodes）；
2. 查找所有命名空间中状态异常的 Pod（非 Running，如 CrashLoopBackOff / Pending / ImagePullBackOff / OOMKilled）；
3. 查看最近的集群事件（kubectl get events -A）。
将发现的异常按严重程度分级：P0 紧急 / P1 重要 / P2 一般，并用简体中文输出一份结构化巡检报告（含每项的证据）。

【可信度约束】
- 每项发现必须附证据链：执行的命令 + 原始输出摘录 + 时间戳。
- 无法确认的疑似发现必须标注「AI 疑似，需人工复核」，不得当作既定事实。
- 同一异常不重复报告；噪声（偶发重启、非关键事件）过滤掉或归入 P2。
- 本巡检为只读操作：只执行查询命令，禁止任何写操作（apply/delete/scale/create）。
【只读提示】若你的凭据被 RBAC 拒绝，如实说明权限范围，不重试被拒操作。`

// Run executes the inspection prompt against the given agent instance and
// returns the agent's final natural-language report text. onEvent (optional)
// receives every stream event so callers can record tool calls for audit.
func Run(ctx context.Context, oc openclaw.AgentRuntime, sessionKey string, onEvent func(openclaw.Event)) (string, error) {
	var buf strings.Builder
	err := oc.StreamChat(ctx, openclaw.ChatParams{
		SessionKey: sessionKey,
		Messages:   []openclaw.ChatMessage{{Role: "user", Content: prompt}},
	}, func(ev openclaw.Event) error {
		if onEvent != nil {
			onEvent(ev)
		}
		if ev.Type == openclaw.EventMessageDelta {
			buf.WriteString(ev.Delta)
		}
		return nil
	})
	return buf.String(), err
}

// Prompt returns the fixed inspection prompt (also used as the preset for the
// cluster-health-inspection task template on the tasks page).
func Prompt() string { return prompt }
