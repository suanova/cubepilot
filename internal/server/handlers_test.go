package server

import (
	"encoding/json"
	"fmt"
	"testing"

	agentruntime "github.com/suanova/cubepilot/internal/runtime"
)

// TestLiveTurnDoneIsTheSingleTerminalFrame pins what a finished turn puts on
// the wire: exactly one message_done, carrying either the error text or the
// stopped flag and never both. A stopped turn must not be byte-identical to a
// completed one, or the browser renders partial text as a finished answer.
func TestLiveTurnDoneIsTheSingleTerminalFrame(t *testing.T) {
	cases := []struct {
		name    string
		outcome agentruntime.TurnOutcome
		err     error
		want    string
	}{
		{"completed", agentruntime.TurnOutcome{}, nil, `{"type":"message_done","sessionId":"conv-1"}`},
		{"stopped", agentruntime.TurnOutcome{Stopped: true}, nil, `{"type":"message_done","sessionId":"conv-1","stopped":true}`},
		{"failed", agentruntime.TurnOutcome{}, fmt.Errorf("boom"), `{"type":"message_done","sessionId":"conv-1","error":"boom"}`},
		{"error wins over a stale stop", agentruntime.TurnOutcome{Stopped: true}, fmt.Errorf("boom"), `{"type":"message_done","sessionId":"conv-1","error":"boom"}`},
	}
	for _, c := range cases {
		raw, err := json.Marshal(liveTurnDone("conv-1", c.outcome, c.err))
		if err != nil {
			t.Fatalf("%s: marshal: %v", c.name, err)
		}
		if string(raw) != c.want {
			t.Errorf("%s: got %s, want %s", c.name, raw, c.want)
		}
	}
}
