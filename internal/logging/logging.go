// Package logging provides the platform's logr sink: one line per record on
// stderr, gated by a verbosity level.
//
// controller-runtime and client-go reach this sink through logr, so the level
// passed to New is what decides how much of their verbosity reaches the pod
// log. Left ungated, the effective verbosity is the highest level anything in
// the dependency tree asks for, which is V(10) -- and V(8) alone prints whole
// request and response bodies.
package logging

import (
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
)

// rfc3339Millis is the timestamp format. Milliseconds are enough to order
// records within a second without the noise of full nanosecond precision.
const rfc3339Millis = "2006-01-02T15:04:05.000Z"

// sink is a logr.LogSink rendering one line per record.
//
// It owns its *log.Logger instead of using the global one: the components log
// their own messages through log.Printf, and that global instance's flags must
// keep producing the timestamps those lines have always carried.
type sink struct {
	out    *log.Logger
	name   string
	values []any
	max    int
}

var _ logr.LogSink = (*sink)(nil)

// New returns a logr.Logger writing to stderr. Level 0 emits V(0) and every
// Error; each increment admits one more V level from controller-runtime and
// client-go.
//
// Level 8 is client-go's request/response body dump (rest/request.go logBody),
// which hex-dumps the body whole -- Secret bodies included. Raise the level
// past 7 only to watch actual API traffic, and expect that.
func New(level int) logr.Logger {
	return logr.New(newSink(os.Stderr, level))
}

func newSink(w io.Writer, level int) *sink {
	return &sink{out: log.New(w, "", 0), max: level}
}

func (l *sink) Init(logr.RuntimeInfo) {}

// Enabled is the level gate, and it is what makes the configured level
// authoritative for dependency verbosity -- but not because logr's own
// Logger.V consults it. V only accumulates the requested level; it never
// calls Enabled and never returns a null logger. The gate bites at call
// sites that ask Enabled before doing expensive work, the way client-go's
// rest/request.go logBody does:
//
//	if loggerV := logger.V(8); loggerV.Enabled() {
//		loggerV.Info(prefix, "body", hex.Dump(body))
//	}
//
// The Enabled() check precedes hex.Dump, so a false answer means the dump is
// never built. A call site that instead passes an already-computed value to
// Logger.Info is evaluated regardless of level -- Info runs after Go has
// already built the argument -- and must guard itself the same way.
func (l *sink) Enabled(level int) bool { return level <= l.max }

func (l *sink) Info(level int, msg string, kv ...any) {
	if !l.Enabled(level) {
		return
	}
	l.out.Print(l.line("INFO", msg, kv))
}

// Error is deliberately not gated: it prints at every level.
func (l *sink) Error(err error, msg string, kv ...any) {
	l.out.Print(l.line("ERROR", msg, append([]any{"err", err}, kv...)))
}

// WithValues accumulates. controller-runtime builds its per-reconcile logger
// this way (controller / object / reconcileID), so returning the receiver
// would leave every reconcile error without a subject.
func (l *sink) WithValues(kv ...any) logr.LogSink {
	if len(kv) == 0 {
		return l
	}
	values := make([]any, 0, len(l.values)+len(kv))
	values = append(values, l.values...)
	values = append(values, kv...)
	return &sink{out: l.out, name: l.name, values: values, max: l.max}
}

func (l *sink) WithName(name string) logr.LogSink {
	if name == "" {
		return l
	}
	if l.name != "" {
		name = l.name + "/" + name
	}
	return &sink{out: l.out, name: name, values: l.values, max: l.max}
}

// line renders "<RFC3339 millis> <LEVEL> <logger>: <msg> <k=v ...>", with the
// name carried by WithValues first so a line still names its subject.
func (l *sink) line(level, msg string, kv []any) string {
	var b strings.Builder
	b.WriteString(time.Now().UTC().Format(rfc3339Millis))
	b.WriteString(" ")
	b.WriteString(level)
	for i := len(level); i < 5; i++ {
		b.WriteString(" ")
	}
	b.WriteString(" ")
	if l.name != "" {
		b.WriteString(l.name)
		b.WriteString(": ")
	}
	b.WriteString(msg)
	for _, group := range [][]any{l.values, kv} {
		for i := 0; i+1 < len(group); i += 2 {
			b.WriteString(" ")
			b.WriteString(key(group[i]))
			b.WriteString("=")
			b.WriteString(value(group[i+1]))
		}
	}
	return b.String()
}

func key(k any) string {
	if s, ok := k.(string); ok {
		return s
	}
	return fmt.Sprint(k)
}

// value renders a value so a copy-paste of the line is unambiguous: anything
// containing whitespace is quoted, and a value implementing fmt.Stringer (an
// error, a namespaced name) is asked for its own rendering.
func value(v any) string {
	if v == nil {
		return "null"
	}
	if err, ok := v.(error); ok && err == nil {
		return "null"
	}
	s := fmt.Sprint(v)
	if s == "" || strings.ContainsAny(s, " \t\n\"") {
		return strconv.Quote(s)
	}
	return s
}
