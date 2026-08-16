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
将发现的异常按严重程度分级：P0 紧急 / P1 重要 / P2 一般，并用简体中文输出一份结构化巡检报告（含每项的证据）。`

// Run executes the inspection prompt against the given agent instance and
// returns the agent's final natural-language report text. onEvent (optional)
// receives every stream event so callers can record tool calls for audit.
func Run(ctx context.Context, oc *openclaw.Client, sessionKey string, onEvent func(openclaw.Event)) (string, error) {
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
// 集群健康巡检 task template on the tasks page).
func Prompt() string { return prompt }
