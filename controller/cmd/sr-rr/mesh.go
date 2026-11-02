package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"sdwan/controller/internal/keybinding"
	"sdwan/controller/internal/route"
)

// The mesh is grown from the controller's RR library, not configured: every
// poll the RR asks /v1/rr/config who else exists in its tenant and diffs the
// answer against what it is already connected to. Rows are only ever added
// (ids are monotonic), so an added row shows up here as exactly one new peer
// and leaves every existing session untouched.
//
// One direction only: the lexicographically smaller rr_id dials. Both ends
// read the same pair of ids from the same library, so they agree on who dials
// and each pair gets exactly one session -- no double-dial, no collision
// resolution. The cost is stated in the plan: a pair where only the larger id
// can reach the smaller gets no session, and both /status pages say so.

const (
	meshInitialBackoff = time.Second
	meshMaxBackoff     = 30 * time.Second
	// A session that lived this long was established, so its disconnect starts
	// the reconnect backoff over instead of continuing to climb.
	meshStableSession = 5 * time.Second
)

// errMeshFatal means the controller does not recognize this RR any more (or
// does not accept its certificate). Nothing this process can do makes that
// better, so it stops instead of retrying forever as a ghost in the grid.
var errMeshFatal = errors.New("controller does not accept this RR")

type rrPeer struct {
	ID   string `json:"id"`
	Addr string `json:"addr"`
}

type rrConfigResponse struct {
	Tenant string   `json:"tenant"`
	RRID   string   `json:"rr_id"`
	Peers  []rrPeer `json:"peers"`
}

type meshParams struct {
	addr    string // controller base URL, https://host:port
	poll    time.Duration
	tenant  string
	rrID    string
	tlsBase *tls.Config // our certificate + the controller CA
	hc      *http.Client
	ref     *route.Reflector
	logf    func(format string, args ...any)
}

type mesh struct {
	meshParams

	mu    sync.Mutex
	peers map[string]*meshPeer
}

type meshPeer struct {
	id      string
	addr    string
	dialing bool // this side owns the direction (rr_id ordering)

	// guarded by mesh.mu
	cancel    context.CancelFunc
	conn      net.Conn // live outbound connection, closed on removal
	lastErr   string
	loggedErr string
}

func newMesh(p meshParams) *mesh {
	return &mesh{meshParams: p, peers: make(map[string]*meshPeer)}
}

// run polls until the process stops. A transient failure keeps the current
// grid and retries with backoff; a fatal answer (401/403/404) returns an error
// and the caller exits non-zero.
func (m *mesh) run(ctx context.Context) error {
	backoff := meshInitialBackoff
	lastPollErr := ""
	for {
		peers, err := m.fetch(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, errMeshFatal) {
				return err
			}
			if err.Error() != lastPollErr {
				m.logf("sr-rr %s: peer list from %s: %v (grid kept; retrying)", m.rrID, m.addr, err)
				lastPollErr = err.Error()
			}
			if !sleepCtx(ctx, backoff) {
				return nil
			}
			backoff = nextMeshBackoff(backoff)
			continue
		}
		lastPollErr = ""
		backoff = meshInitialBackoff
		m.reconcile(ctx, peers)
		if !sleepCtx(ctx, m.poll) {
			return nil
		}
	}
}

func (m *mesh) fetch(ctx context.Context) ([]rrPeer, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	url := strings.TrimRight(m.addr, "/") + "/v1/rr/config"
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := m.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return nil, fmt.Errorf("%w: HTTP %d: %s", errMeshFatal, resp.StatusCode,
			strings.TrimSpace(string(body)))
	default:
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out rrConfigResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parse answer: %w", err)
	}
	if out.Tenant != m.tenant || out.RRID != m.rrID {
		// A controller that names someone else would have us dial and accept
		// sessions under the wrong identity.
		return nil, fmt.Errorf("controller answered for tenant %q rr %q, we are %q rr %q",
			out.Tenant, out.RRID, m.tenant, m.rrID)
	}
	return out.Peers, nil
}

// reconcile applies one peer list: gone peers are cancelled, new ones get a
// dial loop, and untouched peers keep their loop (and their `since`) exactly
// as they were. An address change is the one case that restarts that pair.
func (m *mesh) reconcile(parent context.Context, peers []rrPeer) {
	desired := make(map[string]string, len(peers))
	for _, p := range peers {
		if p.ID == m.rrID || p.ID == "" || p.Addr == "" {
			continue
		}
		desired[p.ID] = p.Addr
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for id, p := range m.peers {
		addr, kept := desired[id]
		if kept && addr == p.addr {
			continue
		}
		m.stopPeerLocked(p)
		delete(m.peers, id)
		if kept {
			m.logf("sr-rr %s: mesh peer %s moved from %s to %s", m.rrID, id, p.addr, addr)
		} else {
			m.logf("sr-rr %s: mesh peer %s (%s) left the grid", m.rrID, id, p.addr)
		}
	}
	for id, addr := range desired {
		if _, ok := m.peers[id]; ok {
			continue
		}
		p := &meshPeer{id: id, addr: addr, dialing: m.rrID < id}
		m.peers[id] = p
		if !p.dialing {
			continue
		}
		ctx, cancel := context.WithCancel(parent)
		p.cancel = cancel
		go m.dialLoop(ctx, p)
	}
}

// stopPeerLocked ends the pair's dial loop and its live session, if any.
// Closing the socket makes route.DialConn return immediately instead of
// waiting for the hold timer, so a removed row is gone within one poll.
func (m *mesh) stopPeerLocked(p *meshPeer) {
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	if p.conn != nil {
		p.conn.Close()
		p.conn = nil
	}
}

func (m *mesh) dialLoop(ctx context.Context, p *meshPeer) {
	backoff := meshInitialBackoff
	for ctx.Err() == nil {
		start := time.Now()
		err := m.sessionOnce(ctx, p)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) >= meshStableSession {
			backoff = meshInitialBackoff
		}
		m.recordErr(p, err)
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = nextMeshBackoff(backoff)
	}
}

// sessionOnce dials, checks who answered, and runs one session until it ends.
func (m *mesh) sessionOnce(ctx context.Context, p *meshPeer) error {
	conn, err := m.dial(ctx, p.addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", p.addr, err)
	}
	m.mu.Lock()
	if p.cancel == nil {
		// The row disappeared while we were dialing.
		m.mu.Unlock()
		conn.Close()
		return errors.New("peer left the grid while dialing")
	}
	p.conn = conn
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		p.conn = nil
		m.mu.Unlock()
	}()

	id, err := dialIdentity(conn)
	if err != nil {
		conn.Close()
		return err
	}
	if err := m.ref.DialConn(conn, id); err != nil {
		return err
	}
	return errors.New("session ended")
}

// dialIdentity is the outbound mirror of serveConn's certificate check: the
// session speaks for whatever the peer certificate says, and it must be an rr.
func dialIdentity(conn net.Conn) (route.PeerIdentity, error) {
	tc, ok := conn.(*tls.Conn)
	if !ok {
		return route.PeerIdentity{}, errors.New("not a TLS connection")
	}
	peers := tc.ConnectionState().PeerCertificates
	if len(peers) == 0 {
		return route.PeerIdentity{}, errors.New("peer presented no certificate")
	}
	ref, err := keybinding.RefOf(peers[0])
	if err != nil {
		return route.PeerIdentity{}, err
	}
	if ref.Role != keybinding.RoleRR {
		return route.PeerIdentity{}, fmt.Errorf("peer certificate is %s, not a reflector", ref)
	}
	return route.PeerIdentity{Tenant: ref.Tenant, Role: route.RoleRR, RRID: ref.RRID}, nil
}

// dial uses our own certificate as the client certificate and verifies the
// peer against the controller CA. The ServerName is the host part of the
// peer's row -- the same rule sr-route uses for its RR addresses.
func (m *mesh) dial(ctx context.Context, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("peer address %q: %w", addr, err)
	}
	tcfg := m.tlsBase.Clone()
	tcfg.ServerName = host
	d := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second},
		Config:    tcfg,
	}
	return d.DialContext(ctx, "tcp", addr)
}

func (m *mesh) recordErr(p *meshPeer, err error) {
	if err == nil {
		return
	}
	msg := err.Error()
	m.mu.Lock()
	p.lastErr = msg
	show := p.loggedErr != msg
	if show {
		p.loggedErr = msg
	}
	m.mu.Unlock()
	if show {
		m.logf("sr-rr %s: mesh peer %s: %s", m.rrID, p.id, msg)
	}
}

// peerStatus is the /status view of one row: connected comes from the live
// session table (the reflector is the truth), so a peer this side does not
// dial still reports the session the other side opened.
type peerStatus struct {
	ID        string     `json:"id"`
	Addr      string     `json:"addr"`
	Dialing   bool       `json:"dialing"`
	Connected bool       `json:"connected"`
	Since     *time.Time `json:"since,omitempty"`
	LastError string     `json:"last_error,omitempty"`
}

func (m *mesh) Status() []peerStatus {
	sessions := make(map[string]route.SessionInfo)
	for _, s := range m.ref.Sessions() {
		sessions[s.Key] = s
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]peerStatus, 0, len(m.peers))
	for _, p := range m.peers {
		ps := peerStatus{ID: p.id, Addr: p.addr, Dialing: p.dialing, LastError: p.lastErr}
		if s, ok := sessions["rr/"+p.id]; ok {
			ps.Connected = true
			since := s.Since
			ps.Since = &since
		}
		out = append(out, ps)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func nextMeshBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > meshMaxBackoff {
		d = meshMaxBackoff
	}
	return d
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
