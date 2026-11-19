package clock

import (
	"strings"
	"testing"
	"time"
)

func TestRealClock(t *testing.T) {
	before := time.Now()
	got := RealClock{}.Now()
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Fatalf("Now() = %v, want within [%v, %v]", got, before, after)
	}
}

func TestRealIDGen(t *testing.T) {
	gen := RealIDGen{}
	a, b := gen.NewID("task"), gen.NewID("task")
	if !strings.HasPrefix(a, "task-") || len(strings.TrimPrefix(a, "task-")) != 16 {
		t.Fatalf("NewID(%q) = %q, want prefix + 16 hex chars", "task", a)
	}
	if a == b {
		t.Fatalf("NewID returned the same id twice: %q", a)
	}
	if got := gen.NewID(""); strings.Contains(got, "-") || len(got) != 16 {
		t.Fatalf("NewID(\"\") = %q, want bare 16 hex chars", got)
	}
}
