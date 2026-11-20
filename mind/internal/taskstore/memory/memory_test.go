package memory

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	holismv1 "github.com/torrefatto/holism/api/holism/v1"
	"github.com/torrefatto/holism/mind/internal/taskstore"
)

// fakeClock returns a fixed time, advanced manually by tests.
type fakeClock struct{ t time.Time }

func (f *fakeClock) Now() time.Time { return f.t }

// fakeIDs returns deterministic ids per prefix: "<prefix>-001", "<prefix>-002", …
type fakeIDs struct{ n map[string]int }

func (f *fakeIDs) NewID(prefix string) string {
	if f.n == nil {
		f.n = map[string]int{}
	}
	f.n[prefix]++
	return fmt.Sprintf("%s-%03d", prefix, f.n[prefix])
}

func newTestStore() (*Store, *fakeClock, *fakeIDs) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	ids := &fakeIDs{}
	return New(clk, ids), clk, ids
}

func mustCreateProject(t *testing.T, s *Store, p *holismv1.Project) *holismv1.Project {
	t.Helper()
	out, err := s.CreateProject(context.Background(), p)
	require.NoError(t, err)
	return out
}

func mustCreateTask(t *testing.T, s *Store, task *holismv1.Task) *holismv1.Task {
	t.Helper()
	out, err := s.Create(context.Background(), task)
	require.NoError(t, err)
	return out
}

func TestCreateAndGetProject(t *testing.T) {
	s, _, _ := newTestStore()
	ctx := context.Background()

	p := mustCreateProject(t, s, &holismv1.Project{Name: "web shop", Goal: "sell things", Labels: []string{"gpu"}})
	assert.Equal(t, "proj-001", p.Id, "store should assign an id")

	got, err := s.GetProject(ctx, p.Id)
	require.NoError(t, err)
	assert.Equal(t, "web shop", got.Name)
	assert.Equal(t, "sell things", got.Goal)
	assert.Equal(t, []string{"gpu"}, got.Labels)

	// The store must not alias the caller's message or its own copy.
	p.Name = "mutated"
	again, err := s.GetProject(ctx, got.Id)
	require.NoError(t, err)
	assert.Equal(t, "web shop", again.Name)
	again.Name = "mutated-again"
	third, err := s.GetProject(ctx, got.Id)
	require.NoError(t, err)
	assert.Equal(t, "web shop", third.Name)
}

func TestCreateProjectErrors(t *testing.T) {
	s, _, _ := newTestStore()
	ctx := context.Background()

	_, err := s.CreateProject(ctx, &holismv1.Project{})
	assert.ErrorIs(t, err, taskstore.ErrInvalid, "empty name")
	_, err = s.CreateProject(ctx, nil)
	assert.ErrorIs(t, err, taskstore.ErrInvalid, "nil project")
	mustCreateProject(t, s, &holismv1.Project{Id: "p1", Name: "one"})
	_, err = s.CreateProject(ctx, &holismv1.Project{Id: "p1", Name: "dup"})
	assert.ErrorIs(t, err, taskstore.ErrAlreadyExists, "duplicate id")
	_, err = s.GetProject(ctx, "nope")
	assert.ErrorIs(t, err, taskstore.ErrNotFound, "unknown id")
}

func TestListProjectsFilterByLabel(t *testing.T) {
	s, _, _ := newTestStore()
	ctx := context.Background()
	mustCreateProject(t, s, &holismv1.Project{Id: "b", Name: "b", Labels: []string{"gpu"}})
	mustCreateProject(t, s, &holismv1.Project{Id: "a", Name: "a", Labels: []string{"gpu", "region:eu"}})
	mustCreateProject(t, s, &holismv1.Project{Id: "c", Name: "c"})

	all, err := s.ListProjects(ctx, taskstore.ProjectFilter{})
	require.NoError(t, err)
	require.Len(t, all, 3)
	assert.Equal(t, []string{"a", "b", "c"}, []string{all[0].Id, all[1].Id, all[2].Id}, "sorted by id")

	gpu, err := s.ListProjects(ctx, taskstore.ProjectFilter{Label: "gpu"})
	require.NoError(t, err)
	require.Len(t, gpu, 2)
	assert.Equal(t, "a", gpu[0].Id)
	assert.Equal(t, "b", gpu[1].Id)
}

func TestCreateTaskRequiresExistingProject(t *testing.T) {
	s, _, _ := newTestStore()
	ctx := context.Background()

	_, err := s.Create(ctx, &holismv1.Task{Spec: &holismv1.TaskSpec{Title: "x"}})
	assert.ErrorIs(t, err, taskstore.ErrInvalid, "empty project_id")
	_, err = s.Create(ctx, &holismv1.Task{ProjectId: "ghost", Spec: &holismv1.TaskSpec{Title: "x"}})
	assert.ErrorIs(t, err, taskstore.ErrNotFound, "unknown project")
}

func TestCreateTaskDefaultsAndIsolation(t *testing.T) {
	s, clk, _ := newTestStore()
	ctx := context.Background()
	p := mustCreateProject(t, s, &holismv1.Project{Name: "p"})

	tk := mustCreateTask(t, s, &holismv1.Task{
		ProjectId: p.Id,
		Spec:      &holismv1.TaskSpec{Title: "do something"},
	})
	assert.Equal(t, "task-001", tk.Id, "store should assign an id")
	assert.Equal(t, holismv1.TaskStatus_TASK_STATUS_QUEUED, tk.Status, "UNSPECIFIED becomes QUEUED")
	want := clk.Now().Unix()
	assert.Equal(t, want, tk.CreatedAtUnix)
	assert.Equal(t, want, tk.UpdatedAtUnix)

	tk.Spec.Title = "mutated"
	got, err := s.Get(ctx, tk.Id)
	require.NoError(t, err)
	assert.Equal(t, "do something", got.Spec.Title, "stored task must not be mutated through a returned message")

	_, err = s.Create(ctx, &holismv1.Task{Id: tk.Id, ProjectId: p.Id})
	assert.ErrorIs(t, err, taskstore.ErrAlreadyExists, "duplicate id")
	_, err = s.Get(ctx, "ghost")
	assert.ErrorIs(t, err, taskstore.ErrNotFound, "unknown id")
}

func TestUpdateTask(t *testing.T) {
	s, clk, _ := newTestStore()
	ctx := context.Background()
	p := mustCreateProject(t, s, &holismv1.Project{Name: "p"})
	tk := mustCreateTask(t, s, &holismv1.Task{ProjectId: p.Id, Spec: &holismv1.TaskSpec{Title: "t"}})
	created := tk.CreatedAtUnix

	clk.t = clk.t.Add(time.Hour)
	updated, err := s.Update(ctx, &holismv1.Task{
		Id:        tk.Id,
		ProjectId: "some-other-project", // must be ignored: project_id is immutable
		Status:    holismv1.TaskStatus_TASK_STATUS_RUNNING,
		Spec:      &holismv1.TaskSpec{Title: "t"},
	})
	require.NoError(t, err)
	assert.Equal(t, holismv1.TaskStatus_TASK_STATUS_RUNNING, updated.Status)
	assert.Equal(t, p.Id, updated.ProjectId, "project_id must be immutable")
	assert.Equal(t, created, updated.CreatedAtUnix, "created_at must be preserved")
	assert.Equal(t, created+3600, updated.UpdatedAtUnix, "updated_at must be re-stamped")

	_, err = s.Update(ctx, &holismv1.Task{Id: "ghost", ProjectId: p.Id})
	assert.ErrorIs(t, err, taskstore.ErrNotFound, "unknown id")
	_, err = s.Update(ctx, &holismv1.Task{ProjectId: p.Id})
	assert.ErrorIs(t, err, taskstore.ErrInvalid, "empty id")
}

func TestListTasksFilter(t *testing.T) {
	s, clk, _ := newTestStore()
	ctx := context.Background()
	gpu := mustCreateProject(t, s, &holismv1.Project{Name: "gpu", Labels: []string{"gpu", "region:eu"}})
	cpu := mustCreateProject(t, s, &holismv1.Project{Name: "cpu"})

	t1 := mustCreateTask(t, s, &holismv1.Task{ProjectId: gpu.Id, Spec: &holismv1.TaskSpec{Title: "t1"}})
	clk.t = clk.t.Add(time.Second)
	t2 := mustCreateTask(t, s, &holismv1.Task{ProjectId: gpu.Id, Spec: &holismv1.TaskSpec{Title: "t2"}})
	mustCreateTask(t, s, &holismv1.Task{ProjectId: cpu.Id, Spec: &holismv1.TaskSpec{Title: "other"}})
	_, err := s.Update(ctx, &holismv1.Task{Id: t2.Id, ProjectId: gpu.Id, Status: holismv1.TaskStatus_TASK_STATUS_RUNNING})
	require.NoError(t, err)

	// project_id is required.
	_, err = s.List(ctx, taskstore.Filter{})
	assert.ErrorIs(t, err, taskstore.ErrInvalid, "empty project_id")

	// All tasks of the project, sorted by (created_at, id).
	all, err := s.List(ctx, taskstore.Filter{ProjectID: gpu.Id})
	require.NoError(t, err)
	require.Len(t, all, 2)
	assert.Equal(t, t1.Id, all[0].Id)
	assert.Equal(t, t2.Id, all[1].Id)

	// Status filter.
	running := holismv1.TaskStatus_TASK_STATUS_RUNNING
	byStatus, err := s.List(ctx, taskstore.Filter{ProjectID: gpu.Id, Status: &running})
	require.NoError(t, err)
	require.Len(t, byStatus, 1)
	assert.Equal(t, t2.Id, byStatus[0].Id)

	// Label filter: labels match the owning project's labels (all must match).
	byLabel, err := s.List(ctx, taskstore.Filter{ProjectID: gpu.Id, Labels: []string{"gpu", "region:eu"}})
	require.NoError(t, err)
	assert.Len(t, byLabel, 2)
	noMatch, err := s.List(ctx, taskstore.Filter{ProjectID: gpu.Id, Labels: []string{"gpu", "missing"}})
	require.NoError(t, err)
	assert.Empty(t, noMatch)
	// Same filter on the unlabeled project matches nothing.
	cpuTasks, err := s.List(ctx, taskstore.Filter{ProjectID: cpu.Id, Labels: []string{"gpu"}})
	require.NoError(t, err)
	assert.Empty(t, cpuTasks)
}

func TestConcurrentUse(t *testing.T) {
	s, _, _ := newTestStore()
	ctx := context.Background()
	p := mustCreateProject(t, s, &holismv1.Project{Name: "p"})
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				tk, err := s.Create(ctx, &holismv1.Task{ProjectId: p.Id, Spec: &holismv1.TaskSpec{Title: fmt.Sprintf("t%d-%d", i, j)}})
				if !assert.NoError(t, err) {
					return
				}
				if _, err := s.Update(ctx, &holismv1.Task{Id: tk.Id, ProjectId: p.Id, Status: holismv1.TaskStatus_TASK_STATUS_RUNNING}); !assert.NoError(t, err) {
					return
				}
				if _, err := s.List(ctx, taskstore.Filter{ProjectID: p.Id}); !assert.NoError(t, err) {
					return
				}
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	tasks, err := s.List(ctx, taskstore.Filter{ProjectID: p.Id})
	require.NoError(t, err)
	assert.Len(t, tasks, 8*50)
}
