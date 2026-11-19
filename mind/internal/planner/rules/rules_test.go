package rules

import (
	"context"
	"errors"
	"strings"
	"testing"

	holismv1 "github.com/torrefatto/holism/api/holism/v1"
	"github.com/torrefatto/holism/mind/internal/planner"
)

func TestPlanSingleTaskFromGoal(t *testing.T) {
	p := &holismv1.Project{Id: "p1", Name: "web shop", Goal: "sell things"}
	specs, err := New().Plan(context.Background(), p, "Write the checkout flow\nimplement payment, add tests")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("got %d specs, want 1", len(specs))
	}
	if specs[0].Title != "Write the checkout flow" {
		t.Fatalf("title = %q", specs[0].Title)
	}
	if specs[0].Description != "Write the checkout flow\nimplement payment, add tests" {
		t.Fatalf("description = %q", specs[0].Description)
	}
}

func TestPlanTruncatesLongTitle(t *testing.T) {
	p := &holismv1.Project{Id: "p1", Name: "p"}
	long := strings.Repeat("a", 200) + "\nbody"
	specs, err := New().Plan(context.Background(), p, long)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if r := []rune(specs[0].Title); len(r) != maxTitleLen {
		t.Fatalf("title length = %d runes, want %d (%q)", len(r), maxTitleLen, specs[0].Title)
	}
	if !strings.HasSuffix(specs[0].Title, "…") {
		t.Fatalf("truncated title should end with …: %q", specs[0].Title)
	}
}

func TestPlanErrors(t *testing.T) {
	p := &holismv1.Project{Id: "p1", Name: "p"}
	if _, err := New().Plan(context.Background(), nil, "goal"); !errors.Is(err, planner.ErrNilProject) {
		t.Fatalf("nil project: err = %v, want ErrNilProject", err)
	}
	if _, err := New().Plan(context.Background(), p, ""); !errors.Is(err, planner.ErrEmptyGoal) {
		t.Fatalf("empty goal: err = %v, want ErrEmptyGoal", err)
	}
	if _, err := New().Plan(context.Background(), p, "  \n\t "); !errors.Is(err, planner.ErrEmptyGoal) {
		t.Fatalf("whitespace goal: err = %v, want ErrEmptyGoal", err)
	}
}
