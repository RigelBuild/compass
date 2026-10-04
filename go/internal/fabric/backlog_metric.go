package fabric

import (
	"context"
	"math"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	instrumentationScope = "github.com/RigelBuild/compass/go/internal/fabric"
	backlogMetricName    = "compass.fabric.consumer.backlog"
	// backlogReadBudget caps one collection's Info round trips, so a NATS outage
	// costs a missing point, not a stalled export.
	backlogReadBudget = 2 * time.Second
)

// liveConsumer is ref-counted: two subscriptions on one subject share a durable.
type liveConsumer struct {
	consumer jetstream.Consumer
	refs     int
}

// Every instance on a shared durable reports the same value, so aggregate with
// max, not sum. The attribute is the hashed durable name, never a tenant id.
func (f *Fabric) registerBacklogMetric() {
	meter := otel.Meter(instrumentationScope)
	gauge, err := meter.Int64ObservableGauge(
		backlogMetricName,
		metric.WithUnit("{message}"),
		metric.WithDescription("Messages waiting on or held by a durable comms consumer."),
	)
	if err != nil {
		f.log.Warn("fabric: failed to create consumer backlog gauge", "error", err)
		return
	}
	registration, err := meter.RegisterCallback(func(ctx context.Context, observer metric.Observer) error {
		f.consumerMu.RLock()
		consumers := make(map[string]jetstream.Consumer, len(f.consumers))
		for name, live := range f.consumers {
			consumers[name] = live.consumer
		}
		f.consumerMu.RUnlock()

		ctx, cancel := context.WithTimeout(ctx, backlogReadBudget)
		defer cancel()
		for name, cons := range consumers {
			info, err := cons.Info(ctx)
			if err != nil {
				f.log.DebugContext(ctx, "fabric: reading consumer backlog failed", "consumer", name, "error", err)
				continue
			}
			observer.ObserveInt64(gauge, backlog(info), metric.WithAttributes(attribute.String("consumer", name)))
		}
		return nil
	}, gauge)
	if err != nil {
		f.log.Warn("fabric: failed to register consumer backlog callback", "error", err)
		return
	}
	f.backlogRegistration = registration
}

// backlog clamps instead of wrapping; a real consumer never nears MaxInt64.
func backlog(info *jetstream.ConsumerInfo) int64 {
	ackPending := uint64(max(info.NumAckPending, 0))
	total := info.NumPending + ackPending
	if total < info.NumPending || total > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(total)
}

func (f *Fabric) trackConsumer(name string, cons jetstream.Consumer) {
	f.consumerMu.Lock()
	defer f.consumerMu.Unlock()
	live, ok := f.consumers[name]
	if !ok {
		live.consumer = cons
	}
	live.refs++
	f.consumers[name] = live
}

func (f *Fabric) untrackConsumer(name string) {
	f.consumerMu.Lock()
	defer f.consumerMu.Unlock()
	live, ok := f.consumers[name]
	if !ok {
		return
	}
	if live.refs--; live.refs > 0 {
		f.consumers[name] = live
		return
	}
	delete(f.consumers, name)
}
