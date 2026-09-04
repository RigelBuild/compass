package fabric

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
)

// streamConfig is the COMPASS_COMMS stream configuration.
//
// # Where sync_interval lives
//
// The record specifies `sync_interval: 100ms` for this stream — a bounded fsync
// window, because JetStream's default sync behavior can lose acknowledged writes
// under power failure. It is deliberately NOT set here: sync_interval is a
// nats-server FILE STORE option, not a per-stream one, so jetstream.StreamConfig
// (nats.go v1.53.1) exposes no field for it. It is set on the NATS process:
//
//   - server config:  jetstream { sync_interval: "100ms" }
//   - Go embedded/test: server.Options.SyncInterval = 100 * time.Millisecond
//
// The stack's NATS service config is where the deployment sets it (the sibling
// nats-container task); testServer in this package sets the server.Options field
// so the tests exercise the record's value rather than the server default.
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

// consumerConfig is the durable pull-consumer configuration for one subscribed
// subject. Durable and named deterministically from the subject so every Server
// instance subscribing to that subject shares one consumer — which is what gives
// the delivery plane its queue-group semantics (§Q3: each event is claimed by
// exactly one worker) and what lets a restarted Server resume where it stopped.
func (c Config) consumerConfig(subject string) jetstream.ConsumerConfig {
	return jetstream.ConsumerConfig{
		Durable:       durableName(subject),
		Description:   "comms EventRef fan-out for " + subject,
		FilterSubject: subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       c.ackWait(),
		MaxDeliver:    c.maxDeliver(),
		Replicas:      c.replicas(),
	}
}

// durableName derives a JetStream durable consumer name for a subject.
// Consumer names cannot contain "." (nor whitespace, "*", ">", or a path
// separator), and a subject's tokens can legally contain "_" (ValidSubjectToken
// permits it — every snake_case EventKind uses it), so a "."→"_" substitution
// is NOT injective: compass.a.comms.b_comms_c and compass.a_comms_b.comms.c
// would collapse to one name and the second Subscribe would silently re-point
// the first's shared consumer FilterSubject (a cross-tenant mis-delivery).
// Hashing the subject is injective by construction; the "comms-" prefix keeps
// the name greppable, and the consumer's Description/FilterSubject still carry
// the readable subject for operators. Untruncated: "comms-" + 64 hex is 70
// chars, far inside JetStream's 255-char limit, and truncating would reintroduce
// the collision surface this exists to remove.
func durableName(subject string) string {
	sum := sha256.Sum256([]byte(subject))
	return "comms-" + hex.EncodeToString(sum[:])
}

// ensureStream creates or updates the comms stream, idempotently, and caches it.
// Called lazily from the first Publish/Subscribe rather than from New because the
// frozen New(cfg Config) signature carries no context — deriving the topology
// call from the first caller's ctx keeps the cancellation chain intact instead of
// rooting a fresh one.
//
// CreateOrUpdateStream (not CreateStream) so a container restart, a second
// Server, and a config change all converge on the same stream rather than
// racing or failing.
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
