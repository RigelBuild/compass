package fabric

import (
	"context"
	"errors"
	"fmt"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"
)

// defaultRunnerEventBuffer is the default capacity of the channel Events
// returns. Deep enough to absorb a burst while a consumer is mid-write, shallow
// enough that a genuinely stalled consumer shows up as NATS slow-consumer drops
// (the intended best-effort semantic) rather than as unbounded memory growth.
const defaultRunnerEventBuffer = 256

// RunnerEvent is one Runner→Server agent event as it arrives off the fan-in
// subject.
//
// It carries the decoded PublishEventsRequest whole rather than flattening its
// fields: the hub's existing write-through path already consumes exactly that
// proto (RunnerSeq for gap detection, SessionId for routing, Frame for
// classification, IdempotencyKey for at-most-once commit), so re-projecting it
// into local fields would only create a shape that has to be un-projected again
// at the seam — and would silently drop any field a later proto revision adds.
type RunnerEvent struct {
	// RunnerID identifies the publishing Runner. The fan-in subject is shared
	// (compass.runner.events, queue-grouped), so unlike the per-Runner command
	// subject it does not encode the sender; the Runner stamps it in a header.
	// Empty if the publisher set no header — a consumer is EXPECTED to treat an
	// unattributed event as it treats an unknown frame: logged and counted,
	// never silently trusted. The fabric itself does not enforce that; it
	// reports the header verbatim.
	//
	// This value is publisher-asserted, NOT authenticated attribution: any
	// publisher on the shared fan-in subject can stamp any id. A consumer must
	// not treat it as identity — §Q2 puts the trust model on the Server's own
	// resolution of the Runner, and this header is only a routing/diagnostic
	// hint on top of it.
	RunnerID string
	// Event is the decoded envelope, never nil for an event read off the
	// channel.
	Event *compassv1internal.PublishEventsRequest
}

// RunnerIDHeader is the header the Runner stamps its id into when publishing on
// the shared fan-in subject. A header rather than a proto field because the wire
// (PublishEventsRequest) is frozen and carries no runner id — it was a per-stream
// property back when the Runner had one stream, and pub/sub has no stream to
// carry it.
const RunnerIDHeader = "Compass-Runner-Id"

// SendCommand pushes cmd to one Runner's command subject over core NATS.
//
// Core NATS, not JetStream, by design (§Q3): commands are best-effort. A command
// published while a Runner is offline is dropped, and that is correct — the
// delivery-cursor sweep in Postgres re-drives it on reconnect, so a stream here
// would add a second store of state Postgres already owns.
//
// Publish is fire-and-forget, so a nil return means "handed to the NATS client",
// not "the Runner got it". The flush against the caller's ctx below is what
// makes the error real: it surfaces a connection that cannot actually take the
// write, which is the failure a caller can act on.
func (f *Fabric) SendCommand(ctx context.Context, runnerID string, cmd *compassv1internal.SessionsResponse) error {
	if err := f.checkOpen(); err != nil {
		return err
	}
	if cmd == nil {
		return fmt.Errorf("fabric: SendCommand to runner %q requires a command", runnerID)
	}
	subject, err := RunnerCommandSubject(runnerID)
	if err != nil {
		return err
	}
	data, err := proto.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("fabric: marshaling command %q for runner %q: %w", cmd.GetRequestId(), runnerID, err)
	}
	if err := f.nc.Publish(subject, data); err != nil {
		return fmt.Errorf("fabric: publishing command %q to %q: %w", cmd.GetRequestId(), subject, err)
	}
	if err := f.flush(ctx); err != nil {
		return fmt.Errorf("fabric: flushing command %q to %q: %w", cmd.GetRequestId(), subject, err)
	}
	return nil
}

// Events returns a channel of Runner events, closed once ctx is done or the
// Fabric is closed — whichever happens first, so a consumer ranging over it
// terminates on either shutdown path rather than only on its own cancellation.
//
// The subscription joins RunnerEventsQueue, so with several Servers connected
// each event is delivered to exactly one of them — the fan-in half of the Q2
// topology, and what lets any Server handle any Runner's events without sticky
// connections.
//
// The channel is closed exactly once, after the subscription is drained: a
// receiver ranging over it sees every event NATS had already delivered and then
// a clean close, never a send on a closed channel.
func (f *Fabric) Events(ctx context.Context) (<-chan RunnerEvent, error) {
	if err := f.checkOpen(); err != nil {
		return nil, err
	}
	subject := RunnerEventsSubject()

	// A channel-backed subscription rather than a callback one: NATS owns the
	// buffering and applies its slow-consumer policy to it, so a stalled
	// receiver is dropped-and-reported instead of blocking the connection's
	// dispatcher.
	raw := make(chan *nats.Msg, f.cfg.runnerEventBuffer())
	sub, err := f.nc.QueueSubscribeSyncWithChan(subject, RunnerEventsQueue, raw)
	if err != nil {
		return nil, fmt.Errorf("fabric: queue-subscribing %q (group %q): %w", subject, RunnerEventsQueue, err)
	}
	// Flush so Events returns only once the server has registered the interest.
	// Without it a caller that subscribes and then triggers a Runner would race
	// its own first event — core NATS drops a message with no interest yet, and
	// the drop would be invisible.
	if err := f.flush(ctx); err != nil {
		if derr := sub.Unsubscribe(); derr != nil {
			f.log.WarnContext(ctx, "fabric: unsubscribing after a failed flush", "subject", subject, "error", derr)
		}
		return nil, fmt.Errorf("fabric: establishing the runner-events subscription on %q: %w", subject, err)
	}

	out := make(chan RunnerEvent, f.cfg.runnerEventBuffer())
	go f.pumpRunnerEvents(ctx, sub, raw, out)
	return out, nil
}

// pumpRunnerEvents decodes raw messages onto out until ctx is done or the
// fabric is closed, then drains the subscription and closes out. It owns both
// the subscription's and the channel's lifetime, so there is exactly one place
// either is torn down.
//
// f.teardown is a load-bearing case in BOTH selects below, not a belt-and-braces
// duplicate of ctx.Done(): nats.go does not close a ChanSubscription's channel
// when the connection closes, so a Close with an uncancelled ctx would otherwise
// leave this goroutine parked on raw forever — and out never closed under a
// consumer ranging over it.
func (f *Fabric) pumpRunnerEvents(ctx context.Context, sub *nats.Subscription, raw <-chan *nats.Msg, out chan<- RunnerEvent) {
	defer close(out)
	defer func() {
		// Drain rather than Unsubscribe: it lets NATS deliver what it has
		// already accepted for this subject before removing the interest,
		// mirroring Close's connection-level drain.
		//
		// An already-closed connection is the expected shutdown outcome, not a
		// failure: Close closes f.teardown before nc.Drain(), and the
		// connection-level drain reclaims every subscription itself. If that
		// finishes before this descheduled goroutine reaches the defer,
		// sub.Drain() returns ErrConnectionClosed for a subscription that WAS
		// drained — so warning on it would be a false alarm on a clean path.
		if err := sub.Drain(); err != nil && !errors.Is(err, nats.ErrConnectionClosed) {
			f.log.WarnContext(ctx, "fabric: draining the runner-events subscription failed",
				"subject", sub.Subject, "error", err)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-f.teardown:
			return
		case msg, ok := <-raw:
			if !ok {
				// The subscription's channel closed under us (connection gone).
				return
			}
			ev, err := decodeRunnerEvent(msg)
			if err != nil {
				// Best-effort plane: one undecodable event must not tear down
				// the fan-in for every other Runner. Surfaced, never silent.
				f.log.ErrorContext(ctx, "fabric: dropping an undecodable runner event",
					"subject", msg.Subject, "error", err)
				continue
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			case <-f.teardown:
				return
			}
		}
	}
}

// decodeRunnerEvent unmarshals one fan-in message into a RunnerEvent.
func decodeRunnerEvent(msg *nats.Msg) (RunnerEvent, error) {
	var req compassv1internal.PublishEventsRequest
	if err := proto.Unmarshal(msg.Data, &req); err != nil {
		return RunnerEvent{}, fmt.Errorf("fabric: unmarshaling a runner event from %d bytes: %w", len(msg.Data), err)
	}
	return RunnerEvent{RunnerID: msg.Header.Get(RunnerIDHeader), Event: &req}, nil
}
