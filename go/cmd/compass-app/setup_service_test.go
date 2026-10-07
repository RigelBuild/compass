//go:build unix

package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/appconfig"
)

func TestSetupServiceChooseEmbeddedPreflightFailure(t *testing.T) {
	preflightErr := errors.New("preflight failed")
	saves := 0
	gate := &firstRunGate{}
	svc := newBridgeService(nil, nil, nil)
	setup := setupService{
		gate: gate,
		svc:  svc,
		preflight: func(context.Context) error {
			return preflightErr
		},
		save: func() error {
			saves++
			return nil
		},
	}

	result := setup.ChooseEmbedded(context.Background())
	if result.OK || result.Message != preflightErr.Error() {
		t.Fatalf("ChooseEmbedded() = %+v, want preflight error", result)
	}
	if saves != 0 {
		t.Errorf("save calls = %d, want 0 after failed preflight", saves)
	}
	if err := gate.begin(); err != nil {
		t.Fatalf("gate after preflight failure: %v, want open", err)
	}
	gate.end(false)
}

func TestSetupServiceChooseEmbeddedSuccess(t *testing.T) {
	events := &setupServiceEvents{}
	gate := &firstRunGate{}
	svc := newBridgeService(nil, events, nil)
	saves := 0
	setup := setupService{
		gate: gate,
		svc:  svc,
		preflight: func(context.Context) error {
			return nil
		},
		save: func() error {
			saves++
			return nil
		},
	}

	result := setup.ChooseEmbedded(context.Background())
	if !result.OK {
		t.Fatalf("ChooseEmbedded() = %+v, want success", result)
	}
	if result.Message != "Compass is set up to run on this computer. Quit and reopen it to start." {
		t.Errorf("success message = %q, want embedded reopen guidance", result.Message)
	}
	if saves != 1 {
		t.Errorf("save calls = %d, want 1", saves)
	}
	mode, serverURL := svc.shellState()
	if mode != "reopen" || serverURL != "" {
		t.Errorf("shellState() = (%q, %q), want (reopen, empty URL)", mode, serverURL)
	}
	if events.decidedCount() != 1 {
		t.Errorf("setup:decided events = %d, want 1", events.decidedCount())
	}
	if err := gate.begin(); err == nil {
		t.Error("gate after successful save is open, want final")
	} else {
		gate.end(false)
	}
}

func TestSetupServiceChooseEmbeddedSaveFailure(t *testing.T) {
	saveErr := errors.New("save failed")
	gate := &firstRunGate{}
	setup := setupService{
		gate: gate,
		svc:  newBridgeService(nil, nil, nil),
		preflight: func(context.Context) error {
			return nil
		},
		save: func() error {
			return saveErr
		},
	}

	result := setup.ChooseEmbedded(context.Background())
	if result.OK || result.Message != saveErr.Error() {
		t.Fatalf("ChooseEmbedded() = %+v, want save error", result)
	}
	if err := gate.begin(); err != nil {
		t.Fatalf("gate after save failure: %v, want open", err)
	}
	gate.end(false)
}

func TestSetupServiceChooseEmbeddedConfigExists(t *testing.T) {
	events := &setupServiceEvents{}
	gate := &firstRunGate{}
	setup := setupService{
		gate: gate,
		svc:  newBridgeService(nil, events, nil),
		preflight: func(context.Context) error {
			return nil
		},
		save: func() error {
			return appconfig.ErrConfigExists
		},
	}

	result := setup.ChooseEmbedded(context.Background())
	if result.OK || result.Message != setupDecidedMessage {
		t.Fatalf("ChooseEmbedded() = %+v, want neutral already-set-up result", result)
	}
	if strings.Contains(strings.ToLower(result.Message), "embedded") || strings.Contains(strings.ToLower(result.Message), "client") {
		t.Errorf("already-set-up message names a mode: %q", result.Message)
	}
	mode, serverURL := setup.svc.shellState()
	if mode != "reopen" || serverURL != "" {
		t.Errorf("shellState() = (%q, %q), want (reopen, empty URL)", mode, serverURL)
	}
	if events.decidedCount() != 1 {
		t.Errorf("setup:decided events = %d, want 1", events.decidedCount())
	}
	if err := gate.begin(); err == nil {
		t.Error("gate after ErrConfigExists is open, want final")
	} else {
		gate.end(false)
	}
}

func TestSetupServiceChooseEmbeddedGateRefusals(t *testing.T) {
	t.Run("busy gate refuses before preflight", func(t *testing.T) {
		gate := &firstRunGate{}
		if err := gate.begin(); err != nil {
			t.Fatalf("begin in-flight connect: %v", err)
		}
		preflights := 0
		setup := setupService{
			gate: gate,
			svc:  newBridgeService(nil, nil, nil),
			preflight: func(context.Context) error {
				preflights++
				return nil
			},
			save: func() error { return nil },
		}

		result := setup.ChooseEmbedded(context.Background())
		if result.OK || result.Message != setupBusyMessage {
			t.Fatalf("ChooseEmbedded() = %+v, want busy refusal", result)
		}
		if preflights != 0 {
			t.Errorf("preflight calls = %d, want 0 while gate is busy", preflights)
		}
		gate.end(false)
	})

	t.Run("decided gate refuses after save", func(t *testing.T) {
		gate := &firstRunGate{}
		setup := setupService{
			gate: gate,
			svc:  newBridgeService(nil, nil, nil),
			preflight: func(context.Context) error {
				return nil
			},
			save: func() error { return nil },
		}
		if result := setup.ChooseEmbedded(context.Background()); !result.OK {
			t.Fatalf("first ChooseEmbedded() = %+v, want success", result)
		}
		preflights := 0
		setup.preflight = func(context.Context) error {
			preflights++
			return nil
		}

		result := setup.ChooseEmbedded(context.Background())
		if result.OK || result.Message != setupDecidedMessage {
			t.Fatalf("second ChooseEmbedded() = %+v, want final refusal", result)
		}
		if preflights != 0 {
			t.Errorf("preflight calls after save = %d, want 0", preflights)
		}
	})
}

type setupServiceEvents struct {
	names []string
}

func (e *setupServiceEvents) Emit(name string, _ ...any) bool {
	e.names = append(e.names, name)
	return true
}

func (e *setupServiceEvents) decidedCount() int {
	count := 0
	for _, name := range e.names {
		if name == "setup:decided" {
			count++
		}
	}
	return count
}
