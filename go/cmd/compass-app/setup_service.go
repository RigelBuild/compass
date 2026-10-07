//go:build unix

package main

import (
	"context"
	"errors"

	"github.com/RigelBuild/compass/go/internal/appconfig"
)

type setupResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

type setupService struct {
	gate      *firstRunGate
	svc       *bridgeService
	preflight func(context.Context) error
	save      func() error
}

func (s *setupService) ChooseEmbedded(ctx context.Context) setupResult {
	if err := s.gate.begin(); err != nil {
		return setupResult{Message: err.Error()}
	}

	finished := false
	defer func() {
		if !finished {
			s.gate.end(false)
		}
	}()

	if err := s.preflight(ctx); err != nil {
		return setupResult{Message: err.Error()}
	}
	if err := s.save(); err != nil {
		if errors.Is(err, appconfig.ErrConfigExists) {
			decide(s.gate, s.svc, false)
			finished = true
			return setupResult{Message: setupDecidedMessage}
		}
		return setupResult{Message: err.Error()}
	}

	decide(s.gate, s.svc, false)
	finished = true
	return setupResult{
		OK:      true,
		Message: "Compass is set up to run on this computer. Quit and reopen it to start.",
	}
}
