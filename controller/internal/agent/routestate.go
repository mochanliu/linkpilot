package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"sdwan/controller/internal/route"
	"sdwan/controller/internal/wire"
)

// routeStatePushTimeout bounds one PUT /tunnel-state. sr-route answers in
// milliseconds; the metrics loop must never stall on a wedged sibling process
// -- the sample's own window is the deadline, not the HTTP round trip.
const routeStatePushTimeout = 3 * time.Second

// RouteStatePusher feeds the agent's per-tunnel BFD verdict to the sr-route
// client on this same box, so reachableLocked can consult it (#275). It reads
// route.json's status_addr once at construction: the local URL never changes
// during a box's life, and a route.json rewritten by the controller only
// changes tunnels/vrfs/rrs -- status_addr is a box key the operator wrote.
//
// A zero-value pusher (no route.json on disk, no status_addr in it) is not an
// error to construct -- it is an error to push. Callers should keep running:
// without tunnel-state, sr-route falls back to the config gate, which is the
// pre-#275 behaviour.
type RouteStatePusher struct {
	base string
	hc   *http.Client
}

// NewRouteStatePusher reads the sr-route config file at routeConfPath and
// returns a pusher pointed at its status_addr. Empty path skips it entirely
// (nil, nil) so tests that never had sr-route on the same host can start.
func NewRouteStatePusher(routeConfPath string) (*RouteStatePusher, error) {
	if routeConfPath == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(routeConfPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读 %s: %w", routeConfPath, err)
	}
	base, ok := statusBaseFromRouteFile(raw)
	if !ok {
		return nil, nil
	}
	return &RouteStatePusher{base: base, hc: &http.Client{}}, nil
}

// statusBaseFromRouteFile picks the URL out of the file's status_addr key.
// Loopback if the operator wrote a wildcard, matching what sr-apply already
// does when it pushes overlays.
func statusBaseFromRouteFile(raw []byte) (string, bool) {
	var box struct {
		StatusAddr string `json:"status_addr"`
	}
	if err := json.Unmarshal(raw, &box); err != nil {
		return "", false
	}
	a := strings.TrimSpace(box.StatusAddr)
	if a == "" {
		return "", false
	}
	if strings.HasPrefix(a, "http://") || strings.HasPrefix(a, "https://") {
		return strings.TrimRight(a, "/"), true
	}
	host, port, wildcard := splitHostPort(a)
	if wildcard {
		return "http://127.0.0.1:" + port, true
	}
	return "http://" + host + ":" + port, true
}

func splitHostPort(a string) (host, port string, wildcard bool) {
	i := strings.LastIndex(a, ":")
	if i < 0 {
		return "", a, true
	}
	host, port = a[:i], a[i+1:]
	switch host {
	case "", "0.0.0.0", "::", "[::]", "[::1]", "::1":
		return host, port, true
	}
	return host, port, false
}

// Push sends the agent's latest per-tunnel BFD rows to sr-route. An empty
// rows slice is a legitimate "no information this round": sr-route clears
// the map and every peer falls back to the config gate.
//
// A nil receiver is a no-op so callers do not need to null-check the pusher
// when the box has no route.json (an unenrolled CPE, a test harness).
func (p *RouteStatePusher) Push(ctx context.Context, rows []wire.TunnelMetrics) error {
	if p == nil {
		return nil
	}
	states := make([]route.TunnelState, 0, len(rows))
	for _, r := range rows {
		// peer_node_id 0 means the row never made it into the controller's
		// tunnel table (a half-installed WG interface). sr-route cannot gate
		// a route on it, and the client rejects 0 -- drop it here rather than
		// fail the whole batch.
		if r.PeerNodeID == 0 {
			continue
		}
		states = append(states, route.TunnelState{PeerNodeID: r.PeerNodeID, BFDState: r.BFDState})
	}
	body, err := json.Marshal(map[string]any{"peers": states})
	if err != nil {
		return err
	}
	pctx, cancel := context.WithTimeout(ctx, routeStatePushTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodPut, p.base+"/tunnel-state", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := p.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("sr-route 拒绝 tunnel-state (%d): %s", resp.StatusCode, strings.TrimSpace(buf.String()))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return nil
}
