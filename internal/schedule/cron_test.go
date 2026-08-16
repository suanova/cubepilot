package schedule

import (
	"testing"
	"time"
)

func TestParseAndNextAfter(t *testing.T) {
	base := time.Date(2026, 8, 14, 10, 23, 30, 0, time.Local)
	cases := []struct {
		expr string
		want time.Time
	}{
		{"*/5 * * * *", time.Date(2026, 8, 14, 10, 25, 0, 0, time.Local)},
		{"0 2 * * *", time.Date(2026, 8, 15, 2, 0, 0, 0, time.Local)},
		{"30 10 * * *", time.Date(2026, 8, 14, 10, 30, 0, 0, time.Local)},
		{"0 9 * * 1", time.Date(2026, 8, 17, 9, 0, 0, 0, time.Local)}, // next Monday
		{"* * * * *", time.Date(2026, 8, 14, 10, 24, 0, 0, time.Local)},
	}
	for _, c := range cases {
		cron, err := Parse(c.expr)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.expr, err)
		}
		if got := cron.NextAfter(base); !got.Equal(c.want) {
			t.Errorf("NextAfter(%q, %v) = %v, want %v", c.expr, base, got, c.want)
		}
	}
}

func TestParseRejectsBad(t *testing.T) {
	for _, expr := range []string{"", "* * *", "61 * * * *", "*/0 * * * *", "a b c d e"} {
		if _, err := Parse(expr); err == nil {
			t.Errorf("Parse(%q) unexpectedly succeeded", expr)
		}
	}
}
