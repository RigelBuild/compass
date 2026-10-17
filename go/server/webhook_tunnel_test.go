//go:build unix

// The webhook relay client the live tunnel leg subscribes through, plus its
// hermetic tests against a stub relay that speaks the gosmee SSE wire shape.
package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// tunnelMaxEvent bounds one SSE line; a relayed body arrives base64 on one line.
const tunnelMaxEvent = 32 << 20

// tunnelDelivery is one webhook POST as the relay forwarded it.
type tunnelDelivery struct {
	header http.Header
	body   []byte
}

// tunnelSub is a live subscription to one relay channel.
type tunnelSub struct {
	relay      string
	body       io.Closer
	deliveries chan tunnelDelivery
	done       chan struct{}
	stop       chan struct{} // closed by close; unblocks a pending delivery send
	err        error         // set before done closes
	closeOnce  sync.Once
}

// tunnelSubscribe opens the channel's event stream and returns once the relay
// reports ready, so a webhook triggered after it returns cannot be missed. Every
// error names the relay: a relay outage must read as one, never as a forge bug.
func tunnelSubscribe(ctx context.Context, relay, channel string) (*tunnelSub, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, relay+"/events/"+channel, nil)
	if err != nil {
		return nil, fmt.Errorf("webhook relay %s: build subscribe request: %w", relay, err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// The *url.Error text carries the channel, a read capability: keep only its cause.
		if urlErr, ok := errors.AsType[*url.Error](err); ok {
			err = urlErr.Err
		}
		return nil, fmt.Errorf("webhook relay %s unreachable: %w", relay, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("webhook relay %s: subscribe returned HTTP %d", relay, resp.StatusCode)
	}
	sub := &tunnelSub{
		relay:      relay,
		body:       resp.Body,
		deliveries: make(chan tunnelDelivery, 16),
		done:       make(chan struct{}),
		stop:       make(chan struct{}),
	}
	ready := make(chan struct{})
	go sub.read(resp.Body, ready)
	select {
	case <-ready:
		return sub, nil
	case <-sub.done:
		return nil, sub.err
	case <-ctx.Done():
		sub.close()
		return nil, fmt.Errorf("webhook relay %s: not ready before deadline: %w", relay, ctx.Err())
	}
}

func (s *tunnelSub) close() {
	s.closeOnce.Do(func() {
		close(s.stop)
		_ = s.body.Close()
	})
}

// read parses the SSE stream until it ends, closing ready on the relay's
// "ready" message and forwarding every webhook frame.
func (s *tunnelSub) read(stream io.Reader, ready chan struct{}) {
	readyOnce := sync.Once{}
	sc := bufio.NewScanner(stream)
	sc.Buffer(make([]byte, 0, 64<<10), tunnelMaxEvent)
	var data bytes.Buffer
	for sc.Scan() {
		line := sc.Text()
		if line != "" {
			if v, ok := strings.CutPrefix(line, "data:"); ok {
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(strings.TrimPrefix(v, " "))
			}
			continue
		}
		frame := data.Bytes()
		data.Reset()
		if len(frame) == 0 {
			continue
		}
		d, isReady, err := parseTunnelFrame(frame)
		switch {
		case err != nil:
			s.err = fmt.Errorf("webhook relay %s: %w", s.relay, err)
			close(s.done)
			return
		case isReady:
			readyOnce.Do(func() { close(ready) })
		case d != nil:
			select {
			case s.deliveries <- *d:
			case <-s.stop:
				s.err = fmt.Errorf("webhook relay %s: subscription closed", s.relay)
				close(s.done)
				return
			}
		}
	}
	if err := sc.Err(); err != nil {
		s.err = fmt.Errorf("webhook relay %s: event stream broke: %w", s.relay, err)
	} else {
		s.err = fmt.Errorf("webhook relay %s closed the event stream", s.relay)
	}
	close(s.done)
}

// parseTunnelFrame decodes one gosmee frame: a {"message":...} status frame, or
// a webhook frame of lower-cased headers plus base64 `bodyB`.
func parseTunnelFrame(frame []byte) (*tunnelDelivery, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(frame, &fields); err != nil {
		return nil, false, fmt.Errorf("undecodable event frame: %w", err)
	}
	rawBody, isWebhook := fields["bodyB"]
	if !isWebhook {
		var msg struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(frame, &msg)
		return nil, msg.Message == "ready", nil
	}
	var b64 string
	if err := json.Unmarshal(rawBody, &b64); err != nil {
		return nil, false, fmt.Errorf("bodyB is not a string: %w", err)
	}
	body, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, false, fmt.Errorf("bodyB is not base64: %w", err)
	}
	header := http.Header{}
	for k, raw := range fields {
		if k == "bodyB" || k == "timestamp" {
			continue
		}
		var v string
		if json.Unmarshal(raw, &v) == nil {
			header.Set(k, v)
		}
	}
	return &tunnelDelivery{header: header, body: body}, false, nil
}

// next returns the first delivery match accepts, skipping others (a shared
// testbed fires deliveries for other runs too).
func (s *tunnelSub) next(ctx context.Context, match func(tunnelDelivery) bool) (tunnelDelivery, error) {
	for {
		select {
		case d := <-s.deliveries:
			if match(d) {
				return d, nil
			}
		case <-s.done:
			return tunnelDelivery{}, s.err
		case <-ctx.Done():
			return tunnelDelivery{}, fmt.Errorf("webhook relay %s: no matching delivery before deadline: %w", s.relay, ctx.Err())
		}
	}
}

// replayTunnel feeds a relayed delivery, raw body and headers, to h.
func replayTunnel(t *testing.T, h http.Handler, path string, d tunnelDelivery) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, bytes.NewReader(d.body))
	maps.Copy(req.Header, d.header)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// stubRelay serves one channel in the gosmee SSE shape; publish forwards a POST
// the way gosmee does (lower-cased headers, base64 body).
type stubRelay struct {
	srv    *httptest.Server
	frames chan []byte
}

func newStubRelay(t *testing.T) *stubRelay {
	t.Helper()
	r := &stubRelay{frames: make(chan []byte, 8)}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		write := func(frame []byte) {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", frame)
			_ = http.NewResponseController(w).Flush()
		}
		write([]byte(`{"message":"connected"}`))
		_, _ = fmt.Fprint(w, ": keepalive\n\n")
		write([]byte(`{"message":"ready"}`))
		for {
			select {
			case frame, ok := <-r.frames:
				if !ok {
					return // closing the channel ends the stream
				}
				write(frame)
			case <-req.Context().Done():
				return
			}
		}
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *stubRelay) publish(t *testing.T, header http.Header, body []byte) {
	t.Helper()
	payload := map[string]string{
		"timestamp": "1792230860565",
		"bodyB":     base64.StdEncoding.EncodeToString(body),
	}
	for k := range header {
		payload[strings.ToLower(k)] = header.Get(k)
	}
	frame, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	r.frames <- frame
}

func tunnelCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func subscribeStub(t *testing.T, r *stubRelay) *tunnelSub {
	t.Helper()
	sub, err := tunnelSubscribe(tunnelCtx(t), r.srv.URL, "chan")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(sub.close)
	return sub
}

func TestTunnelRelay_GitHubDeliveryVerifiesThroughHandler(t *testing.T) {
	secret := []byte("testbed-secret")
	relay := newStubRelay(t)
	sub := subscribeStub(t, relay)

	// Whitespace and 1.0 survive only a byte-exact relay; a re-serialized body
	// changes them and fails the HMAC.
	body := []byte(`{"action":"opened", "issue":{"number":7,"html_url":"u"},"repository":{"full_name":"o/r"},"x":1.0}`)
	other := []byte(`{"action":"opened","issue":{"number":8},"repository":{"full_name":"o/r"}}`)
	hdr := func(delivery string, b []byte) http.Header {
		h := http.Header{}
		h.Set(githubEventHeader, "issues")
		h.Set(githubDeliveryHeader, delivery)
		h.Set(githubSignatureHeader, ghSign(secret, b))
		return h
	}
	relay.publish(t, hdr("d-other", other), other)
	relay.publish(t, hdr("d-7", body), body)

	d, err := sub.next(tunnelCtx(t), func(d tunnelDelivery) bool {
		return d.header.Get(githubDeliveryHeader) == "d-7"
	})
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if !bytes.Equal(d.body, body) {
		t.Fatalf("relayed body = %q, want byte-exact %q", d.body, body)
	}
	h, sink := newTestHandler(t, secret)
	if rec := replayTunnel(t, h, githubWebhookPath, d); rec.Code != http.StatusOK {
		t.Fatalf("handler code = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if sink.count() != 1 || sink.events[0].Number != 7 || sink.events[0].DeliveryID != "d-7" {
		t.Fatalf("sink = %+v, want one event for #7 delivery d-7", sink.events)
	}
}

func TestTunnelRelay_LinearDeliveryVerifiesThroughHandler(t *testing.T) {
	secret := []byte("testbed-secret")
	relay := newStubRelay(t)
	sub := subscribeStub(t, relay)

	body := fmt.Appendf(nil, `{"type":"Issue","action":"create","webhookTimestamp":%d,"data":{"number":42,"team":{"key":"TEST"},"url":"u"}}`, time.Now().UnixMilli())
	h := http.Header{}
	h.Set(linearSignatureHeader, linSign(secret, body))
	relay.publish(t, h, body)

	d, err := sub.next(tunnelCtx(t), func(tunnelDelivery) bool { return true })
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	data := &recordingSink{}
	_, lh := NewLinearWebhookHandler(func(context.Context) ([]byte, error) { return secret, nil }, data, nil, nil)
	if rec := replayTunnel(t, lh, linearWebhookPath, d); rec.Code != http.StatusOK {
		t.Fatalf("handler code = %d, want 200", rec.Code)
	}
	if data.count() != 1 || data.events[0].Number != 42 {
		t.Fatalf("data sink = %+v, want one event for TEST-42", data.events)
	}
}

func TestTunnelRelay_DownNamesRelay(t *testing.T) {
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(bad.Close)

	for name, relay := range map[string]string{"unreachable": gone.URL, "5xx": bad.URL} {
		t.Run(name, func(t *testing.T) {
			_, err := tunnelSubscribe(tunnelCtx(t), relay, "chan")
			if err == nil || !strings.Contains(err.Error(), "webhook relay "+relay) {
				t.Fatalf("err = %v, want an error naming relay %s", err, relay)
			}
		})
	}
}

func TestTunnelRelay_UnreachableHidesChannel(t *testing.T) {
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	_, err := tunnelSubscribe(tunnelCtx(t), gone.URL, "secret-channel-name")
	if err == nil || strings.Contains(err.Error(), "secret-channel-name") {
		t.Fatalf("err = %v, want an error without the channel name", err)
	}
}

// A reader blocked on a full delivery queue must exit when the test closes it.
func TestTunnelRelay_CloseUnblocksSaturatedReader(t *testing.T) {
	relay := newStubRelay(t)
	sub := subscribeStub(t, relay)
	for range cap(sub.deliveries) + 1 {
		relay.publish(t, http.Header{}, []byte(`{}`))
	}
	for len(sub.deliveries) < cap(sub.deliveries) {
		runtime.Gosched()
	}
	sub.close()
	select {
	case <-sub.done:
	case <-tunnelCtx(t).Done():
		t.Fatal("reader goroutine still running after close")
	}
}

func TestTunnelRelay_StreamClosedBeforeDelivery(t *testing.T) {
	relay := newStubRelay(t)
	sub := subscribeStub(t, relay)
	close(relay.frames)

	_, err := sub.next(tunnelCtx(t), func(tunnelDelivery) bool { return true })
	if err == nil || !strings.Contains(err.Error(), "webhook relay "+relay.srv.URL+" closed the event stream") {
		t.Fatalf("err = %v, want the relay-closed error", err)
	}
}

func TestTunnelRelay_NoDeliveryBeforeDeadline(t *testing.T) {
	relay := newStubRelay(t)
	sub := subscribeStub(t, relay)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := sub.next(ctx, func(tunnelDelivery) bool { return true })
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "webhook relay "+relay.srv.URL) {
		t.Fatalf("err = %v, want a deadline error naming the relay", err)
	}
}
