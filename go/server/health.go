//go:build unix

package server

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"

	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"
)

const healthzPattern = "GET /healthz"
const healthMaxReadBytes = 1 << 10

//nolint:gosec // This is the public health service name, not a credential.
const compassHealthService = "compass.v1.CompassService"

type serverHealth struct {
	draining atomic.Bool
	grpcPath string
	grpc     http.Handler
}

func newServerHealth() *serverHealth {
	health := new(serverHealth)
	health.grpcPath, health.grpc = grpchealth.NewHandler(health,
		connect.WithReadMaxBytes(healthMaxReadBytes),
	)
	return health
}

func (h *serverHealth) Check(_ context.Context, req *grpchealth.CheckRequest) (*grpchealth.CheckResponse, error) {
	if req.Service != "" && req.Service != compassHealthService {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown health service %q", req.Service))
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
	mux.Handle(h.grpcPath, h.grpc)
	mux.HandleFunc(healthzPattern, h.serveHealthz)
}
