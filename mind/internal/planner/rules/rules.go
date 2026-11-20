// Package rules provides the rule-based Planner stub: it decomposes a goal
// into a single task carrying the goal text. An LLM-based adapter can
// replace it without touching the mind core.
package rules

import (
	"context"
	"strings"

	holismv1 "github.com/torrefatto/holism/api/holism/v1"
	"github.com/torrefatto/holism/mind/internal/planner"
)

// maxTitleLen caps the task title derived from the goal's first line.
const maxTitleLen = 80

// Planner is the rule-based planner stub.
type Planner struct{}

// New returns the rule-based Planner.
func New() *Planner { return &Planner{} }

// Plan implements planner.Planner. The stub returns exactly one TaskSpec:
// its title is the first line of the goal (truncated to maxTitleLen runes),
// its description is the full goal text.
func (p *Planner) Plan(ctx context.Context, project *holismv1.Project, goal string) ([]*holismv1.TaskSpec, error) {
	if project == nil {
		return nil, planner.ErrNilProject
	}
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return nil, planner.ErrEmptyGoal
	}
	return []*holismv1.TaskSpec{{
		Title:       titleFrom(goal),
		Description: goal,
	}}, nil
}

// titleFrom extracts the first line of s, truncated to maxTitleLen runes.
func titleFrom(s string) string {
	line := s
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		line = s[:i]
	}
	line = strings.TrimSpace(line)
	r := []rune(line)
	if len(r) > maxTitleLen {
		line = strings.TrimSpace(string(r[:maxTitleLen-1])) + "…"
	}
	return line
}
