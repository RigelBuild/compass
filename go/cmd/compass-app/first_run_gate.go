//go:build unix

package main

import (
	"crypto/rand"
	"encoding/hex"
	"sync"

	"github.com/RigelBuild/compass/go/internal/appconfig"
	"github.com/RigelBuild/compass/go/internal/bridge"
)

const (
	setupBusyMessage    = "Another window is setting up Compass."
	setupDecidedMessage = "Compass is already set up. Quit and reopen it to change this."
)

var (
	errSetupBusy    error = setupGateError(setupBusyMessage)
	errSetupDecided error = setupGateError(setupDecidedMessage)
)

type setupGateError string

func (e setupGateError) Error() string {
	return string(e)
}

type firstRunGate struct {
	mu      sync.Mutex
	busy    bool
	decided bool
}

func (g *firstRunGate) begin() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.decided {
		return errSetupDecided
	}
	if g.busy {
		return errSetupBusy
	}
	g.busy = true
	return nil
}

func (g *firstRunGate) end(saved bool) {
	g.mu.Lock()
	g.busy = false
	if saved {
		g.decided = true
	}
	g.mu.Unlock()
}

func decide(g *firstRunGate, svc *bridgeService, clientInstalled bool) {
	g.end(true)
	if !clientInstalled {
		svc.setPhase("reopen")
	}
	if svc.events != nil {
		svc.events.Emit("setup:decided")
	}
}

type caPicks struct {
	mu    sync.Mutex
	byRef map[string][]byte
}

func (p *caPicks) add(pem []byte) string {
	for {
		var raw [16]byte
		rand.Read(raw[:])
		ref := hex.EncodeToString(raw[:])
		p.mu.Lock()
		if _, exists := p.byRef[ref]; exists {
			p.mu.Unlock()
			continue
		}
		if p.byRef == nil {
			p.byRef = make(map[string][]byte)
		}
		p.byRef[ref] = append([]byte(nil), pem...)
		p.mu.Unlock()
		return ref
	}
}

func (p *caPicks) get(ref string) ([]byte, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pem, ok := p.byRef[ref]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), pem...), true
}

func (p *caPicks) clear() {
	p.mu.Lock()
	clear(p.byRef)
	p.mu.Unlock()
}

type setupWiring struct {
	configPath string
	gate       *firstRunGate
	picks      *caPicks
	newTarget  func(serverURL string, caPEM []byte) (*bridge.Target, error)
	saveClient func(path string, cfg appconfig.Config, caPEM []byte) (appconfig.Config, error)
}
