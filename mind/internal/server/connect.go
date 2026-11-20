package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	holismv1 "github.com/torrefatto/holism/api/holism/v1"
	"github.com/torrefatto/holism/mind/internal/registry"
	"github.com/torrefatto/holism/mind/internal/taskstore"
)

// outboxBuffer is the per-drone send-queue capacity between the read pump and
// the write pump of a Connect stream.
const outboxBuffer = 64

// Outbox is the send queue of a drone's live Connect stream, handed to the
// registry as registry.Drone.Queue. The scheduler (and any other mind
// component) type-asserts the handle and sends MindMessages into it; the
// stream's write pump drains it onto the wire.
type Outbox chan *holismv1.MindMessage

// Connect implements holismv1.MindServiceServer: the drone channel. The first
// message on the stream must be a Register; the drone is then entered in the
// registry with its Outbox as queue handle, and read/write pumps move messages
// between the stream and the rest of the mind until the stream ends.
func (s *Server) Connect(stream grpc.BidiStreamingServer[holismv1.DroneMessage, holismv1.MindMessage]) error {
	ctx := stream.Context()

	first, err := stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	reg := first.GetRegister()
	if reg == nil {
		return status.Error(codes.FailedPrecondition, "Connect: first message must be a Register")
	}
	if err := reg.Validate(); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if identity, err := PeerIdentity(ctx); err == nil {
		slog.Debug("server: drone connected", "drone_id", reg.GetDroneId(), "peer_identity", identity)
	}

	outbox := make(Outbox, outboxBuffer)
	if _, err := s.registry.Register(ctx, &registry.Drone{
		ID:           reg.GetDroneId(),
		Labels:       reg.GetLabels(),
		Capabilities: capabilityNames(reg.GetCapabilities()),
		Queue:        outbox,
	}); err != nil {
		return statusFromError(err)
	}
	s.publish(ctx, &holismv1.Event{
		Type:     holismv1.Event_TYPE_DRONE_REGISTERED,
		DroneId:  reg.GetDroneId(),
		Summary:  fmt.Sprintf("drone %s registered", reg.GetDroneId()),
	})
	enqueue(ctx, outbox, pong())

	// Write pump: drains the outbox onto the stream until it is closed or the
	// stream breaks.
	sendErr := make(chan error, 1)
	go func() {
		for msg := range outbox {
			if err := stream.Send(msg); err != nil {
				sendErr <- err
				return
			}
		}
		sendErr <- nil
	}()

	// Read pump runs on the handler goroutine.
	rerr := s.readLoop(ctx, stream, reg.GetDroneId(), outbox)

	close(outbox)
	select {
	case werr := <-sendErr:
		if rerr == nil {
			rerr = werr
		}
	case <-ctx.Done():
	}
	// The stream is over: drop the drone from the registry (idempotent —
	// a Bye already ended the loop, but unregister happens here, once).
	if err := s.registry.Unregister(context.Background(), reg.GetDroneId()); err != nil && !errors.Is(err, registry.ErrNotFound) {
		slog.Warn("server: failed to unregister drone", "drone_id", reg.GetDroneId(), "error", err)
	}
	s.publish(context.Background(), &holismv1.Event{
		Type:     holismv1.Event_TYPE_DRONE_UNREGISTERED,
		DroneId:  reg.GetDroneId(),
		Summary:  fmt.Sprintf("drone %s unregistered", reg.GetDroneId()),
	})
	if rerr != nil && errors.Is(rerr, io.EOF) {
		return nil
	}
	return rerr
}

// readLoop receives DroneMessages until the stream ends (io.EOF), the drone
// says Bye, or a protocol violation occurs.
func (s *Server) readLoop(ctx context.Context, stream grpc.BidiStreamingServer[holismv1.DroneMessage, holismv1.MindMessage], droneID string, outbox Outbox) error {
	for {
		msg, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if err := msg.Validate(); err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		switch p := msg.GetPayload().(type) {
		case *holismv1.DroneMessage_Register:
			return status.Error(codes.FailedPrecondition, "Connect: Register is only valid as the first message")
		case *holismv1.DroneMessage_Heartbeat:
			hb := p.Heartbeat
			if hb.GetDroneId() != droneID {
				return status.Errorf(codes.InvalidArgument, "heartbeat drone_id %q does not match registered drone %q", hb.GetDroneId(), droneID)
			}
			if _, err := s.registry.Heartbeat(ctx, droneID, hb.GetLoad()); err != nil {
				return statusFromError(err)
			}
			enqueue(ctx, outbox, pong())
		case *holismv1.DroneMessage_TaskAck:
			if err := s.handleTaskAck(ctx, droneID, p.TaskAck); err != nil {
				return err
			}
		case *holismv1.DroneMessage_Progress:
			if err := s.handleProgress(ctx, msg.GetId(), p.Progress, outbox); err != nil {
				return err
			}
		case *holismv1.DroneMessage_Result:
			if err := s.handleResult(ctx, msg.GetId(), p.Result, outbox); err != nil {
				return err
			}
		case *holismv1.DroneMessage_Bye:
			return nil
		default:
			return status.Error(codes.InvalidArgument, "DroneMessage carries no payload")
		}
	}
}

// handleTaskAck applies exactly-once assignment (design §4.2.1): a task moves
// QUEUED → ASSIGNED only on TaskAck(accepted=true) from the registered drone.
// A rejected ack leaves the task queued for the scheduler to resubmit.
func (s *Server) handleTaskAck(ctx context.Context, droneID string, ack *holismv1.TaskAck) error {
	t, err := s.store.Get(ctx, ack.GetTaskId())
	if errors.Is(err, taskstore.ErrNotFound) {
		return nil // unknown task: nothing to transition (TaskAck is not acked on the stream)
	}
	if err != nil {
		return statusFromError(err)
	}
	if !ack.GetAccepted() || t.GetStatus() != holismv1.TaskStatus_TASK_STATUS_QUEUED {
		return nil
	}
	t.Status = holismv1.TaskStatus_TASK_STATUS_ASSIGNED
	t.AssignedDroneId = droneID
	if _, err := s.store.Update(ctx, t); err != nil {
		return statusFromError(err)
	}
	s.publish(ctx, &holismv1.Event{
		Type:      holismv1.Event_TYPE_TASK_ASSIGNED,
		ProjectId: t.GetProjectId(),
		TaskId:    t.GetId(),
		DroneId:   droneID,
		Summary:   fmt.Sprintf("task %s assigned to drone %s", t.GetId(), droneID),
	})
	return nil
}

// handleProgress applies a Progress update and acks it by DroneMessage.id.
// WORKING moves ASSIGNED → RUNNING; the final status is decided by the
// Result (§4.2.3), so DONE alone does not transition.
func (s *Server) handleProgress(ctx context.Context, msgID string, p *holismv1.Progress, outbox Outbox) error {
	defer enqueue(ctx, outbox, ack(msgID))
	t, err := s.store.Get(ctx, p.GetTaskId())
	if errors.Is(err, taskstore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return statusFromError(err)
	}
	if p.GetStatus() == holismv1.Progress_STATUS_WORKING && t.GetStatus() == holismv1.TaskStatus_TASK_STATUS_ASSIGNED {
		t.Status = holismv1.TaskStatus_TASK_STATUS_RUNNING
		if _, err := s.store.Update(ctx, t); err != nil {
			return statusFromError(err)
		}
		s.publish(ctx, &holismv1.Event{
			Type:      holismv1.Event_TYPE_TASK_RUNNING,
			ProjectId: t.GetProjectId(),
			TaskId:    t.GetId(),
			DroneId:   t.GetAssignedDroneId(),
			Summary:   fmt.Sprintf("task %s running on drone %s", t.GetId(), t.GetAssignedDroneId()),
		})
	}
	return nil
}

// handleResult applies the terminal Result (§4.2.3): the mind examines the
// result and sets SUCCEEDED (empty error) or FAILED, then acks by id.
func (s *Server) handleResult(ctx context.Context, msgID string, r *holismv1.Result, outbox Outbox) error {
	defer enqueue(ctx, outbox, ack(msgID))
	t, err := s.store.Get(ctx, r.GetTaskId())
	if errors.Is(err, taskstore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return statusFromError(err)
	}
	switch t.GetStatus() {
	case holismv1.TaskStatus_TASK_STATUS_ASSIGNED, holismv1.TaskStatus_TASK_STATUS_RUNNING:
	default:
		return nil // already terminal or cancelled: keep the existing status
	}
	t.Result = r
	if r.GetError() != "" {
		t.Status = holismv1.TaskStatus_TASK_STATUS_FAILED
	} else {
		t.Status = holismv1.TaskStatus_TASK_STATUS_SUCCEEDED
	}
	if _, err := s.store.Update(ctx, t); err != nil {
		return statusFromError(err)
	}
	evType := holismv1.Event_TYPE_TASK_SUCCEEDED
	if t.GetStatus() == holismv1.TaskStatus_TASK_STATUS_FAILED {
		evType = holismv1.Event_TYPE_TASK_FAILED
	}
	s.publish(ctx, &holismv1.Event{
		Type:      evType,
		ProjectId: t.GetProjectId(),
		TaskId:    t.GetId(),
		DroneId:   t.GetAssignedDroneId(),
		Summary:   fmt.Sprintf("task %s %s on drone %s", t.GetId(), t.GetStatus(), t.GetAssignedDroneId()),
	})
	return nil
}

// enqueue queues an outbound MindMessage for the write pump, giving up when
// the stream context is done (e.g. a dead stream whose write pump already
// exited) so the read loop can never block on a full outbox.
func enqueue(ctx context.Context, outbox Outbox, msg *holismv1.MindMessage) {
	select {
	case outbox <- msg:
	case <-ctx.Done():
	}
}

// capabilityNames extracts the capability names from declared Capabilities;
// matching is on names, per the contract.
func capabilityNames(caps []*holismv1.Capability) []string {
	if len(caps) == 0 {
		return nil
	}
	names := make([]string, 0, len(caps))
	for _, c := range caps {
		names = append(names, c.GetName())
	}
	return names
}

func pong() *holismv1.MindMessage {
	return &holismv1.MindMessage{Payload: &holismv1.MindMessage_Pong{Pong: &holismv1.Pong{}}}
}

func ack(ref string) *holismv1.MindMessage {
	return &holismv1.MindMessage{Payload: &holismv1.MindMessage_Ack{Ack: &holismv1.Ack{Ref: ref}}}
}
