package ws

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestSessionPatchParamsEncodeOmittedValueAndExplicitNull(t *testing.T) {
	params := sessionPatchParams{
		Key:            "agent:main:conv-1",
		Model:          stringField(""),
		PermissionMode: stringField("guarded"),
	}
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if model, ok := got["model"]; !ok || model != nil {
		t.Fatalf("model = %#v, present = %v; want explicit null", model, ok)
	}
	if got["permissionMode"] != "guarded" {
		t.Fatalf("permissionMode = %#v, want guarded", got["permissionMode"])
	}
	if _, ok := got["unexpected"]; ok {
		t.Fatal("unexpected field encoded")
	}

	omitted, err := json.Marshal(sessionPatchParams{Key: "agent:main:conv-1"})
	if err != nil {
		t.Fatal(err)
	}
	var gotOmitted map[string]any
	if err := json.Unmarshal(omitted, &gotOmitted); err != nil {
		t.Fatal(err)
	}
	if _, ok := gotOmitted["model"]; ok {
		t.Fatal("unset model was not omitted")
	}
	if _, ok := gotOmitted["permissionMode"]; ok {
		t.Fatal("unset permissionMode was not omitted")
	}
}

// The status read must never pull the transcript: chat.history answers with one
// frame carrying everything asked for, so an unbounded request is what makes
// /turn and the abort liveness wait depend on the conversation's length. This
// asserts the request that goes on the wire, not just the decode, because the
// cost is in what the gateway is asked to send.
func TestSessionRunStatusRequestsOnlyOneMessage(t *testing.T) {
	c := &Client{}
	var gotMethod string
	var gotParams map[string]any
	c.callFn = func(_ context.Context, method string, params any) (json.RawMessage, error) {
		gotMethod = method
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &gotParams); err != nil {
			return nil, err
		}
		return json.RawMessage(`{"inFlightRun":{"runId":"run-1"}}`), nil
	}

	out, err := c.sessionRunStatus(context.Background(), "agent:main:conv-1")
	if err != nil {
		t.Fatalf("sessionRunStatus: %v", err)
	}
	if gotMethod != "chat.history" {
		t.Fatalf("method = %q, want chat.history", gotMethod)
	}
	// Exactly two fields, and no others. This is the narrowness that keeps the
	// frame transcript-independent: chat.history accepts cursor, offset, maxChars
	// and friends, and any one of them added here would let the read grow with
	// the conversation again -- which is the failure this read was fixed for.
	if len(gotParams) != 2 {
		t.Fatalf("request carries %d fields (%v), want sessionKey and limit only", len(gotParams), gotParams)
	}
	if gotParams["sessionKey"] != "agent:main:conv-1" {
		t.Fatalf("sessionKey = %#v", gotParams["sessionKey"])
	}
	limit, ok := gotParams["limit"]
	if !ok {
		t.Fatal("limit was omitted: the gateway would send the whole transcript")
	}
	if limit != float64(sessionRunStatusLimit) {
		t.Fatalf("limit = %#v, want %d", limit, sessionRunStatusLimit)
	}
	// inFlightRun is the whole point of the read and must survive the cap: the
	// gateway computes it from run state rather than from the paged transcript.
	if !hasInFlightRun(out.InFlightRun) {
		t.Fatalf("inFlightRun = %q, want the gateway's snapshot", out.InFlightRun)
	}
}

// The status read hands back only the run descriptor. Nothing else chat.history
// returns is declared on the result type, so a caller cannot reach for the
// transcript through it -- the shape itself is the guard, not just the name.
func TestSessionRunStatusResultCarriesNoTranscript(t *testing.T) {
	c := &Client{}
	c.callFn = func(_ context.Context, _ string, _ any) (json.RawMessage, error) {
		return json.RawMessage(`{
			"sessionKey": "agent:main:conv-1",
			"sessionId": "sess-1",
			"messages": [{"role": "user", "content": "hello"}],
			"deltaCursor": "eyJjdXJzb3IiOjF9",
			"totalMessages": 48,
			"inFlightRun": {"runId": "run-1", "text": "partial"}
		}`), nil
	}

	out, err := c.sessionRunStatus(context.Background(), "agent:main:conv-1")
	if err != nil {
		t.Fatalf("sessionRunStatus: %v", err)
	}
	if string(out.InFlightRun) == "" || !strings.Contains(string(out.InFlightRun), "run-1") {
		t.Fatalf("inFlightRun = %q, want the gateway's descriptor", out.InFlightRun)
	}
	// Exactly one field: a transcript or cursor that a later caller could read
	// would have to be added here, where this test fails.
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 1 {
		t.Fatalf("result carries %d fields (%v), want only inFlightRun", len(fields), fields)
	}
	if _, ok := fields["inFlightRun"]; !ok {
		t.Fatalf("result fields = %v, want inFlightRun", fields)
	}
}
