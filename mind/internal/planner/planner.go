// Package planner defines the Planner port: the mind-side component that
// decomposes a project's goal into an ordered set of task specs. Adapters
// (rule-based, LLM-based) live in subpackages and import this package —
// never the other way around. Knowledge harnesses are wired into planner
// adapters at the composition root, not into Holism itself.
package planner

import (
	"context"
	"errors"

	holismv1 "github.com/torrefatto/holism/api/holism/v1"
)

// Sentinel errors returned by Planner implementations.
var (
	// ErrNilProject is returned when Plan is called with a nil project.
	ErrNilProject = errors.New("planner: nil project")
	// ErrEmptyGoal is returned when the goal is empty or whitespace-only.
	ErrEmptyGoal = errors.New("planner: empty goal")
)

// Planner decomposes a goal within a project into task specs. The returned
// specs are ordered; each may carry required_capabilities naming the
// capabilities a drone must declare to receive the task.
type Planner interface {
	Plan(ctx context.Context, project *holismv1.Project, goal string) ([]*holismv1.TaskSpec, error)
}
