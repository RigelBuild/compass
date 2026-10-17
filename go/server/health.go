//go:build unix

package server

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/grpchealth/v2"

	"github.com/RigelBuild/compass/go/internal/runnerhub"
)

const healthzPattern = "GET /healthz"
const healthMaxReadBytes = 1 << 10

//nolint:gosec // This is the public health service name, not a credential.
const compassHealthService = "compass.v1.CompassService"

const runnerHealthService = "compass.runner"

type serverHealth struct {
	draining atomic.Bool
	grpc     *connect.Server
	hub      *runnerhub.Hub
}

func newServerHealth(hub *runnerhub.Hub) *serverHealth {
	health := &serverHealth{hub: hub, grpc: connect.NewServer()}
	grpchealth.Register(health.grpc, health)
	return health
}

func (h *serverHealth) Check(_ context.Context, req *grpchealth.CheckRequest) (*grpchealth.CheckResponse, error) {
	if req.Service == runnerHealthService {
		if h.draining.Load() || h.hub == nil {
			return &grpchealth.CheckResponse{Status: grpchealth.StatusNotServing}, nil
		}
		if h.hub.RunnerEnrolled() {
			return &grpchealth.CheckResponse{Status: grpchealth.StatusServing}, nil
		}
		return &grpchealth.CheckResponse{Status: grpchealth.StatusNotServing}, nil
	}
	if req.Service != "" && req.Service != compassHealthService {
		return nil, connect.Errorf(connect.CodeNotFound, "unknown health service %q", req.Service)
	}
	if h.draining.Load() {
		return &grpchealth.CheckResponse{Status: grpchealth.StatusNotServing}, nil
	}
	return &grpchealth.CheckResponse{Status: grpchealth.StatusServing}, nil
}

func (h *serverHealth) markDraining() {
	h.draining.Store(true)
}

func (h *serverHealth) serveHealthz(w http.ResponseWriter, r *http.Request) {
	response, err := h.Check(r.Context(), &grpchealth.CheckRequest{Service: r.URL.Query().Get("service")})
	if err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	if response.Status == grpchealth.StatusNotServing {
		w.WriteHeader(http.StatusServiceUnavailable)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	// The status is already sent; a failed body write leaves the prober nothing to act on.
	_, _ = fmt.Fprint(w, response.Status.String())
}

func (h *serverHealth) mount(mux *http.ServeMux) {
	connecthttp.Mount(mux, h.grpc, connecthttp.WithReadMaxBytes(healthMaxReadBytes))
	mux.HandleFunc(healthzPattern, h.serveHealthz)
}
