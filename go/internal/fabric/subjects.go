package fabric

import (
	"fmt"
	"strings"
)

// The frozen subject grammar (§T3). The fabric owns the grammar: callers build a
// subject through the helpers below and hand the string to Publish / Subscribe /
// SendCommand, so no consumer ever concatenates a subject itself.
const (
	// subjectPrefix roots every Compass-owned subject.
	subjectPrefix = "compass"

	// commsStreamSubjects is the single wildcard the COMPASS_COMMS JetStream
	// stream captures: every tenant's every comms kind. It matches exactly what
	// CommsSubject builds — four tokens, tenant and kind wildcarded.
	commsStreamSubjects = subjectPrefix + "." + wildcardToken + ".comms." + wildcardToken

	// RunnerEventsQueue is the queue group every Server's RunnerFabric.Events
	// subscription joins, so one Runner event is handled by exactly one Server
	// instance (§Q3, "delivery queue groups": the three-hop model's hop 2).
	RunnerEventsQueue = "compass-runner-events"

	// DLQSubject is where a comms event that exhausted its delivery attempts is
	// parked. JetStream has no native dead-letter queue, so the fabric
	// implements the app-level pattern: publish to this subject, then Term() the
	// message so the server stops redelivering it. Parking is a diagnostic, not
	// a recovery path — recovery is always the Postgres row.
	DLQSubject = subjectPrefix + ".dlq.comms"
)

// CommsSubject builds the comms-event subject for a tenant and event kind:
// compass.<tenant>.comms.<kind>. It returns an error if tenant or kind is not a
// valid single subject token — a tenant id containing a "." is an upstream bug,
// and silently corrupting the subject would cross-wire tenants (see
// ValidSubjectToken).
func CommsSubject(tenant string, kind EventKind) (string, error) {
	if err := ValidSubjectToken("tenant", tenant); err != nil {
		return "", err
	}
	if err := ValidSubjectToken("event kind", string(kind)); err != nil {
		return "", err
	}
	return subjectPrefix + "." + tenant + ".comms." + string(kind), nil
}

// CommsWildcardSubject builds the TENANT-WILDCARD comms subject for one event
// kind: compass.*.comms.<kind>. It is the delivery plane's cross-tenant
// fan-in subject — the delivery consumer is a per-Server singleton serving
// every tenant, and each message publishes to a concrete
// compass.<tenant>.comms.<kind>, so catching every tenant needs one consumer
// whose FilterSubject wildcards the tenant token (§T3).
//
// Only the TENANT token is wildcarded. The kind stays a concrete, validated
// token — a wildcard kind would capture all seven comms kinds on one consumer,
// which delivery must not do — so this returns an error if kind is not a valid
// single subject token (see ValidSubjectToken).
//
// Subscribe-side only. Publish derives its subject from the ref via
// CommsSubject, and EventRef.valid rejects a "*" tenant, so no publish can
// ever target this subject.
func CommsWildcardSubject(kind EventKind) (string, error) {
	if err := ValidSubjectToken("event kind", string(kind)); err != nil {
		return "", err
	}
	return subjectPrefix + "." + wildcardToken + ".comms." + string(kind), nil
}

// validCommsSubject checks a whole comms subject against the frozen grammar:
// exactly compass.<tenant>.comms.<kind>, with both variable tokens valid.
//
// Subscribe takes a subject string, not a (tenant, kind) pair, so this is the
// only place the grammar can be enforced on that path — and a subject that is
// merely rooted at "compass" would otherwise reach CreateOrUpdateConsumer with
// a FilterSubject the COMPASS_COMMS stream does not capture, producing a
// consumer that silently never delivers. Publish gets the same guarantee for
// free by deriving the subject from the ref.
func validCommsSubject(subject string) error {
	tokens := strings.Split(subject, ".")
	if len(tokens) != commsSubjectTokens {
		return fmt.Errorf("fabric: subject %q has %d tokens, want %d (compass.<tenant>.comms.<kind>)",
			subject, len(tokens), commsSubjectTokens)
	}
	if tokens[0] != subjectPrefix {
		return fmt.Errorf("fabric: subject %q is not rooted at %q", subject, subjectPrefix)
	}
	if tokens[2] != commsToken {
		return fmt.Errorf("fabric: subject %q has %q where the grammar requires %q", subject, tokens[2], commsToken)
	}
	if err := ValidSubjectToken("tenant", tokens[1]); err != nil {
		return err
	}
	return ValidSubjectToken("event kind", tokens[3])
}

// The shape validCommsSubject enforces: compass.<tenant>.comms.<kind>, the same
// four tokens CommsSubject builds and commsStreamSubjects captures.
const (
	commsSubjectTokens = 4
	commsToken         = "comms"

	// wildcardToken is NATS's single-token wildcard, used by
	// CommsWildcardSubject for the tenant token only. Spelled once here so the
	// wildcard subject and commsStreamSubjects cannot drift apart.
	wildcardToken = "*"
)

// RunnerCommandSubject builds a Runner's command subject:
// compass.runner.<runner_id>.cmd. Every Server publishes async commands for that
// Runner here; the Runner core-NATS-subscribes to it from enrollment (§Q2 —
// Runners are subject-addressable, not connection-owned).
func RunnerCommandSubject(runnerID string) (string, error) {
	if err := ValidSubjectToken("runner id", runnerID); err != nil {
		return "", err
	}
	return subjectPrefix + ".runner." + runnerID + ".cmd", nil
}

// RunnerEventsSubject is the Runner→Server event fan-in subject:
// compass.runner.events. It takes no token and cannot fail. Subscribers join
// RunnerEventsQueue so exactly one Server instance claims each event.
func RunnerEventsSubject() string {
	return subjectPrefix + ".runner.events"
}

// ClientSubject builds the per-connection delivery subject for a live client:
// client.<sessionID>. Deliberately outside the "compass." root — the frozen
// grammar names it "client.<sessionID>" (§T3), and it is not a
// COMPASS_COMMS-stream subject. Unused in this task; the delivery edge lands on
// it later.
func ClientSubject(sessionID string) (string, error) {
	if err := ValidSubjectToken("session id", sessionID); err != nil {
		return "", err
	}
	return "client." + sessionID, nil
}

// ValidSubjectToken reports whether s is usable as a single NATS subject token,
// naming what was wrong. NATS reserves "." (token separator), "*" and ">"
// (wildcards), and rejects whitespace; an empty token would collapse two
// separators into an off-by-one subject.
//
// Tenant ids, runner ids and session ids are opaque to the fabric, so the
// grammar cannot encode-and-escape them without becoming ambiguous on the way
// back out. The choice is therefore to REJECT rather than sanitize: an id
// carrying a reserved character is a bug at the identifier's source, and turning
// it into a different-but-valid subject would route a tenant's events to the
// wrong subject rather than surfacing the bug.
func ValidSubjectToken(what, s string) error {
	if s == "" {
		return fmt.Errorf("fabric: %s is empty; a subject token cannot be empty", what)
	}
	if i := strings.IndexAny(s, ".*>"); i >= 0 {
		return fmt.Errorf("fabric: %s %q contains reserved NATS subject character %q at %d", what, s, s[i], i)
	}
	if i := strings.IndexFunc(s, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\r' || r == '\n'
	}); i >= 0 {
		return fmt.Errorf("fabric: %s %q contains whitespace at %d; a subject token cannot contain whitespace", what, s, i)
	}
	return nil
}
