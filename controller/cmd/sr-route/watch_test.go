package main

// GET /watch is the seam sr-fib reads, so these tests read it the way sr-fib
// does: a live HTTP GET whose body is parsed as Server-Sent Events. The view is
// changed through the real endpoints an operator's edit goes through (PUT
// /tunnels) and through a real RR session, not through a fake client.
//
// What is under test here is the wire -- the frame shape, the busy-slot answer,
// and the response ending when the process behind it does. The hub's diffing
// (which change is an event and which is invisible) is internal/route's
// watch_test.go, and duplicating it at this level would only test the encoder.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sdwan/controller/internal/route"
)

// watchFrameBudget is how long one frame may take to arrive before the test
// gives up. Generous on purpose: a frame only comes after the 10 ms coalesce
// window, and nothing here measures that window.
const watchFrameBudget = 10 * time.Second

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// pipeRR is a one-session reflector over net.Pipe: it handshakes, then lets the
// test push frames at an exact moment. The client only ever sees the wire, so
// this is the smallest thing that can put a real path in a real Loc-RIB -- and
// the view a watcher sees is rendered out of that table, not out of a stub.
type pipeRR struct {
	push chan route.Envelope
}

func newPipeRR() *pipeRR { return &pipeRR{push: make(chan route.Envelope, 8)} }

func (p *pipeRR) dial(ctx context.Context, addr string) (net.Conn, error) {
	srv, cli := net.Pipe()
	go func() {
		defer srv.Close()
		br := bufio.NewReader(srv)
		if _, err := route.ReadEnvelope(br); err != nil { // hello
			return
		}
		ok := route.Envelope{Type: route.MsgHelloOK, Version: route.ProtocolVersion, RRID: "pipe", HoldSec: 30}
		if err := route.WriteEnvelope(srv, ok); err != nil {
			return
		}
		// Drain what the client sends (its own announces and keepalives) so its
		// writes never block on an unread pipe.
		go func() {
			for {
				if _, err := route.ReadEnvelope(br); err != nil {
					return
				}
			}
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case env := <-p.push:
				if err := route.WriteEnvelope(srv, env); err != nil {
					return
				}
			}
		}
	}()
	return cli, nil
}

// announce hands the client a snapshot: routes then EOR. The client serves from
// a session only after its EOR, so a script that just sent routes would leave
// the table empty.
func (p *pipeRR) announce(routes ...route.Route) {
	p.push <- route.Envelope{Type: route.MsgAnnounce, Routes: routes}
	p.push <- route.Envelope{Type: route.MsgEOR}
}

// watchRoute is one prefix from device 20, whose tunnel the client config below
// has -- so the path is installable and therefore in the observable view.
func watchRoute() route.Route {
	return route.Route{DeviceID: 20, VRFID: 5, Prefix: "10.20.0.0/16", RTs: []string{"65000:6"}}
}

// watchClient returns a running client wired to rr plus the stop that shuts it
// down. The other sr-route tests do not call Run; a watcher has to, because the
// point of the endpoint is what happens while the process is alive.
func watchClient(t *testing.T, rr *pipeRR) (*route.Client, func()) {
	t.Helper()
	cfg := route.ClientConfig{
		Tenant:   "tenant-a",
		DeviceID: 10,
		VRFs: []route.VRFConfig{{
			ID:        5,
			ExportRTs: []string{"65000:5"},
			ImportRTs: []string{"65000:6"},
		}},
		Tunnels: []route.Tunnel{{Peer: 20, NextHop: "192.168.99.2"}},
		RRs:     []route.RRConfig{{ID: "rr-a", Addr: "rr-a"}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cfg.Dial = rr.dial
	c, err := route.NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("client did not stop")
		}
	})
	return c, cancel
}

// watchReader is one open GET /watch.
type watchReader struct {
	br     *bufio.Reader
	cancel context.CancelFunc
}

func (w *watchReader) close() { w.cancel() }

// openWatch starts a stream and fails unless the answer is a 200 event stream.
func openWatch(t *testing.T, srv *httptest.Server) *watchReader {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/watch", nil)
	if err != nil {
		cancel()
		t.Fatalf("request: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		cancel()
		t.Fatalf("GET /watch: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		t.Fatalf("GET /watch = %d: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	t.Cleanup(func() {
		cancel()
		resp.Body.Close()
	})
	return &watchReader{br: bufio.NewReader(resp.Body), cancel: cancel}
}

// openWatchUntil retries a 409 for as long as it takes the previous reader's
// handler to run its defer. The slot is released by that goroutine, not by the
// client hanging up, so a retry bounded by a real deadline is the honest way to
// ask for it.
func openWatchUntil(t *testing.T, srv *httptest.Server) *watchReader {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ctx, cancel := context.WithCancel(context.Background())
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/watch", nil)
		if err != nil {
			cancel()
			t.Fatalf("request: %v", err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			cancel()
			t.Fatalf("GET /watch: %v", err)
		}
		if resp.StatusCode == http.StatusOK {
			t.Cleanup(func() {
				cancel()
				resp.Body.Close()
			})
			return &watchReader{br: bufio.NewReader(resp.Body), cancel: cancel}
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("GET /watch = %d: %s", resp.StatusCode, body)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the watch slot stayed busy after the previous reader hung up: %s", body)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// nextFrame reads one SSE frame, skipping comment lines, and returns its event
// name and data payload. A stalled stream cancels the request, so a hang shows
// up as a read error instead of a dead test binary.
func (w *watchReader) nextFrame(t *testing.T) (event, data string) {
	t.Helper()
	wd := time.AfterFunc(watchFrameBudget, w.cancel)
	defer wd.Stop()
	for {
		line, err := w.br.ReadString('\n')
		if err != nil {
			t.Fatalf("watch stream: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if data == "" {
				// A comment-only frame (": ping") is liveness, not content.
				if event == "" {
					continue
				}
				t.Fatalf("watch frame with event %q and no data", event)
			}
			return event, data
		case strings.HasPrefix(line, ":"):
			continue
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data += strings.TrimPrefix(line, "data: ")
		default:
			t.Fatalf("unparsable watch line %q", line)
		}
	}
}

// nextBatch decodes the next frame as a WatchBatch and checks the event name,
// which is the only field the reader keys on besides the JSON.
func (w *watchReader) nextBatch(t *testing.T) route.WatchBatch {
	t.Helper()
	event, data := w.nextFrame(t)
	if event != "routes" {
		t.Fatalf("watch event %q, want routes", event)
	}
	var b route.WatchBatch
	if err := json.Unmarshal([]byte(data), &b); err != nil {
		t.Fatalf("decode watch batch: %v\n%s", err, data)
	}
	return b
}

// expectEOF reads until the stream ends. It is what proves the response is gone
// rather than merely quiet: a reader that never sees EOF is a reader that never
// reconnects.
func (w *watchReader) expectEOF(t *testing.T) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		for {
			line, err := w.br.ReadString('\n')
			if err != nil {
				done <- err
				return
			}
			_ = line
		}
	}()
	select {
	case err := <-done:
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	case <-time.After(watchFrameBudget):
		return fmt.Errorf("the stream was still open %s after the client went away", watchFrameBudget)
	}
}

func eventStrings(events []route.RouteEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		s := e.Op + " t" + fmt.Sprint(e.Table) + " d" + fmt.Sprint(e.DeviceID) +
			" v" + fmt.Sprint(e.VRFID) + " " + e.Prefix
		if len(e.NextHops) > 0 {
			s += " [" + strings.Join(e.NextHops, ",") + "]"
		}
		out = append(out, s)
	}
	return out
}

func wantEventStrings(t *testing.T, b route.WatchBatch, want ...string) {
	t.Helper()
	got := eventStrings(b.Events)
	if len(got) != len(want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("events = %q, want %q", got, want)
		}
	}
}

// TestWatchStreamCarriesARealEdit is the end-to-end claim of slice B: a route
// that an RR reflected is in the INIT, and an operator removing the tunnel that
// resolves it comes out of the same stream as a withdraw. The prefix is still
// in the Loc-RIB at that point -- which is precisely why sr-fib has to be told
// in words, not by re-reading a table.
func TestWatchStreamCarriesARealEdit(t *testing.T) {
	rr := newPipeRR()
	c, stop := watchClient(t, rr)
	defer stop()
	srv := newServer(t, c)

	rr.announce(watchRoute())
	eventually(t, "the reflected route installed", func() bool { return c.Stats().RibRoutes == 1 })

	w := openWatch(t, srv)
	b := w.nextBatch(t)
	if !b.Init {
		t.Fatalf("first frame is not an INIT: %q", eventStrings(b.Events))
	}
	wantEventStrings(t, b, "upsert t5 d20 v5 10.20.0.0/16 [192.168.99.2]")

	if code, raw := call(t, srv, http.MethodPut, "/tunnels", `{"tunnels":[]}`); code != http.StatusOK {
		t.Fatalf("PUT /tunnels = %d: %s", code, raw)
	}
	b = w.nextBatch(t)
	if b.Init {
		t.Fatal("a delta frame arrived marked as INIT")
	}
	wantEventStrings(t, b, "withdraw t5 d20 v5 10.20.0.0/16")
	if n := c.Stats().RibRoutes; n != 0 {
		t.Fatalf("the withdraw left %d installable routes", n)
	}
}

// TestWatchSlotIsExclusive checks the loud answer, not the polite one: two
// readers on one box is a bug, and 409 says so in the status code instead of
// silently evicting whoever is already converging the FIB.
func TestWatchSlotIsExclusive(t *testing.T) {
	rr := newPipeRR()
	c, stop := watchClient(t, rr)
	defer stop()
	srv := newServer(t, c)
	rr.announce(watchRoute())
	eventually(t, "the reflected route installed", func() bool { return c.Stats().RibRoutes == 1 })

	w := openWatch(t, srv)
	w.nextBatch(t) // INIT

	code, raw := call(t, srv, http.MethodGet, "/watch", "")
	if code != http.StatusConflict {
		t.Fatalf("second GET /watch = %d, want 409: %s", code, raw)
	}
	if !strings.Contains(raw, "subscriber") {
		t.Fatalf("409 body does not say why: %s", raw)
	}

	// Hanging up releases the slot: the handler's defer, not the client's
	// close, owns that, so the next reader has to be asked for patiently.
	w.cancel()
	w2 := openWatchUntil(t, srv)
	b := w2.nextBatch(t)
	if !b.Init {
		t.Fatalf("replacement reader did not get an INIT: %+v", b)
	}
	wantEventStrings(t, b, "upsert t5 d20 v5 10.20.0.0/16 [192.168.99.2]")
}

// TestWatchStreamEndsWithTheClient is sr-fib's reconnect trigger at the HTTP
// layer. If the response stayed open when the process behind it shut down, the
// reader would sit on a quiet socket and keep installing from a stale view.
func TestWatchStreamEndsWithTheClient(t *testing.T) {
	rr := newPipeRR()
	c, stop := watchClient(t, rr)
	srv := newServer(t, c)

	w := openWatch(t, srv)
	// An empty view still gets an empty INIT: a snapshot with nothing in it is
	// the snapshot, and the reader diffs against it from here.
	if b := w.nextBatch(t); !b.Init || len(b.Events) != 0 {
		t.Fatalf("INIT = %+v, want an empty snapshot", b)
	}

	stop()
	if err := w.expectEOF(t); err != nil {
		t.Fatalf("stream after shutdown: %v", err)
	}
}
