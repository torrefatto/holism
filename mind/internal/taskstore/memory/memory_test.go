package memory

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

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
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	return out
}

func mustCreateTask(t *testing.T, s *Store, task *holismv1.Task) *holismv1.Task {
	t.Helper()
	out, err := s.Create(context.Background(), task)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return out
}

func TestCreateAndGetProject(t *testing.T) {
	s, _, _ := newTestStore()
	ctx := context.Background()

	p := mustCreateProject(t, s, &holismv1.Project{Name: "web shop", Goal: "sell things", Labels: []string{"gpu"}})
	if p.Id != "proj-001" {
		t.Fatalf("assigned id = %q, want proj-001", p.Id)
	}

	got, err := s.GetProject(ctx, p.Id)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.Name != "web shop" || got.Goal != "sell things" || len(got.Labels) != 1 {
		t.Fatalf("GetProject = %+v", got)
	}

	// The store must not alias the caller's message or its own copy.
	p.Name = "mutated"
	again, _ := s.GetProject(ctx, got.Id)
	if again.Name != "web shop" {
		t.Fatalf("stored project was mutated through the returned message: %+v", again)
	}
	again.Name = "mutated-again"
	third, _ := s.GetProject(ctx, got.Id)
	if third.Name != "web shop" {
		t.Fatalf("stored project was mutated through a returned message: %+v", third)
	}
}

func TestCreateProjectErrors(t *testing.T) {
	s, _, _ := newTestStore()
	ctx := context.Background()

	if _, err := s.CreateProject(ctx, &holismv1.Project{}); !errors.Is(err, taskstore.ErrInvalid) {
		t.Fatalf("empty name: err = %v, want ErrInvalid", err)
	}
	if _, err := s.CreateProject(ctx, nil); !errors.Is(err, taskstore.ErrInvalid) {
		t.Fatalf("nil project: err = %v, want ErrInvalid", err)
	}
	mustCreateProject(t, s, &holismv1.Project{Id: "p1", Name: "one"})
	if _, err := s.CreateProject(ctx, &holismv1.Project{Id: "p1", Name: "dup"}); !errors.Is(err, taskstore.ErrAlreadyExists) {
		t.Fatalf("duplicate id: err = %v, want ErrAlreadyExists", err)
	}
	if _, err := s.GetProject(ctx, "nope"); !errors.Is(err, taskstore.ErrNotFound) {
		t.Fatalf("unknown id: err = %v, want ErrNotFound", err)
	}
}

func TestListProjectsFilterByLabel(t *testing.T) {
	s, _, _ := newTestStore()
	ctx := context.Background()
	mustCreateProject(t, s, &holismv1.Project{Id: "b", Name: "b", Labels: []string{"gpu"}})
	mustCreateProject(t, s, &holismv1.Project{Id: "a", Name: "a", Labels: []string{"gpu", "region:eu"}})
	mustCreateProject(t, s, &holismv1.Project{Id: "c", Name: "c"})

	all, err := s.ListProjects(ctx, taskstore.ProjectFilter{})
	if err != nil || len(all) != 3 {
		t.Fatalf("ListProjects all = %d, %v", len(all), err)
	}
	if all[0].Id != "a" || all[1].Id != "b" || all[2].Id != "c" {
		t.Fatalf("ListProjects not sorted by id: %v %v %v", all[0].Id, all[1].Id, all[2].Id)
	}
	gpu, err := s.ListProjects(ctx, taskstore.ProjectFilter{Label: "gpu"})
	if err != nil || len(gpu) != 2 || gpu[0].Id != "a" || gpu[1].Id != "b" {
		t.Fatalf("ListProjects label=gpu = %+v, %v", gpu, err)
	}
}

func TestCreateTaskRequiresExistingProject(t *testing.T) {
	s, _, _ := newTestStore()
	ctx := context.Background()

	if _, err := s.Create(ctx, &holismv1.Task{Spec: &holismv1.TaskSpec{Title: "x"}}); !errors.Is(err, taskstore.ErrInvalid) {
		t.Fatalf("empty project_id: err = %v, want ErrInvalid", err)
	}
	if _, err := s.Create(ctx, &holismv1.Task{ProjectId: "ghost", Spec: &holismv1.TaskSpec{Title: "x"}}); !errors.Is(err, taskstore.ErrNotFound) {
		t.Fatalf("unknown project: err = %v, want ErrNotFound", err)
	}
}

func TestCreateTaskDefaultsAndIsolation(t *testing.T) {
	s, clk, _ := newTestStore()
	ctx := context.Background()
	p := mustCreateProject(t, s, &holismv1.Project{Name: "p"})

	tk := mustCreateTask(t, s, &holismv1.Task{
		ProjectId: p.Id,
		Spec:      &holismv1.TaskSpec{Title: "do something"},
	})
	if tk.Id != "task-001" {
		t.Fatalf("assigned id = %q, want task-001", tk.Id)
	}
	if tk.Status != holismv1.TaskStatus_TASK_STATUS_QUEUED {
		t.Fatalf("status = %v, want QUEUED", tk.Status)
	}
	want := clk.Now().Unix()
	if tk.CreatedAtUnix != want || tk.UpdatedAtUnix != want {
		t.Fatalf("timestamps = %d/%d, want %d", tk.CreatedAtUnix, tk.UpdatedAtUnix, want)
	}

	tk.Spec.Title = "mutated"
	got, err := s.Get(ctx, tk.Id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Spec.Title != "do something" {
		t.Fatalf("stored task mutated through returned message: %+v", got.Spec)
	}

	if _, err := s.Create(ctx, &holismv1.Task{Id: tk.Id, ProjectId: p.Id}); !errors.Is(err, taskstore.ErrAlreadyExists) {
		t.Fatalf("duplicate id: err = %v, want ErrAlreadyExists", err)
	}
	if _, err := s.Get(ctx, "ghost"); !errors.Is(err, taskstore.ErrNotFound) {
		t.Fatalf("unknown id: err = %v, want ErrNotFound", err)
	}
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
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Status != holismv1.TaskStatus_TASK_STATUS_RUNNING {
		t.Fatalf("status = %v, want RUNNING", updated.Status)
	}
	if updated.ProjectId != p.Id {
		t.Fatalf("project_id changed to %q, want immutable %q", updated.ProjectId, p.Id)
	}
	if updated.CreatedAtUnix != created {
		t.Fatalf("created_at = %d, want preserved %d", updated.CreatedAtUnix, created)
	}
	if updated.UpdatedAtUnix != created+3600 {
		t.Fatalf("updated_at = %d, want %d", updated.UpdatedAtUnix, created+3600)
	}

	if _, err := s.Update(ctx, &holismv1.Task{Id: "ghost", ProjectId: p.Id}); !errors.Is(err, taskstore.ErrNotFound) {
		t.Fatalf("unknown id: err = %v, want ErrNotFound", err)
	}
	if _, err := s.Update(ctx, &holismv1.Task{ProjectId: p.Id}); !errors.Is(err, taskstore.ErrInvalid) {
		t.Fatalf("empty id: err = %v, want ErrInvalid", err)
	}
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
	if _, err := s.Update(ctx, &holismv1.Task{Id: t2.Id, ProjectId: gpu.Id, Status: holismv1.TaskStatus_TASK_STATUS_RUNNING}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// project_id is required.
	if _, err := s.List(ctx, taskstore.Filter{}); !errors.Is(err, taskstore.ErrInvalid) {
		t.Fatalf("empty project_id: err = %v, want ErrInvalid", err)
	}

	// All tasks of the project, sorted by (created_at, id).
	all, err := s.List(ctx, taskstore.Filter{ProjectID: gpu.Id})
	if err != nil || len(all) != 2 || all[0].Id != t1.Id || all[1].Id != t2.Id {
		t.Fatalf("List all = %+v, %v", all, err)
	}

	// Status filter.
	running := holismv1.TaskStatus_TASK_STATUS_RUNNING
	byStatus, err := s.List(ctx, taskstore.Filter{ProjectID: gpu.Id, Status: &running})
	if err != nil || len(byStatus) != 1 || byStatus[0].Id != t2.Id {
		t.Fatalf("List by status = %+v, %v", byStatus, err)
	}

	// Label filter: labels match the owning project's labels (all must match).
	byLabel, err := s.List(ctx, taskstore.Filter{ProjectID: gpu.Id, Labels: []string{"gpu", "region:eu"}})
	if err != nil || len(byLabel) != 2 {
		t.Fatalf("List by labels [gpu region:eu] = %+v, %v", byLabel, err)
	}
	noMatch, err := s.List(ctx, taskstore.Filter{ProjectID: gpu.Id, Labels: []string{"gpu", "missing"}})
	if err != nil || len(noMatch) != 0 {
		t.Fatalf("List by labels [gpu missing] = %+v, %v", noMatch, err)
	}
	// Same filter on the unlabeled project matches nothing.
	cpuTasks, err := s.List(ctx, taskstore.Filter{ProjectID: cpu.Id, Labels: []string{"gpu"}})
	if err != nil || len(cpuTasks) != 0 {
		t.Fatalf("List cpu with label gpu = %+v, %v", cpuTasks, err)
	}
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
				if err != nil {
					t.Errorf("Create: %v", err)
					return
				}
				if _, err := s.Update(ctx, &holismv1.Task{Id: tk.Id, ProjectId: p.Id, Status: holismv1.TaskStatus_TASK_STATUS_RUNNING}); err != nil {
					t.Errorf("Update: %v", err)
					return
				}
				if _, err := s.List(ctx, taskstore.Filter{ProjectID: p.Id}); err != nil {
					t.Errorf("List: %v", err)
					return
				}
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	tasks, err := s.List(ctx, taskstore.Filter{ProjectID: p.Id})
	if err != nil || len(tasks) != 8*50 {
		t.Fatalf("List after concurrent writes = %d, %v", len(tasks), err)
	}
}
