package fabric

import (
	"strings"
	"testing"
)

// TestSubjectBuilders defends the frozen grammar itself: every subject a
// consumer will ever publish or subscribe to comes from these four builders, so
// a drift in any one of them silently re-wires a plane. The exact strings are
// asserted, not a pattern — the grammar is the contract other tasks build
// against (SUBJECTS.md).
func TestSubjectBuilders(t *testing.T) {
	t.Parallel()

	t.Run("comms", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name   string
			tenant string
			kind   EventKind
			want   string
		}{
			{"message posted", "t1", KindMessagePosted, "compass.t1.comms.message_posted"},
			{"account changed", "acme", KindAccountChanged, "compass.acme.comms.account_changed"},
			{"uuid tenant", "0d8f1a2b-3c4d", KindTopicUpserted, "compass.0d8f1a2b-3c4d.comms.topic_upserted"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				got, err := CommsSubject(tc.tenant, tc.kind)
				if err != nil {
					t.Fatalf("CommsSubject(%q, %q): %v", tc.tenant, tc.kind, err)
				}
				if got != tc.want {
					t.Fatalf("CommsSubject(%q, %q) = %q, want %q", tc.tenant, tc.kind, got, tc.want)
				}
			})
		}
	})

	t.Run("runner command", func(t *testing.T) {
		t.Parallel()
		got, err := RunnerCommandSubject("runner-7")
		if err != nil {
			t.Fatalf("RunnerCommandSubject: %v", err)
		}
		if want := "compass.runner.runner-7.cmd"; got != want {
			t.Fatalf("RunnerCommandSubject = %q, want %q", got, want)
		}
	})

	t.Run("runner events", func(t *testing.T) {
		t.Parallel()
		if got, want := RunnerEventsSubject(), "compass.runner.events"; got != want {
			t.Fatalf("RunnerEventsSubject = %q, want %q", got, want)
		}
		if RunnerEventsQueue == "" {
			t.Fatal("RunnerEventsQueue must be set, or every Server handles every Runner event")
		}
	})

	t.Run("client", func(t *testing.T) {
		t.Parallel()
		got, err := ClientSubject("sess-42")
		if err != nil {
			t.Fatalf("ClientSubject: %v", err)
		}
		if want := "client.sess-42"; got != want {
			t.Fatalf("ClientSubject = %q, want %q", got, want)
		}
	})
}

// TestClientSubjectIsOutsideTheCommsStream defends a real cross-plane hazard:
// the COMPASS_COMMS stream captures compass.*.comms.*, and a client subject that
// happened to land inside that wildcard would push every per-connection
// delivery into a durable stream — a second store of state Postgres owns.
func TestClientSubjectIsOutsideTheCommsStream(t *testing.T) {
	t.Parallel()
	got, err := ClientSubject("sess-42")
	if err != nil {
		t.Fatalf("ClientSubject: %v", err)
	}
	if strings.HasPrefix(got, subjectPrefix+".") {
		t.Fatalf("ClientSubject = %q: must not sit under the %q root captured by the comms stream", got, subjectPrefix)
	}
}

// TestSubjectBuildersRejectInvalidTokens defends the reject-never-sanitize
// choice. Each of these tokens would, if silently accepted, produce a
// well-formed but WRONG subject: a "." adds a token (routing a tenant's events
// somewhere nobody is subscribed), and a "*" or ">" turns a publish target into
// a wildcard. The builder must refuse.
func TestSubjectBuildersRejectInvalidTokens(t *testing.T) {
	t.Parallel()
	bad := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"dot", "ten.ant"},
		{"star", "ten*ant"},
		{"gt", "ten>ant"},
		{"space", "ten ant"},
		{"tab", "ten\tant"},
		{"newline", "ten\nant"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if s, err := CommsSubject(tc.token, KindMessagePosted); err == nil {
				t.Errorf("CommsSubject(%q, …) = %q, want an error", tc.token, s)
			}
			if s, err := RunnerCommandSubject(tc.token); err == nil {
				t.Errorf("RunnerCommandSubject(%q) = %q, want an error", tc.token, s)
			}
			if s, err := ClientSubject(tc.token); err == nil {
				t.Errorf("ClientSubject(%q) = %q, want an error", tc.token, s)
			}
		})
	}
}

// TestCommsSubjectRejectsInvalidKind defends the kind token too — it is as much
// a subject token as the tenant is, and an unvalidated kind is how a
// caller-supplied string escapes into the grammar.
func TestCommsSubjectRejectsInvalidKind(t *testing.T) {
	t.Parallel()
	for _, kind := range []EventKind{"", "message.posted", "message>posted", "message posted"} {
		if s, err := CommsSubject("t1", kind); err == nil {
			t.Errorf("CommsSubject(t1, %q) = %q, want an error", kind, s)
		}
	}
}

// TestEventKindsAreValidSubjectTokens defends the closed set of kinds against
// the grammar: a kind constant is used verbatim as a subject token, so one
// introduced with a "." or an uppercase-with-space spelling would break every
// publish for that kind at runtime, not at compile time.
func TestEventKindsAreValidSubjectTokens(t *testing.T) {
	t.Parallel()
	kinds := []EventKind{
		KindAccountChanged, KindChannelGroupChanged, KindChannelChanged,
		KindAgentWorkspaceChanged, KindMessagePosted, KindMessageUpdated,
		KindTopicUpserted,
	}
	if len(kinds) != 7 {
		t.Fatalf("expected the 7 frozen comms kinds, listed %d", len(kinds))
	}
	seen := make(map[EventKind]bool, len(kinds))
	for _, k := range kinds {
		if err := ValidSubjectToken("event kind", string(k)); err != nil {
			t.Errorf("kind %q is not a usable subject token: %v", k, err)
		}
		if seen[k] {
			t.Errorf("kind %q is duplicated; two event classes would share a subject", k)
		}
		seen[k] = true
	}
}

// TestDLQSubjectIsOutsideTheCommsStream defends against the pathological loop:
// if the dead-letter subject were captured by the comms stream, parking a poison
// message would store it back in the stream it came from.
func TestDLQSubjectIsOutsideTheCommsStream(t *testing.T) {
	t.Parallel()
	// The stream wildcard is compass.*.comms.* — four tokens with "comms"
	// third. The DLQ subject is three tokens, so it cannot match; assert the
	// shape that guarantees it.
	if got, want := strings.Count(DLQSubject, "."), 2; got != want {
		t.Fatalf("DLQSubject = %q has %d separators, want %d (a 4-token dlq subject could be captured by %q)",
			DLQSubject, got, want, commsStreamSubjects)
	}
}

// TestValidCommsSubjectRejectsMalformed defends the whole-subject guard that
// replaced the head-only one on the Subscribe path. Every case below is rooted
// at "compass", so a first-token check passed all of them — and a subject the
// COMPASS_COMMS stream's compass.*.comms.* wildcard does not capture produces a
// consumer that is created successfully and then silently never delivers, which
// is the worst failure shape available: no error anywhere.
func TestValidCommsSubjectRejectsMalformed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		subject string
	}{
		{"wildcard tokens", "compass.*.comms.*"},
		{"empty tenant", "compass..comms.x"},
		{"trailing token", "compass.t1.comms.message_posted.extra"},
		{"too few tokens", "compass.t1.comms"},
		{"wrong root", "client.t1.comms.message_posted"},
		{"wrong plane token", "compass.t1.runner.message_posted"},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := validCommsSubject(tc.subject); err == nil {
				t.Errorf("validCommsSubject(%q) = nil, want an error", tc.subject)
			}
		})
	}

	// The positive gate: the grammar the builder itself produces must pass, or
	// the guard would reject every legitimate Subscribe.
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		if err := validCommsSubject("compass.t1.comms.message_posted"); err != nil {
			t.Errorf("validCommsSubject on a well-formed subject: %v", err)
		}
		built, err := CommsSubject("t1", KindMessagePosted)
		if err != nil {
			t.Fatalf("CommsSubject: %v", err)
		}
		if err := validCommsSubject(built); err != nil {
			t.Errorf("validCommsSubject rejected the subject CommsSubject built (%q): %v", built, err)
		}
	})
}
