// Package memory provides the in-memory taskstore adapter, for development
// and tests. It keeps mutex-protected maps and never aliases the messages it
// stores or returns (deep copies via proto.Clone).
package memory

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"

	"google.golang.org/protobuf/proto"

	holismv1 "github.com/torrefatto/holism/api/holism/v1"
	"github.com/torrefatto/holism/mind/internal/clock"
	"github.com/torrefatto/holism/mind/internal/taskstore"
)

// Store is an in-memory taskstore.Store.
type Store struct {
	mu       sync.RWMutex
	projects map[string]*holismv1.Project
	tasks    map[string]*holismv1.Task

	clk clock.Clock
	ids clock.IDGen
}

// New returns an in-memory Store. clk stamps created_at/updated_at and ids
// generates entity ids; nil values fall back to the real implementations.
func New(clk clock.Clock, ids clock.IDGen) *Store {
	if clk == nil {
		clk = clock.RealClock{}
	}
	if ids == nil {
		ids = clock.RealIDGen{}
	}
	return &Store{
		projects: make(map[string]*holismv1.Project),
		tasks:    make(map[string]*holismv1.Task),
		clk:      clk,
		ids:      ids,
	}
}

func cloneProject(p *holismv1.Project) *holismv1.Project {
	if p == nil {
		return nil
	}
	return proto.Clone(p).(*holismv1.Project)
}

func cloneTask(t *holismv1.Task) *holismv1.Task {
	if t == nil {
		return nil
	}
	return proto.Clone(t).(*holismv1.Task)
}

// hasAllLabels reports whether labels contains every want label.
func hasAllLabels(labels, want []string) bool {
	for _, w := range want {
		if !slices.Contains(labels, w) {
			return false
		}
	}
	return true
}

// CreateProject implements taskstore.Store.
func (s *Store) CreateProject(ctx context.Context, p *holismv1.Project) (*holismv1.Project, error) {
	if p == nil || p.Name == "" {
		return nil, fmt.Errorf("%w: project name is required", taskstore.ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := cloneProject(p)
	if stored.Id == "" {
		stored.Id = s.ids.NewID("proj")
	}
	if _, ok := s.projects[stored.Id]; ok {
		return nil, fmt.Errorf("%w: project %q", taskstore.ErrAlreadyExists, stored.Id)
	}
	s.projects[stored.Id] = stored
	return cloneProject(stored), nil
}

// GetProject implements taskstore.Store.
func (s *Store) GetProject(ctx context.Context, id string) (*holismv1.Project, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.projects[id]
	if !ok {
		return nil, fmt.Errorf("%w: project %q", taskstore.ErrNotFound, id)
	}
	return cloneProject(p), nil
}

// ListProjects implements taskstore.Store.
func (s *Store) ListProjects(ctx context.Context, filter taskstore.ProjectFilter) ([]*holismv1.Project, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*holismv1.Project
	for _, p := range s.projects {
		if filter.Label != "" && !slices.Contains(p.Labels, filter.Label) {
			continue
		}
		out = append(out, cloneProject(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Id < out[j].Id })
	return out, nil
}

// Create implements taskstore.Store.
func (s *Store) Create(ctx context.Context, t *holismv1.Task) (*holismv1.Task, error) {
	if t == nil || t.ProjectId == "" {
		return nil, fmt.Errorf("%w: task project_id is required", taskstore.ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.projects[t.ProjectId]; !ok {
		return nil, fmt.Errorf("%w: project %q", taskstore.ErrNotFound, t.ProjectId)
	}
	stored := cloneTask(t)
	if stored.Id == "" {
		stored.Id = s.ids.NewID("task")
	}
	if _, ok := s.tasks[stored.Id]; ok {
		return nil, fmt.Errorf("%w: task %q", taskstore.ErrAlreadyExists, stored.Id)
	}
	if stored.Status == holismv1.TaskStatus_TASK_STATUS_UNSPECIFIED {
		stored.Status = holismv1.TaskStatus_TASK_STATUS_QUEUED
	}
	now := s.clk.Now().Unix()
	stored.CreatedAtUnix = now
	stored.UpdatedAtUnix = now
	s.tasks[stored.Id] = stored
	return cloneTask(stored), nil
}

// Get implements taskstore.Store.
func (s *Store) Get(ctx context.Context, id string) (*holismv1.Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tasks[id]
	if !ok {
		return nil, fmt.Errorf("%w: task %q", taskstore.ErrNotFound, id)
	}
	return cloneTask(t), nil
}

// Update implements taskstore.Store.
func (s *Store) Update(ctx context.Context, t *holismv1.Task) (*holismv1.Task, error) {
	if t == nil || t.Id == "" {
		return nil, fmt.Errorf("%w: task id is required", taskstore.ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.tasks[t.Id]
	if !ok {
		return nil, fmt.Errorf("%w: task %q", taskstore.ErrNotFound, t.Id)
	}
	stored := cloneTask(t)
	stored.ProjectId = cur.ProjectId // a task never moves between projects
	stored.CreatedAtUnix = cur.CreatedAtUnix
	stored.UpdatedAtUnix = s.clk.Now().Unix()
	s.tasks[stored.Id] = stored
	return cloneTask(stored), nil
}

// List implements taskstore.Store.
func (s *Store) List(ctx context.Context, filter taskstore.Filter) ([]*holismv1.Task, error) {
	if filter.ProjectID == "" {
		return nil, fmt.Errorf("%w: filter project_id is required", taskstore.ErrInvalid)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*holismv1.Task
	for _, t := range s.tasks {
		if t.ProjectId != filter.ProjectID {
			continue
		}
		if filter.Status != nil && t.Status != *filter.Status {
			continue
		}
		// Labels live on the project; a task matches if its project
		// carries all the filter labels.
		if len(filter.Labels) > 0 {
			p, ok := s.projects[t.ProjectId]
			if !ok || !hasAllLabels(p.Labels, filter.Labels) {
				continue
			}
		}
		out = append(out, cloneTask(t))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAtUnix != out[j].CreatedAtUnix {
			return out[i].CreatedAtUnix < out[j].CreatedAtUnix
		}
		return out[i].Id < out[j].Id
	})
	return out, nil
}
