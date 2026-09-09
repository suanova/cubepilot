package ws

import (
	"encoding/json"
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
