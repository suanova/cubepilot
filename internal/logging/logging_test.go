package logging

import (
	"bytes"
	"errors"
	"regexp"
	"strings"
	"testing"
)

func newTestSink(level int) (*sink, *bytes.Buffer) {
	var buf bytes.Buffer
	return newSink(&buf, level), &buf
}

// out returns the rendered lines without the trailing newline log.Print adds.
func out(buf *bytes.Buffer) string {
	return strings.TrimRight(buf.String(), "\n")
}

func TestEnabledHonorsLevel(t *testing.T) {
	s, _ := newTestSink(4)
	for _, tc := range []struct {
		level int
		want  bool
	}{
		{0, true}, {1, true}, {4, true}, {5, false}, {8, false},
	} {
		if got := s.Enabled(tc.level); got != tc.want {
			t.Errorf("Enabled(%d) = %v, want %v", tc.level, got, tc.want)
		}
	}
}

// The regression this whole change exists for: client-go's V(8) body dump must
// not reach the sink at the default level. If this fails, the operator starts
// printing request and response bodies again.
func TestBodyDumpIsDroppedAtDefaultLevel(t *testing.T) {
	s, buf := newTestSink(0)
	s.Info(8, "Response Body", "body", "00000000  6b 38 73 00")
	if buf.Len() != 0 {
		t.Fatalf("V(8) reached the sink at level 0: %q", out(buf))
	}
}

func TestErrorPrintsAtDefaultLevel(t *testing.T) {
	s, buf := newTestSink(0)
	s.Error(errors.New("boom"), "Reconciler error")
	got := out(buf)
	if !strings.Contains(got, "ERROR") || !strings.Contains(got, "err=boom") {
		t.Fatalf("Error did not render: %q", got)
	}
}

// controller-runtime attaches controller / object / reconcileID through
// WithValues. Dropping them is what left reconcile errors with no subject.
func TestWithValuesAccumulates(t *testing.T) {
	s, buf := newTestSink(0)
	s.WithValues("controller", "agentinstance").
		WithValues("reconcileID", "abc").
		Info(0, "Reconciling")
	got := out(buf)
	for _, want := range []string{"controller=agentinstance", "reconcileID=abc"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

func TestWithNameComposes(t *testing.T) {
	s, buf := newTestSink(0)
	s.WithName("controller").WithName("agentinstance").Info(0, "hi")
	if got := out(buf); !strings.Contains(got, "controller/agentinstance: hi") {
		t.Fatalf("got %q", got)
	}
}

func TestEmptyValuesRenderNoBrackets(t *testing.T) {
	s, buf := newTestSink(0)
	s.Info(0, "Starting metrics server")
	got := out(buf)
	if strings.Contains(got, "[]") {
		t.Errorf("empty kv rendered brackets: %q", got)
	}
	if !strings.HasSuffix(got, "Starting metrics server") {
		t.Errorf("unexpected trailing content: %q", got)
	}
}

func TestValueWithSpaceIsQuoted(t *testing.T) {
	s, buf := newTestSink(0)
	s.Error(errors.New(`secrets "user-admin-kubeconfig" not found`), "Reconciler error")
	if got := out(buf); !strings.Contains(got, `err="secrets \"user-admin-kubeconfig\" not found"`) {
		t.Fatalf("got %q", got)
	}
}

// rfc3339MillisLinePattern pins the rendered line to the wire format the
// design treats it as: RFC3339 with milliseconds and a trailing Z, a space,
// the level padded to five characters, a space, then the message.
var rfc3339MillisLinePattern = regexp.MustCompile(
	`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z (INFO |ERROR) hi$`)

func TestLineFormatIsPinned(t *testing.T) {
	s, buf := newTestSink(0)
	s.Info(0, "hi")
	got := out(buf)
	if !rfc3339MillisLinePattern.MatchString(got) {
		t.Fatalf("line %q does not match the pinned format %s", got, rfc3339MillisLinePattern)
	}
}

// New floors a negative level at 0 instead of letting it silence V(0) and
// making a misconfigured component look merely quiet.
func TestNewFloorsNegativeLevelAtZero(t *testing.T) {
	l := New(-1)
	if !l.Enabled() {
		t.Error("New(-1).Enabled() = false, want true (V(0) must stay visible)")
	}
	if l.V(1).Enabled() {
		t.Error("New(-1).V(1).Enabled() = true, want false (level must floor at 0, not go negative)")
	}
}

// One record is one line, whatever the fields contain. value() quotes a string
// carrying LF, but a lone CR takes the unquoted path, and the logger name and
// message are written verbatim -- so a field could otherwise split a record and
// let its caller choose the second half.
func TestEveryFieldStaysOnOneLine(t *testing.T) {
	s, buf := newTestSink(0)
	s.WithName("ctor\rFORGED").
		WithValues("key\nFORGED", "value\rFORGED").
		Error(errors.New("boom\nFORGED"), "msg\rFORGED")
	got := out(buf)

	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("a rendered field produced a real CR or LF: %q", got)
	}
	for _, want := range []string{`ctor\rFORGED`, `key\nFORGED`, `value\rFORGED`, `msg\rFORGED`} {
		if !strings.Contains(got, want) {
			t.Errorf("field %s was not escaped into the line: %q", want, got)
		}
	}
}
