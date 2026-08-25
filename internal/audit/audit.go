// Package audit classifies tool calls seen on the agent stream into the M5
// audit model (L0 read-only passthrough / L1 write operation). Phase one
// executes writes directly (stage-1 write passthrough), so entries are
// recorded with status "executed".
package audit

import (
	"encoding/json"
	"strings"

	"github.com/suanova/cubepilot/internal/store"
)

// readOnlyKubectlVerbs are treated as L0 (read-only).
var readOnlyKubectlVerbs = map[string]bool{
	"get": true, "describe": true, "logs": true, "top": true,
	"api-resources": true, "api-versions": true, "version": true,
	"cluster-info": true, "explain": true, "config": true,
}

// writeShellTokens mark a non-kubectl shell command as L1.
var writeShellTokens = []string{"rm ", "rm\t", "mv ", "dd ", "mkfs", "shutdown", "reboot", "kill ", ">"}

// Entry builds the audit entry for a tool_call event (name + JSON arguments).
// Phase one only observes the exec tool, whose arguments carry {"command": ...}.
func Entry(user, sessionID, tool, argsJSON string) store.AuditEntry {
	command := extractCommand(argsJSON)
	return store.AuditEntry{
		User:      user,
		SessionID: sessionID,
		Tool:      toolDisplay(tool, command),
		Command:   command,
		Level:     classify(command),
		Status:    "executed",
	}
}

func extractCommand(argsJSON string) string {
	var args struct {
		Command string `json:"command"`
		Input   string `json:"input"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err == nil {
		if args.Command != "" {
			return args.Command
		}
		return args.Input
	}
	return argsJSON
}

// toolDisplay renders the tool column, e.g. "kubectl get pods" for an exec
// call whose command starts with kubectl.
func toolDisplay(tool, command string) string {
	fields := strings.Fields(command)
	for i, f := range fields {
		if f == "kubectl" || strings.HasSuffix(f, "/kubectl") {
			rest := fields[i+1:]
			if len(rest) > 2 {
				rest = rest[:2]
			}
			return "kubectl " + strings.Join(rest, " ")
		}
	}
	return tool
}

// classify returns L0 for read-only commands, L1 otherwise.
func classify(command string) string {
	fields := strings.Fields(command)
	for i, f := range fields {
		if f == "kubectl" || strings.HasSuffix(f, "/kubectl") {
			if i+1 < len(fields) {
				if readOnlyKubectlVerbs[strings.TrimPrefix(fields[i+1], "-")] {
					return "L0"
				}
			}
			return "L1"
		}
	}
	for _, tok := range writeShellTokens {
		if strings.Contains(command, tok) {
			return "L1"
		}
	}
	return "L0"
}
