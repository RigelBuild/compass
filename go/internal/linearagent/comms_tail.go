package linearagent

import (
	"context"
	"log/slog"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
)

// TailComms feeds live comms bus events to OnCommsEvent until ctx ends or the
// bus closes. Replay is skipped: nothing is armed before the tail starts. A lag
// overrun re-subscribes; a reply lost in the gap leaves that session in Thinking.
func (d *Dispatcher) TailComms(ctx context.Context, bus *events.Bus[*compassv1.SubscribeCommsResponse]) error {
	sub, err := bus.Subscribe(0, bus.InstanceEpoch())
	if err != nil {
		return err
	}
	defer func() { sub.Cancel() }()
	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-sub.Live:
			if !ok {
				if !sub.Lagged() {
					return nil
				}
				fresh, err := bus.Subscribe(0, bus.InstanceEpoch())
				if err != nil {
					slog.ErrorContext(ctx, "linearagent: re-subscribe after comms bus overrun", "error", err)
					return err
				}
				sub.Cancel()
				sub = fresh
				continue
			}
			d.OnCommsEvent(ctx, event.Payload)
		}
	}
}
