//go:build unix

package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
)

func TestStreamDrainProcedureSet(t *testing.T) {
	for _, procedure := range []string{
		compassv1connect.CompassServiceSubscribeEventsProcedure,
		compassv1connect.CompassServiceSubscribeAgentSessionProcedure,
		compassv1connect.CommsServiceSubscribeCommsProcedure,
	} {
		if _, ok := serverStreamPaths[procedure]; !ok {
			t.Errorf("serverStreamPaths is missing %q", procedure)
		}
	}
	for _, procedure := range []string{
		compassv1connect.CompassServiceGetServerInfoProcedure,
		compassv1internalconnect.RunnerServiceSessionsProcedure,
		compassv1internalconnect.RunnerServicePublishEventsProcedure,
	} {
		if _, ok := serverStreamPaths[procedure]; ok {
			t.Errorf("serverStreamPaths unexpectedly contains %q", procedure)
		}
	}
}

func TestStreamDrainAbortsBlockedSend(t *testing.T) {
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	// One replay event so the handler sends and the client stream opens.
	bus.Publish(t.Context(), statusEvent())

	streams := newStreamDrain()
	server := connect.NewServer()
	compassv1connect.RegisterCompassServiceHandler(server, newService("test", bus, nil, nil, nil, nil, nil))
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	srv := httptest.NewUnstartedServer(streams.wrap(mux))
	srv.Config.Protocols = cleartextHTTP2()
	srv.Start()
	t.Cleanup(srv.Close)

	client := newH2CClient(t, srv.URL)
	stream := subscribe(t, client, &compassv1.SubscribeEventsRequest{SinceSeq: 0})
	recvOne(t, stream) // snapshot boundary; the client reads nothing more

	// 16 KiB incompressible events fill the ~4 MiB h2 window in ~256 sends, so
	// with the client not reading, the handler is left blocked in Send.
	const eventBytes = 16 * 1024
	const flood = 2048
	for range flood {
		bus.Publish(t.Context(), chunkEvent(eventBytes))
	}
	bus.Close()

	shutdownCtx, cancel := context.WithTimeout(t.Context(), drainTimeout)
	defer cancel()
	streams.abort(shutdownCtx)
	if err := srv.Config.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown with a blocked server stream = %v, want nil", err)
	}
}

func TestStreamDrainDoesNotCutUnaryCall(t *testing.T) {
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	server := connect.NewServer(unaryDrainGate(started, release))
	compassv1connect.RegisterCompassServiceHandler(server, newService("test", bus, nil, nil, nil, nil, nil))
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	streams := newStreamDrain()
	srv := httptest.NewUnstartedServer(streams.wrap(mux))
	srv.Config.Protocols = cleartextHTTP2()
	srv.Start()
	t.Cleanup(srv.Close)

	client := newH2CClient(t, srv.URL)
	type callResult struct {
		response *compassv1.GetServerInfoResponse
		err      error
	}
	result := make(chan callResult, 1)
	go func() {
		response, err := client.GetServerInfo(t.Context(), &compassv1.GetServerInfoRequest{})
		result <- callResult{response: response, err: err}
	}()
	select {
	case <-started:
	case <-timeAfter():
		t.Fatal("GetServerInfo handler did not start")
	}
	streams.abort(t.Context())
	close(release)
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("GetServerInfo after stream abort: %v", got.err)
		}
		if got.response == nil {
			t.Fatal("GetServerInfo returned a nil response")
		}
	case <-timeAfter():
		t.Fatal("GetServerInfo did not complete after stream abort")
	}
}

func unaryDrainGate(started chan<- struct{}, release <-chan struct{}) connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			if spec.Procedure == compassv1connect.CompassServiceGetServerInfoProcedure {
				select {
				case started <- struct{}{}:
				default:
				}
				<-release
			}
			return next(ctx, spec, stream)
		}
	}
}

func TestStreamDrainWakesIdleStreamOverHTTP1(t *testing.T) {
	// An idle stream has no write to time out; only the ctx cancel can end it.
	started := make(chan struct{})
	idle := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush idle stream headers: %v", err)
		}
		close(started)
		<-r.Context().Done()
	})
	streams := newStreamDrain()
	mux := http.NewServeMux()
	mux.Handle(compassv1connect.CompassServiceSubscribeAgentSessionProcedure, idle)
	srv := httptest.NewServer(streams.wrap(mux))
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+compassv1connect.CompassServiceSubscribeAgentSessionProcedure, http.NoBody)
	if err != nil {
		t.Fatalf("build idle stream request: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST idle stream: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.ProtoMajor != 1 {
		t.Fatalf("proto = HTTP/%d, want HTTP/1.1", resp.ProtoMajor)
	}
	select {
	case <-started:
	case <-timeAfter():
		t.Fatal("idle stream handler did not start")
	}

	shutdownCtx, cancel := context.WithTimeout(t.Context(), drainTimeout)
	defer cancel()
	streams.abort(shutdownCtx)
	if err := srv.Config.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown with an idle HTTP/1.1 stream = %v, want nil", err)
	}
}
