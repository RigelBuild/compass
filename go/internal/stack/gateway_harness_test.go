//go:build unix

package stack

import (
	"context"
	"errors"
	"sync"
)

type fakeGatewayContainer struct {
	mu       sync.Mutex
	rec      *recorder
	lastSpec GatewayContainerSpec
	startErr error
}

func newFakeGatewayContainer(rec *recorder) *fakeGatewayContainer {
	return &fakeGatewayContainer{rec: rec}
}
func (c *fakeGatewayContainer) Start(_ context.Context, spec GatewayContainerSpec) (Process, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastSpec = spec
	c.rec.add("start llm-gateway")
	if c.startErr != nil {
		return nil, c.startErr
	}
	return &stubGatewayProcess{rec: c.rec}, nil
}
func (c *fakeGatewayContainer) spec() GatewayContainerSpec {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastSpec
}

type stubGatewayProcess struct{ rec *recorder }

func (p *stubGatewayProcess) Signal(_ context.Context, sig ProcessSignal) error {
	p.rec.add("signal llm-gateway")
	return nil
}
func (p *stubGatewayProcess) Wait(context.Context) error { p.rec.add("wait llm-gateway"); return nil }
func (*stubGatewayProcess) Pid() int                     { return 0 }

type stubGatewayProber struct {
	mu       sync.Mutex
	rec      *recorder
	endpoint string
	never    bool
}

func (p *stubGatewayProber) ProbeGateway(_ context.Context, endpoint string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rec.add("probe-gateway")
	p.endpoint = endpoint
	if p.never {
		return errGatewayNotReady
	}
	return nil
}
func (p *stubGatewayProber) lastEndpoint() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.endpoint
}

var errGatewayNotReady = errors.New("gateway not answering")
