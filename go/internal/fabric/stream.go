package fabric

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// JetStream topology defaults for the comms event plane. Every one is
// overridable through Config; these are the values the deployment runs unless a
// stack config says otherwise, and SUBJECTS.md is their written spec.
const (
	// DefaultStreamName is the single stream capturing every tenant's comms
	// events. One stream rather than one per tenant: subject filtering on the
	// consumer already isolates tenants, and a stream per tenant would make
	// tenant creation a JetStream admin operation.
	DefaultStreamName = "COMPASS_COMMS"

	// DefaultMaxDeliver bounds delivery attempts per message before the fabric
	// parks it on DLQSubject. Finite by requirement (§Q3: "max_deliver with a
	// dead-letter subject so a poison message parks instead of redelivering
	// forever"); 5 is enough to ride out a transient subscriber fault and small
	// enough that a genuine poison message parks in seconds rather than hours.
	DefaultMaxDeliver = 5

	// DefaultAckWait is how long the server waits for an explicit ack before
	// redelivering. It only governs a subscriber that hangs or dies mid-callback
	// — a callback that fails is Nak'd, which redelivers immediately.
	DefaultAckWait = 30 * time.Second

	// DefaultDuplicateWindow is the publish-side dedup window. Two Servers
	// publishing the same logical change, or one retrying a publish whose ack
	// was lost, collapse to one stored message inside it (EventRef.msgID is the
	// key). Sized to comfortably exceed any plausible publish retry.
	DefaultDuplicateWindow = 2 * time.Minute

	// DefaultMaxAge bounds the stream's replay window. JetStream is a transport
	// whose state is disposable (§Global Constraints), so retaining events past
	// the point where the Postgres delivery cursor is the only sane recovery
	// path buys nothing but disk. A subscriber further behind than this
	// recovers by cursor sweep, not by replay.
	DefaultMaxAge = 24 * time.Hour

	// DefaultReplicas is the stream/consumer replica count. A single-node NATS
	// is R1 by construction; a clustered deployment sets 3 (§Q3: "file storage
	// with R3 replication when NATS runs clustered"). Postgres is the recovery
	// truth either way, so this is a durability optimization, not a correctness
	// requirement.
	DefaultReplicas = 1

	// MinInactiveThreshold floors the consumer reap threshold. A live Consume
	// re-pulls at least every 30s and a callback is bounded by AckWait, so an
	// hour can only elapse without a pull when no Server holds the consumer.
	MinInactiveThreshold = time.Hour
)

// streamConfig omits sync_interval because it is a NATS server FILE STORE option, not a StreamConfig field.
// Set it in server config and embedded/test options instead.
func (c Config) streamConfig() jetstream.StreamConfig {
	return jetstream.StreamConfig{
		Name:        c.streamName(),
		Description: "Compass comms events (EventRef references; Postgres is the truth)",
		Subjects:    []string{commsStreamSubjects},
		// Limits retention (the default): a message ages out on MaxAge rather
		// than being removed once acked, so a second consumer group and a
		// bounded replay both stay possible.
		Retention:  jetstream.LimitsPolicy,
		Storage:    jetstream.FileStorage,
		Replicas:   c.replicas(),
		Discard:    jetstream.DiscardOld,
		MaxAge:     c.maxAge(),
		Duplicates: c.duplicateWindow(),
	}
}

// consumerConfig shares one named durable per subject across Server instances, providing queue-group semantics and restart resume.
func (c Config) consumerConfig(subject string) jetstream.ConsumerConfig {
	return jetstream.ConsumerConfig{
		Durable:       durableName(subject),
		Description:   "comms EventRef fan-out for " + subject,
		FilterSubject: subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       c.ackWait(),
		MaxDeliver:    c.maxDeliver(),
		Replicas:      c.replicas(),
		// Reaps a durable no Server has pulled from; see inactiveThreshold.
		InactiveThreshold: c.inactiveThreshold(),
	}
}

// inactiveThreshold defaults to max(2 × MaxAge, MinInactiveThreshold). A reaped
// consumer is recreated with DeliverAll, so its acked events must age out first;
// the floor keeps a short MaxAge from reaping a live subscription mid-callback.
func (c Config) inactiveThreshold() time.Duration {
	if c.testInactiveThreshold > 0 {
		return c.testInactiveThreshold
	}
	// Saturate below MaxInt64: the server adds up to ~1s of jitter, which must not overflow.
	const ceiling = time.Duration(math.MaxInt64) - 2*time.Second
	age := c.maxAge()
	if age > ceiling/2 {
		return ceiling
	}
	return max(2*age, MinInactiveThreshold)
}

// durableName hashes the subject: replacing dots is not injective because tokens may contain underscores.
// Collisions could repoint another tenant's shared consumer; the prefix remains greppable and the full hash fits the limit.
func durableName(subject string) string {
	sum := sha256.Sum256([]byte(subject))
	return "comms-" + hex.EncodeToString(sum[:])
}

// ensureStream lazily creates or updates the stream using the first caller's context.
// CreateOrUpdate converges restarts, additional servers, and config changes.
func (f *Fabric) ensureStream(ctx context.Context) (jetstream.Stream, error) {
	f.streamMu.Lock()
	defer f.streamMu.Unlock()
	if f.stream != nil {
		return f.stream, nil
	}
	s, err := f.js.CreateOrUpdateStream(ctx, f.cfg.streamConfig())
	if err != nil {
		// Not cached: a failed ensure must be retryable on the next call, never
		// poison the fabric for its lifetime.
		return nil, fmt.Errorf("fabric: ensuring stream %s: %w", f.cfg.streamName(), err)
	}
	f.stream = s
	return s, nil
}
