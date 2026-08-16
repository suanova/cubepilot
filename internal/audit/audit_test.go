package audit

import "testing"

func TestEntryClassification(t *testing.T) {
	cases := []struct {
		args      string
		wantLevel string
		wantTool  string
	}{
		{`{"command":"kubectl get pods -A"}`, "L0", "kubectl get pods"},
		{`{"command":"kubectl get nodes -o wide","timeout":15}`, "L0", "kubectl get nodes"},
		{`{"command":"kubectl describe pod x -n prod"}`, "L0", "kubectl describe pod"},
		{`{"command":"kubectl delete pod train-llama3-7b-0"}`, "L1", "kubectl delete pod"},
		{`{"command":"kubectl apply -f infer.yaml"}`, "L1", "kubectl apply -f"},
		{`{"command":"kubectl rollout restart deploy/x"}`, "L1", "kubectl rollout restart"},
		{`{"command":"kubectl logs agent-x | tail"}`, "L0", "kubectl logs agent-x"},
		{`{"command":"nvidia-smi"}`, "L0", "exec"},
		{`{"command":"rm -rf /tmp/x"}`, "L1", "exec"},
		{`not-json`, "L0", "exec"},
	}
	for _, c := range cases {
		e := Entry("zhang.wei", "conv-1", "exec", c.args)
		if e.Level != c.wantLevel {
			t.Errorf("Entry(%s).Level = %s, want %s", c.args, e.Level, c.wantLevel)
		}
		if e.Tool != c.wantTool {
			t.Errorf("Entry(%s).Tool = %q, want %q", c.args, e.Tool, c.wantTool)
		}
		if e.User != "zhang.wei" || e.SessionID != "conv-1" || e.Status != "executed" {
			t.Errorf("Entry(%s) identity/status wrong: %+v", c.args, e)
		}
	}
}
