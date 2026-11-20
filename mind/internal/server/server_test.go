package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	holismv1 "github.com/torrefatto/holism/api/holism/v1"
	eventbusmemory "github.com/torrefatto/holism/mind/internal/eventbus/memory"
	"github.com/torrefatto/holism/mind/internal/planner/rules"
	"github.com/torrefatto/holism/mind/internal/registry"
	registrymemory "github.com/torrefatto/holism/mind/internal/registry/memory"
	taskstorememory "github.com/torrefatto/holism/mind/internal/taskstore/memory"
)

type fakeClock struct{ t time.Time }

func (f *fakeClock) Now() time.Time { return f.t }

type fakeIDs struct{ n int }

func (f *fakeIDs) NewID(prefix string) string {
	f.n++
	return fmt.Sprintf("%s-%03d", prefix, f.n)
}

// harness wires a Server over in-memory adapters with a deterministic clock,
// serves it on a bufconn listener (plaintext — mTLS is covered separately in
// tls_test.go), and returns a client plus handles to the adapters.
type harness struct {
	client   holismv1.MindServiceClient
	bus      *eventbusmemory.Bus
	registry *registrymemory.Registry
	clk      *fakeClock
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	reg := registrymemory.New(clk, 30*time.Second)
	bus := eventbusmemory.New(clk, &fakeIDs{})
	srv := New(Deps{
		Store:    taskstorememory.New(clk, &fakeIDs{}),
		Bus:      bus,
		Planner:  rules.New(),
		Registry: reg,
	})
	grpcSrv := grpc.NewServer()
	holismv1.RegisterMindServiceServer(grpcSrv, srv)
	lis := bufconn.Listen(1024 * 1024)
	go func() { _ = grpcSrv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = conn.Close()
		grpcSrv.Stop()
	})
	return &harness{
		client:   holismv1.NewMindServiceClient(conn),
		bus:      bus,
		registry: reg,
		clk:      clk,
	}
}

// eventFeed is a SubscribeEvents stream drained on a goroutine. openEvents
// synchronizes with the server-side subscription by publishing pings on the
// bus until one comes back (events published before the handler subscribed
// would be lost); the in-flight pings are then drained so subsequent reads see
// only real events.
type eventFeed struct {
	ch chan *holismv1.Event
}

func openEvents(t *testing.T, ctx context.Context, h *harness, req *holismv1.SubscribeEventsRequest) *eventFeed {
	t.Helper()
	stream, err := h.client.SubscribeEvents(ctx, req)
	require.NoError(t, err)
	feed := &eventFeed{ch: make(chan *holismv1.Event, 128)}
	go func() {
		defer close(feed.ch)
		for {
			ev, err := stream.Recv()
			if err != nil {
				return
			}
			feed.ch <- ev
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		require.NoError(t, h.bus.Publish(ctx, &holismv1.Event{ProjectId: req.GetProjectId(), Summary: pingSummary}))
		select {
		case ev, ok := <-feed.ch:
			require.True(t, ok, "event stream closed during subscription sync")
			if ev.GetSummary() == pingSummary {
				return feed
			}
		case <-time.After(20 * time.Millisecond):
		}
		require.True(t, time.Now().Before(deadline), "SubscribeEvents handler never subscribed to the bus")
	}
}

// pingSummary marks the synchronization events published by openEvents; recv
// and expectNone skip them so only real lifecycle events are asserted on.
const pingSummary = "__ping__"

func (f *eventFeed) recv(t *testing.T) *holismv1.Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		select {
		case ev, ok := <-f.ch:
			require.True(t, ok, "event stream closed unexpectedly")
			if ev.GetSummary() == pingSummary {
				continue
			}
			return ev
		case <-time.After(time.Until(deadline)):
			t.Fatal("timed out waiting for event")
			return nil
		}
	}
}

func (f *eventFeed) expectNone(t *testing.T, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		select {
		case ev, ok := <-f.ch:
			if !ok {
				return
			}
			if ev.GetSummary() != pingSummary {
				t.Fatalf("unexpected event: %+v", ev)
			}
		case <-time.After(time.Until(deadline)):
			return
		}
	}
}

func recvMindMsg(t *testing.T, stream grpc.BidiStreamingClient[holismv1.DroneMessage, holismv1.MindMessage]) *holismv1.MindMessage {
	t.Helper()
	msg, err := stream.Recv()
	require.NoError(t, err)
	return msg
}

func TestCreateGetListProjects(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	p, err := h.client.CreateProject(ctx, &holismv1.CreateProjectRequest{
		Name: "swarm", Goal: "ship it", Labels: []string{"env:prod"},
	})
	require.NoError(t, err)
	assert.Equal(t, "proj-001", p.GetId())
	assert.Equal(t, "swarm", p.GetName())
	assert.Equal(t, "ship it", p.GetGoal())

	got, err := h.client.GetProject(ctx, &holismv1.GetProjectRequest{Id: p.GetId()})
	require.NoError(t, err)
	assert.Equal(t, p.GetId(), got.GetId())

	_, err = h.client.GetProject(ctx, &holismv1.GetProjectRequest{Id: "nope"})
	assert.Equal(t, codes.NotFound, status.Code(err))

	_, err = h.client.CreateProject(ctx, &holismv1.CreateProjectRequest{})
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "empty name must fail generated validation")

	_, err = h.client.GetProject(ctx, &holismv1.GetProjectRequest{})
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "empty id must fail generated validation")

	_, err = h.client.CreateProject(ctx, &holismv1.CreateProjectRequest{Name: "other"})
	require.NoError(t, err)
	all, err := h.client.ListProjects(ctx, &holismv1.ListProjectsRequest{})
	require.NoError(t, err)
	require.Len(t, all.GetProjects(), 2)
	filtered, err := h.client.ListProjects(ctx, &holismv1.ListProjectsRequest{Label: "env:prod"})
	require.NoError(t, err)
	require.Len(t, filtered.GetProjects(), 1)
	assert.Equal(t, p.GetId(), filtered.GetProjects()[0].GetId())
}

func TestSubmitTaskValidation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	_, err := h.client.SubmitTask(ctx, &holismv1.SubmitTaskRequest{
		Payload: &holismv1.SubmitTaskRequest_Spec{Spec: &holismv1.TaskSpec{Title: "t"}},
	})
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "empty project_id must fail generated buf.validate rules")

	_, err = h.client.SubmitTask(ctx, &holismv1.SubmitTaskRequest{
		ProjectId: "missing",
		Payload:   &holismv1.SubmitTaskRequest_Spec{Spec: &holismv1.TaskSpec{Title: "t"}},
	})
	assert.Equal(t, codes.NotFound, status.Code(err), "unknown project must fail the referential check")

	p, err := h.client.CreateProject(ctx, &holismv1.CreateProjectRequest{Name: "p"})
	require.NoError(t, err)
	_, err = h.client.SubmitTask(ctx, &holismv1.SubmitTaskRequest{ProjectId: p.GetId()})
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "neither spec nor goal must be rejected")

	_, err = h.client.SubmitTask(ctx, &holismv1.SubmitTaskRequest{
		ProjectId: p.GetId(),
		Payload:   &holismv1.SubmitTaskRequest_Goal{Goal: "   "},
	})
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "blank goal must fail the planner")
}

func TestSubmitTaskSpecAndGoal(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p, err := h.client.CreateProject(ctx, &holismv1.CreateProjectRequest{Name: "p", Goal: "g"})
	require.NoError(t, err)

	task, err := h.client.SubmitTask(ctx, &holismv1.SubmitTaskRequest{
		ProjectId: p.GetId(),
		Payload: &holismv1.SubmitTaskRequest_Spec{Spec: &holismv1.TaskSpec{
			Title:                  "do x",
			RequiredCapabilities: []string{"tool:kubectl"},
		}},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, task.GetId())
	assert.Equal(t, p.GetId(), task.GetProjectId())
	assert.Equal(t, holismv1.TaskStatus_TASK_STATUS_QUEUED, task.GetStatus())
	assert.Equal(t, "do x", task.GetSpec().GetTitle())
	assert.Positive(t, task.GetCreatedAtUnix())

	goalTask, err := h.client.SubmitTask(ctx, &holismv1.SubmitTaskRequest{
		ProjectId: p.GetId(),
		Payload:   &holismv1.SubmitTaskRequest_Goal{Goal: "fix the deploy\nand the tests"},
	})
	require.NoError(t, err)
	assert.Equal(t, "fix the deploy", goalTask.GetSpec().GetTitle(), "rules planner titles the task from the goal's first line")
	assert.Equal(t, "fix the deploy\nand the tests", goalTask.GetSpec().GetDescription())

	got, err := h.client.GetTask(ctx, &holismv1.GetTaskRequest{Id: task.GetId()})
	require.NoError(t, err)
	assert.Equal(t, task.GetId(), got.GetId())
	_, err = h.client.GetTask(ctx, &holismv1.GetTaskRequest{Id: "missing"})
	assert.Equal(t, codes.NotFound, status.Code(err))
}

func TestListTasks(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p, err := h.client.CreateProject(ctx, &holismv1.CreateProjectRequest{Name: "p"})
	require.NoError(t, err)

	_, err = h.client.ListTasks(ctx, &holismv1.ListTasksRequest{})
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "project_id is required in the filter")

	_, err = h.client.ListTasks(ctx, &holismv1.ListTasksRequest{ProjectId: "missing"})
	require.NoError(t, err, "listing in an unknown project is an empty list, not an error")

	t1, err := h.client.SubmitTask(ctx, &holismv1.SubmitTaskRequest{
		ProjectId: p.GetId(),
		Payload:   &holismv1.SubmitTaskRequest_Spec{Spec: &holismv1.TaskSpec{Title: "one"}},
	})
	require.NoError(t, err)
	t2, err := h.client.SubmitTask(ctx, &holismv1.SubmitTaskRequest{
		ProjectId: p.GetId(),
		Payload:   &holismv1.SubmitTaskRequest_Spec{Spec: &holismv1.TaskSpec{Title: "two"}},
	})
	require.NoError(t, err)
	_, err = h.client.CancelTask(ctx, &holismv1.CancelTaskRequest{Id: t2.GetId()})
	require.NoError(t, err)

	all, err := h.client.ListTasks(ctx, &holismv1.ListTasksRequest{ProjectId: p.GetId()})
	require.NoError(t, err)
	require.Len(t, all.GetTasks(), 2)
	assert.Equal(t, t1.GetId(), all.GetTasks()[0].GetId())

	queued, err := h.client.ListTasks(ctx, &holismv1.ListTasksRequest{
		ProjectId: p.GetId(),
		Status:    holismv1.TaskStatus_TASK_STATUS_QUEUED,
	})
	require.NoError(t, err)
	require.Len(t, queued.GetTasks(), 1)
	assert.Equal(t, t1.GetId(), queued.GetTasks()[0].GetId())
}

func TestCancelTask(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p, err := h.client.CreateProject(ctx, &holismv1.CreateProjectRequest{Name: "p"})
	require.NoError(t, err)
	task, err := h.client.SubmitTask(ctx, &holismv1.SubmitTaskRequest{
		ProjectId: p.GetId(),
		Payload:   &holismv1.SubmitTaskRequest_Spec{Spec: &holismv1.TaskSpec{Title: "t"}},
	})
	require.NoError(t, err)

	cancelled, err := h.client.CancelTask(ctx, &holismv1.CancelTaskRequest{Id: task.GetId()})
	require.NoError(t, err)
	assert.Equal(t, holismv1.TaskStatus_TASK_STATUS_CANCELLED, cancelled.GetStatus())

	_, err = h.client.CancelTask(ctx, &holismv1.CancelTaskRequest{Id: task.GetId()})
	assert.Equal(t, codes.FailedPrecondition, status.Code(err), "terminal task cannot be cancelled again")

	_, err = h.client.CancelTask(ctx, &holismv1.CancelTaskRequest{Id: "missing"})
	assert.Equal(t, codes.NotFound, status.Code(err))

	_, err = h.client.CancelTask(ctx, &holismv1.CancelTaskRequest{})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestSubscribeEventsFanOut(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	events := openEvents(t, ctx, h, &holismv1.SubscribeEventsRequest{})
	projectOnly := openEvents(t, ctx, h, &holismv1.SubscribeEventsRequest{ProjectId: "proj-001"})

	p, err := h.client.CreateProject(ctx, &holismv1.CreateProjectRequest{Name: "p"})
	require.NoError(t, err)
	assert.Equal(t, "proj-001", p.GetId(), "fake idgen keeps event project ids predictable")
	task, err := h.client.SubmitTask(ctx, &holismv1.SubmitTaskRequest{
		ProjectId: p.GetId(),
		Payload:   &holismv1.SubmitTaskRequest_Spec{Spec: &holismv1.TaskSpec{Title: "t"}},
	})
	require.NoError(t, err)
	_, err = h.client.CancelTask(ctx, &holismv1.CancelTaskRequest{Id: task.GetId()})
	require.NoError(t, err)

	ev1 := events.recv(t)
	assert.Equal(t, holismv1.Event_TYPE_PROJECT_CREATED, ev1.GetType())
	assert.Equal(t, p.GetId(), ev1.GetProjectId())
	assert.NotEmpty(t, ev1.GetId(), "bus stamps event ids")
	assert.Equal(t, h.clk.Now().Unix(), ev1.GetTimestampUnix(), "bus stamps timestamps")

	ev2 := events.recv(t)
	assert.Equal(t, holismv1.Event_TYPE_TASK_QUEUED, ev2.GetType())
	assert.Equal(t, p.GetId(), ev2.GetProjectId())
	assert.Equal(t, task.GetId(), ev2.GetTaskId())

	ev3 := events.recv(t)
	assert.Equal(t, holismv1.Event_TYPE_TASK_CANCELLED, ev3.GetType())
	assert.Equal(t, task.GetId(), ev3.GetTaskId())

	// The project-filtered subscriber saw all three (they all belong to proj-001).
	assert.Equal(t, holismv1.Event_TYPE_PROJECT_CREATED, projectOnly.recv(t).GetType())
	assert.Equal(t, holismv1.Event_TYPE_TASK_QUEUED, projectOnly.recv(t).GetType())
	assert.Equal(t, holismv1.Event_TYPE_TASK_CANCELLED, projectOnly.recv(t).GetType())

	// A filter for another project sees nothing even when a new project appears.
	other := openEvents(t, ctx, h, &holismv1.SubscribeEventsRequest{ProjectId: "other"})
	_, err = h.client.CreateProject(ctx, &holismv1.CreateProjectRequest{Name: "p2"})
	require.NoError(t, err)
	assert.Equal(t, holismv1.Event_TYPE_PROJECT_CREATED, events.recv(t).GetType())
	other.expectNone(t, 200*time.Millisecond)
}

func TestConnectRegisterFirst(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// A heartbeat as first message must be rejected.
	stream, err := h.client.Connect(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&holismv1.DroneMessage{
		Id:      "m1",
		Payload: &holismv1.DroneMessage_Heartbeat{Heartbeat: &holismv1.Heartbeat{DroneId: "d1"}},
	}))
	_, err = stream.Recv()
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err), "first message must be Register")

	// An empty message as first message must be rejected too.
	stream2, err := h.client.Connect(ctx)
	require.NoError(t, err)
	require.NoError(t, stream2.Send(&holismv1.DroneMessage{Id: "m1"}))
	_, err = stream2.Recv()
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))

	// A Register with an empty drone_id fails generated validation.
	stream3, err := h.client.Connect(ctx)
	require.NoError(t, err)
	require.NoError(t, stream3.Send(&holismv1.DroneMessage{
		Id:      "m1",
		Payload: &holismv1.DroneMessage_Register{Register: &holismv1.Register{}},
	}))
	_, err = stream3.Recv()
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))

	// Nothing was registered.
	ready, err := h.registry.Ready(ctx, registry.Filter{})
	require.NoError(t, err)
	assert.Empty(t, ready)
}

func TestConnectRegisterHeartbeatBye(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	events := openEvents(t, ctx, h, &holismv1.SubscribeEventsRequest{})

	stream, err := h.client.Connect(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&holismv1.DroneMessage{
		Id: "m1",
		Payload: &holismv1.DroneMessage_Register{Register: &holismv1.Register{
			DroneId:      "d1",
			Labels:       []string{"gpu"},
			Capabilities: []*holismv1.Capability{{Name: "tool:kubectl", Description: "runs kubectl"}},
		}},
	}))
	msg := recvMindMsg(t, stream)
	assert.NotNil(t, msg.GetPong(), "Register is answered with Pong")

	ready, err := h.registry.Ready(ctx, registry.Filter{})
	require.NoError(t, err)
	require.Len(t, ready, 1)
	assert.Equal(t, "d1", ready[0].ID)
	assert.Equal(t, []string{"gpu"}, ready[0].Labels)
	assert.Equal(t, []string{"tool:kubectl"}, ready[0].Capabilities)
	assert.True(t, ready[0].Ready)
	_, isOutbox := ready[0].Queue.(Outbox)
	assert.True(t, isOutbox, "the registry holds the stream's send queue")

	// Heartbeat renews the lease and is answered with Pong.
	require.NoError(t, stream.Send(&holismv1.DroneMessage{
		Id:      "m2",
		Payload: &holismv1.DroneMessage_Heartbeat{Heartbeat: &holismv1.Heartbeat{DroneId: "d1", Load: 3}},
	}))
	msg = recvMindMsg(t, stream)
	assert.NotNil(t, msg.GetPong())
	ready, err = h.registry.Ready(ctx, registry.Filter{})
	require.NoError(t, err)
	require.Len(t, ready, 1)
	assert.Equal(t, int32(3), ready[0].Load)

	// A heartbeat for another drone id is rejected and ends the stream; the
	// cleanup unregisters d2 before the error reaches the client.
	streamBad, err := h.client.Connect(ctx)
	require.NoError(t, err)
	require.NoError(t, streamBad.Send(&holismv1.DroneMessage{
		Id:      "m1",
		Payload: &holismv1.DroneMessage_Register{Register: &holismv1.Register{DroneId: "d2"}},
	}))
	recvMindMsg(t, streamBad) // Pong
	require.NoError(t, streamBad.Send(&holismv1.DroneMessage{
		Id:      "m2",
		Payload: &holismv1.DroneMessage_Heartbeat{Heartbeat: &holismv1.Heartbeat{DroneId: "d1"}},
	}))
	_, err = streamBad.Recv()
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	ready, err = h.registry.Ready(ctx, registry.Filter{})
	require.NoError(t, err)
	require.Len(t, ready, 1)
	assert.Equal(t, "d1", ready[0].ID, "d2 was unregistered when its stream broke")

	// Bye ends the stream gracefully and unregisters the drone.
	require.NoError(t, stream.Send(&holismv1.DroneMessage{
		Id:      "m3",
		Payload: &holismv1.DroneMessage_Bye{Bye: &holismv1.Bye{DroneId: "d1"}},
	}))
	_, err = stream.Recv()
	assert.ErrorIs(t, err, io.EOF, "Bye closes the stream")

	ready, err = h.registry.Ready(ctx, registry.Filter{})
	require.NoError(t, err)
	assert.Empty(t, ready)

	ev := events.recv(t)
	assert.Equal(t, holismv1.Event_TYPE_DRONE_REGISTERED, ev.GetType())
	assert.Equal(t, "d1", ev.GetDroneId())
	ev = events.recv(t)
	assert.Equal(t, holismv1.Event_TYPE_DRONE_REGISTERED, ev.GetType())
	assert.Equal(t, "d2", ev.GetDroneId())
	ev = events.recv(t)
	assert.Equal(t, holismv1.Event_TYPE_DRONE_UNREGISTERED, ev.GetType())
	assert.Equal(t, "d2", ev.GetDroneId())
	ev = events.recv(t)
	assert.Equal(t, holismv1.Event_TYPE_DRONE_UNREGISTERED, ev.GetType())
	assert.Equal(t, "d1", ev.GetDroneId())
}

func TestConnectTaskLifecycle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	events := openEvents(t, ctx, h, &holismv1.SubscribeEventsRequest{})

	p, err := h.client.CreateProject(ctx, &holismv1.CreateProjectRequest{Name: "p"})
	require.NoError(t, err)
	task, err := h.client.SubmitTask(ctx, &holismv1.SubmitTaskRequest{
		ProjectId: p.GetId(),
		Payload:   &holismv1.SubmitTaskRequest_Spec{Spec: &holismv1.TaskSpec{Title: "t"}},
	})
	require.NoError(t, err)

	stream, err := h.client.Connect(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&holismv1.DroneMessage{
		Id:      "m1",
		Payload: &holismv1.DroneMessage_Register{Register: &holismv1.Register{DroneId: "d1"}},
	}))
	recvMindMsg(t, stream) // Pong

	// TaskAck(accepted) moves the task QUEUED → ASSIGNED; Progress{WORKING}
	// then moves it ASSIGNED → RUNNING and is acked by DroneMessage.id.
	// Stream ordering guarantees the ack for m3 implies m2 was processed.
	require.NoError(t, stream.Send(&holismv1.DroneMessage{
		Id:      "m2",
		Payload: &holismv1.DroneMessage_TaskAck{TaskAck: &holismv1.TaskAck{TaskId: task.GetId(), Accepted: true}},
	}))
	require.NoError(t, stream.Send(&holismv1.DroneMessage{
		Id: "m3",
		Payload: &holismv1.DroneMessage_Progress{Progress: &holismv1.Progress{
			TaskId: task.GetId(),
			Status: holismv1.Progress_STATUS_WORKING,
		}},
	}))
	msg := recvMindMsg(t, stream)
	assert.Equal(t, "m3", msg.GetAck().GetRef())
	got, err := h.client.GetTask(ctx, &holismv1.GetTaskRequest{Id: task.GetId()})
	require.NoError(t, err)
	assert.Equal(t, holismv1.TaskStatus_TASK_STATUS_RUNNING, got.GetStatus())
	assert.Equal(t, "d1", got.GetAssignedDroneId())

	// Result decides the final status (SUCCEEDED on empty error) and is acked.
	require.NoError(t, stream.Send(&holismv1.DroneMessage{
		Id: "m4",
		Payload: &holismv1.DroneMessage_Result{Result: &holismv1.Result{
			TaskId: task.GetId(),
			Output: "all done",
		}},
	}))
	msg = recvMindMsg(t, stream)
	assert.Equal(t, "m4", msg.GetAck().GetRef())
	got, err = h.client.GetTask(ctx, &holismv1.GetTaskRequest{Id: task.GetId()})
	require.NoError(t, err)
	assert.Equal(t, holismv1.TaskStatus_TASK_STATUS_SUCCEEDED, got.GetStatus())
	assert.Equal(t, "all done", got.GetResult().GetOutput())

	// A Result with an error marks the task FAILED (second task on the same stream).
	task2, err := h.client.SubmitTask(ctx, &holismv1.SubmitTaskRequest{
		ProjectId: p.GetId(),
		Payload:   &holismv1.SubmitTaskRequest_Spec{Spec: &holismv1.TaskSpec{Title: "t2"}},
	})
	require.NoError(t, err)
	require.NoError(t, stream.Send(&holismv1.DroneMessage{
		Id:      "m5",
		Payload: &holismv1.DroneMessage_TaskAck{TaskAck: &holismv1.TaskAck{TaskId: task2.GetId(), Accepted: true}},
	}))
	require.NoError(t, stream.Send(&holismv1.DroneMessage{
		Id: "m6",
		Payload: &holismv1.DroneMessage_Result{Result: &holismv1.Result{
			TaskId: task2.GetId(),
			Error:  "boom",
		}},
	}))
	msg = recvMindMsg(t, stream)
	assert.Equal(t, "m6", msg.GetAck().GetRef())
	got, err = h.client.GetTask(ctx, &holismv1.GetTaskRequest{Id: task2.GetId()})
	require.NoError(t, err)
	assert.Equal(t, holismv1.TaskStatus_TASK_STATUS_FAILED, got.GetStatus())
	assert.Equal(t, "boom", got.GetResult().GetError())

	// Every lifecycle transition was fanned out on the event stream, in order.
	wantTypes := []holismv1.Event_Type{
		holismv1.Event_TYPE_PROJECT_CREATED,
		holismv1.Event_TYPE_TASK_QUEUED,
		holismv1.Event_TYPE_DRONE_REGISTERED,
		holismv1.Event_TYPE_TASK_ASSIGNED,
		holismv1.Event_TYPE_TASK_RUNNING,
		holismv1.Event_TYPE_TASK_SUCCEEDED,
		holismv1.Event_TYPE_TASK_QUEUED,
		holismv1.Event_TYPE_TASK_ASSIGNED,
		holismv1.Event_TYPE_TASK_FAILED,
	}
	for _, want := range wantTypes {
		assert.Equal(t, want, events.recv(t).GetType())
	}
}
