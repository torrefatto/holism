// Package taskstore defines the Store port: the mind's durable view of
// projects and tasks. Adapters (in-memory, PostgreSQL, Linear, …) live in
// subpackages and import this package — never the other way around.
//
// The mind never assumes it owns the storage: with an external adapter
// (e.g. Linear) the durable state lives outside the mind process.
package taskstore

import (
	"context"
	"errors"

	holismv1 "github.com/torrefatto/holism/api/holism/v1"
)

// Sentinel errors returned by Store implementations. Callers should match
// them with errors.Is.
var (
	// ErrNotFound is returned when the requested entity does not exist
	// (also used for the referential check: a task whose project_id does
	// not exist).
	ErrNotFound = errors.New("taskstore: not found")
	// ErrAlreadyExists is returned when creating an entity whose id is
	// already taken.
	ErrAlreadyExists = errors.New("taskstore: already exists")
	// ErrInvalid is returned for contract violations the store enforces
	// itself (e.g. a task without project_id, a filter without project_id).
	ErrInvalid = errors.New("taskstore: invalid")
)

// Filter selects tasks in Store.List.
//
// ProjectID is required: tasks are always listed within a project, mirroring
// the contract (ListTasksRequest.project_id is validated min_len: 1).
// Status nil means any status. Labels match against the owning project's
// labels — a task matches only if its project carries ALL of them (tasks
// inherit their project's labels; Task itself has no label field).
type Filter struct {
	ProjectID string
	Status    *holismv1.TaskStatus
	Labels    []string
}

// ProjectFilter selects projects in Store.ListProjects. Label empty means
// any project; otherwise the project must carry that label.
type ProjectFilter struct {
	Label string
}

// Store is the TaskStore port. Implementations must be safe for concurrent
// use and must not alias the messages they store or return.
type Store interface {
	// CreateProject stores a new project. An empty id is assigned by the
	// store. Returns ErrAlreadyExists on id conflict, ErrInvalid on an
	// empty name.
	CreateProject(ctx context.Context, p *holismv1.Project) (*holismv1.Project, error)
	// GetProject returns the project with the given id, or ErrNotFound.
	GetProject(ctx context.Context, id string) (*holismv1.Project, error)
	// ListProjects returns all projects matching the filter, sorted by id.
	ListProjects(ctx context.Context, filter ProjectFilter) ([]*holismv1.Project, error)

	// Create stores a new task. The task must carry a project_id that
	// exists (referential check; the contract already enforces a non-empty
	// project_id at the API boundary). An empty id is assigned, an
	// UNSPECIFIED status becomes QUEUED, and created_at/updated_at are
	// stamped by the store.
	Create(ctx context.Context, t *holismv1.Task) (*holismv1.Task, error)
	// Get returns the task with the given id, or ErrNotFound.
	Get(ctx context.Context, id string) (*holismv1.Task, error)
	// Update replaces the stored task with the same id. created_at and
	// project_id are immutable and preserved from the stored task;
	// updated_at is re-stamped. Returns ErrNotFound if the id is unknown.
	Update(ctx context.Context, t *holismv1.Task) (*holismv1.Task, error)
	// List returns the tasks matching the filter, sorted by
	// (created_at, id). Filter.ProjectID is required.
	List(ctx context.Context, filter Filter) ([]*holismv1.Task, error)
}
