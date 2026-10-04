package fabric

import (
	"context"
	"fmt"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestFabricConsumerBacklogMetric(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		if err := mp.Shutdown(context.Background()); err != nil {
			t.Errorf("meter provider shutdown: %v", err)
		}
	})
	prev := otel.GetMeterProvider()
	t.Cleanup(func() { otel.SetMeterProvider(prev) })
	otel.SetMeterProvider(mp)

	ctx := testCtx(t)
	f := newFabric(t, Config{})
	blocked := make(chan struct{})
	received := make(chan struct{}, 3)
	unsub, err := f.SubscribeKind(ctx, KindMessagePosted, func(context.Context, EventRef) error {
		select {
		case <-blocked:
		case <-ctx.Done():
			return ctx.Err()
		}
		received <- struct{}{}
		return nil
	})
	if err != nil {
		t.Fatalf("SubscribeKind: %v", err)
	}

	const count = 3
	for i := range count {
		ref := EventRef{Tenant: "tenant-metric", Kind: KindMessagePosted, RowID: fmt.Sprintf("msg-%d", i)}
		subject, err := CommsSubject(ref.Tenant, ref.Kind)
		if err != nil {
			t.Fatalf("CommsSubject: %v", err)
		}
		if err := f.Publish(ctx, subject, ref); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}

	cons := kindConsumer(t, ctx, f)
	pollUntil(t, "backlog held by blocked callback", func() bool {
		info, err := cons.Info(ctx)
		return err == nil && info.NumAckPending >= 1
	})
	if got := collectBacklog(t, reader, ctx); got < 1 || got > count {
		t.Fatalf("backlog = %d, want between 1 and %d", got, count)
	}

	close(blocked)
	for range count {
		select {
		case <-received:
		case <-ctx.Done():
			t.Fatalf("callbacks did not drain: %v", ctx.Err())
		}
	}
	pollUntil(t, "consumer ack backlog drained", func() bool {
		info, err := cons.Info(ctx)
		return err == nil && info.NumPending+uint64(info.NumAckPending) == 0
	})
	if got := collectBacklog(t, reader, ctx); got != 0 {
		t.Fatalf("drained backlog = %d, want 0", got)
	}

	defer unsub()
	unsub()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect after unsubscribe: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != backlogMetricName {
				continue
			}
			gauge, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("backlog data = %T, want Gauge[int64]", m.Data)
			}
			for _, point := range gauge.DataPoints {
				if len(point.Attributes.ToSlice()) != 1 {
					t.Fatalf("backlog attributes = %v, want only consumer", point.Attributes)
				}
				consumer, ok := point.Attributes.Value(attribute.Key("consumer"))
				if ok && consumer.AsString() == durableName("compass.*.comms.message_posted") {
					t.Fatalf("consumer still observed after unsubscribe: %v", point)
				}
			}
		}
	}
}

func collectBacklog(t *testing.T, reader *sdkmetric.ManualReader, ctx context.Context) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != backlogMetricName {
				continue
			}
			gauge, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("backlog data = %T, want Gauge[int64]", m.Data)
			}
			for _, point := range gauge.DataPoints {
				consumer, ok := point.Attributes.Value(attribute.Key("consumer"))
				if ok && consumer.AsString() == durableName("compass.*.comms.message_posted") {
					return point.Value
				}
			}
		}
	}
	return -1
}

func TestFabricConsumerBacklogMetricSurvivesOneOfTwoSubscriptions(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		if err := mp.Shutdown(context.Background()); err != nil {
			t.Errorf("meter provider shutdown: %v", err)
		}
	})
	prev := otel.GetMeterProvider()
	t.Cleanup(func() { otel.SetMeterProvider(prev) })
	otel.SetMeterProvider(mp)

	ctx := testCtx(t)
	f := newFabric(t, Config{})
	noop := func(context.Context, EventRef) error { return nil }
	first, err := f.SubscribeKind(ctx, KindMessagePosted, noop)
	if err != nil {
		t.Fatalf("SubscribeKind first: %v", err)
	}
	second, err := f.SubscribeKind(ctx, KindMessagePosted, noop)
	if err != nil {
		t.Fatalf("SubscribeKind second: %v", err)
	}
	defer second()

	first()
	if got := collectBacklog(t, reader, ctx); got != 0 {
		t.Fatalf("backlog after one of two unsubscribes = %d, want 0 (still observed)", got)
	}
	second()
	if got := collectBacklog(t, reader, ctx); got != -1 {
		t.Fatalf("backlog after both unsubscribes = %d, want unobserved (-1)", got)
	}
}
