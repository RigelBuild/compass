//go:build unix

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/RigelBuild/compass/go/internal/appconfig"
)

const maxCAFileBytes = 1 << 20

func readCAFile(path string) (data []byte, retErr error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: selected by the user in the native CA dialog
	if err != nil {
		return nil, fmt.Errorf("opening CA certificate %q: %w", path, err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("closing CA certificate %q: %w", path, err))
		}
	}()

	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("checking CA certificate %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("CA certificate %q is not a regular file", path)
	}

	data, err = io.ReadAll(io.LimitReader(file, maxCAFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading CA certificate %q: %w", path, err)
	}
	if len(data) > maxCAFileBytes {
		return nil, fmt.Errorf("CA certificate %q is larger than 1 MiB", path)
	}
	return data, nil
}

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
