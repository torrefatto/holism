// Package server implements the mind's gRPC server: MindService over the
// generated api/holism/v1 contract, backed by the mind's ports (TaskStore,
// EventBus, Planner, drone registry). It depends only on ports, never on
// adapters; the composition root (cmd/mind) wires concrete implementations.
//
// The server itself is transport-agnostic; mTLS is configured at the listener
// (see tls.go) with tls.RequireAndVerifyClientCert.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	holismv1 "github.com/torrefatto/holism/api/holism/v1"
	"github.com/torrefatto/holism/mind/internal/eventbus"
	"github.com/torrefatto/holism/mind/internal/planner"
	"github.com/torrefatto/holism/mind/internal/registry"
	"github.com/torrefatto/holism/mind/internal/taskstore"
)

// Deps are the ports the Server is built on.
type Deps struct {
	Store    taskstore.Store
	Bus      eventbus.Bus
	Planner  planner.Planner
	Registry registry.Registry
}

// Server implements holismv1.MindServiceServer.
type Server struct {
	holismv1.UnimplementedMindServiceServer

	store    taskstore.Store
	bus      eventbus.Bus
	planner  planner.Planner
	registry registry.Registry
}

// New returns a Server over the given ports.
func New(deps Deps) *Server {
	return &Server{
		store:    deps.Store,
		bus:      deps.Bus,
		planner:  deps.Planner,
		registry: deps.Registry,
	}
}

// validator is implemented by the generated pgv messages.
type validator interface {
	Validate() error
}

// validateReq runs the generated buf.validate rules on a request and maps a
// violation to InvalidArgument.
func validateReq(msg validator) error {
	if err := msg.Validate(); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return nil
}

// statusFromError maps the ports' sentinel errors to gRPC status codes.
// Unknown errors are returned unchanged (gRPC maps them to codes.Unknown).
func statusFromError(err error) error {
	switch {
	case errors.Is(err, taskstore.ErrNotFound), errors.Is(err, registry.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, taskstore.ErrAlreadyExists):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, taskstore.ErrInvalid),
		errors.Is(err, registry.ErrInvalid),
		errors.Is(err, planner.ErrNilProject),
		errors.Is(err, planner.ErrEmptyGoal),
		errors.Is(err, eventbus.ErrInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	default:
		return err
	}
}

// publish delivers a lifecycle event on the EventBus. Event delivery is
// observability: a publish failure is logged, never fails the RPC.
func (s *Server) publish(ctx context.Context, ev *holismv1.Event) {
	if err := s.bus.Publish(ctx, ev); err != nil {
		slog.Warn("server: failed to publish event",
			"type", ev.GetType().String(), "error", err)
	}
}

// CreateProject implements holismv1.MindServiceServer.
func (s *Server) CreateProject(ctx context.Context, req *holismv1.CreateProjectRequest) (*holismv1.Project, error) {
	if err := validateReq(req); err != nil {
		return nil, err
	}
	p, err := s.store.CreateProject(ctx, &holismv1.Project{
		Name:        req.GetName(),
		Goal:        req.GetGoal(),
		Description: req.GetDescription(),
		Labels:      req.GetLabels(),
	})
	if err != nil {
		return nil, statusFromError(err)
	}
	s.publish(ctx, &holismv1.Event{
		Type:      holismv1.Event_TYPE_PROJECT_CREATED,
		ProjectId: p.GetId(),
		Summary:   fmt.Sprintf("project %s (%q) created", p.GetId(), p.GetName()),
	})
	return p, nil
}

// GetProject implements holismv1.MindServiceServer.
func (s *Server) GetProject(ctx context.Context, req *holismv1.GetProjectRequest) (*holismv1.Project, error) {
	if err := validateReq(req); err != nil {
		return nil, err
	}
	p, err := s.store.GetProject(ctx, req.GetId())
	if err != nil {
		return nil, statusFromError(err)
	}
	return p, nil
}

// ListProjects implements holismv1.MindServiceServer.
func (s *Server) ListProjects(ctx context.Context, req *holismv1.ListProjectsRequest) (*holismv1.ListProjectsResponse, error) {
	if err := validateReq(req); err != nil {
		return nil, err
	}
	projects, err := s.store.ListProjects(ctx, taskstore.ProjectFilter{Label: req.GetLabel()})
	if err != nil {
		return nil, statusFromError(err)
	}
	return &holismv1.ListProjectsResponse{Projects: projects}, nil
}

// SubmitTask implements holismv1.MindServiceServer. project_id is validated
// by the generated buf.validate rules and checked against the TaskStore. A
// submission carrying a goal is decomposed by the Planner; a submission
// carrying a spec creates a single task. Every created task is queued and
// publishes TYPE_TASK_QUEUED; the first created task is returned.
func (s *Server) SubmitTask(ctx context.Context, req *holismv1.SubmitTaskRequest) (*holismv1.Task, error) {
	if err := validateReq(req); err != nil {
		return nil, err
	}
	project, err := s.store.GetProject(ctx, req.GetProjectId())
	if err != nil {
		return nil, statusFromError(err)
	}
	var specs []*holismv1.TaskSpec
	switch p := req.GetPayload().(type) {
	case *holismv1.SubmitTaskRequest_Spec:
		if p.Spec == nil {
			return nil, status.Error(codes.InvalidArgument, "spec payload is nil")
		}
		if err := p.Spec.Validate(); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		specs = []*holismv1.TaskSpec{p.Spec}
	case *holismv1.SubmitTaskRequest_Goal:
		specs, err = s.planner.Plan(ctx, project, p.Goal)
		if err != nil {
			return nil, statusFromError(err)
		}
	default:
		return nil, status.Error(codes.InvalidArgument, "SubmitTaskRequest must carry a spec or a goal")
	}
	if len(specs) == 0 {
		return nil, status.Error(codes.InvalidArgument, "planner returned no task specs")
	}
	var first *holismv1.Task
	for _, spec := range specs {
		t, err := s.store.Create(ctx, &holismv1.Task{
			ProjectId: req.GetProjectId(),
			Spec:      spec,
		})
		if err != nil {
			return nil, statusFromError(err)
		}
		s.publish(ctx, &holismv1.Event{
			Type:      holismv1.Event_TYPE_TASK_QUEUED,
			ProjectId: t.GetProjectId(),
			TaskId:    t.GetId(),
			Summary:   fmt.Sprintf("task %s queued in project %s: %s", t.GetId(), t.GetProjectId(), t.GetSpec().GetTitle()),
		})
		if first == nil {
			first = t
		}
	}
	return first, nil
}

// GetTask implements holismv1.MindServiceServer.
func (s *Server) GetTask(ctx context.Context, req *holismv1.GetTaskRequest) (*holismv1.Task, error) {
	if err := validateReq(req); err != nil {
		return nil, err
	}
	t, err := s.store.Get(ctx, req.GetId())
	if err != nil {
		return nil, statusFromError(err)
	}
	return t, nil
}

// ListTasks implements holismv1.MindServiceServer. project_id is required
// (generated buf.validate rule) and enforced again by the store filter.
func (s *Server) ListTasks(ctx context.Context, req *holismv1.ListTasksRequest) (*holismv1.ListTasksResponse, error) {
	if err := validateReq(req); err != nil {
		return nil, err
	}
	filter := taskstore.Filter{ProjectID: req.GetProjectId()}
	if req.GetStatus() != holismv1.TaskStatus_TASK_STATUS_UNSPECIFIED {
		st := req.GetStatus()
		filter.Status = &st
	}
	tasks, err := s.store.List(ctx, filter)
	if err != nil {
		return nil, statusFromError(err)
	}
	return &holismv1.ListTasksResponse{Tasks: tasks}, nil
}

// CancelTask implements holismv1.MindServiceServer. Only tasks in a
// non-terminal state can be cancelled.
func (s *Server) CancelTask(ctx context.Context, req *holismv1.CancelTaskRequest) (*holismv1.Task, error) {
	if err := validateReq(req); err != nil {
		return nil, err
	}
	t, err := s.store.Get(ctx, req.GetId())
	if err != nil {
		return nil, statusFromError(err)
	}
	switch t.GetStatus() {
	case holismv1.TaskStatus_TASK_STATUS_SUCCEEDED,
		holismv1.TaskStatus_TASK_STATUS_FAILED,
		holismv1.TaskStatus_TASK_STATUS_CANCELLED:
		return nil, status.Errorf(codes.FailedPrecondition, "task %s is already %s", t.GetId(), t.GetStatus())
	}
	t.Status = holismv1.TaskStatus_TASK_STATUS_CANCELLED
	updated, err := s.store.Update(ctx, t)
	if err != nil {
		return nil, statusFromError(err)
	}
	s.publish(ctx, &holismv1.Event{
		Type:      holismv1.Event_TYPE_TASK_CANCELLED,
		ProjectId: updated.GetProjectId(),
		TaskId:    updated.GetId(),
		Summary:   fmt.Sprintf("task %s cancelled", updated.GetId()),
	})
	return updated, nil
}

// SubscribeEvents implements holismv1.MindServiceServer: fans EventBus events
// matching the request filter out to the stream until the client disconnects.
func (s *Server) SubscribeEvents(req *holismv1.SubscribeEventsRequest, stream grpc.ServerStreamingServer[holismv1.Event]) error {
	if err := validateReq(req); err != nil {
		return err
	}
	ctx := stream.Context()
	ch, err := s.bus.Subscribe(ctx, eventbus.Filter{ProjectID: req.GetProjectId()})
	if err != nil {
		return statusFromError(err)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(ev); err != nil {
				return err
			}
		}
	}
}
