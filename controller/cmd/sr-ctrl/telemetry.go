// In-tunnel telemetry: the northbound side of wire.MetricsReport.
//
// The store is deliberately NOT part of State and therefore not persisted. A
// metrics window is a statement about NOW -- probe counters are deltas over a
// window and BFD state describes a session that may not even exist after a
// restart. Restoring one from disk after a controller restart would present a
// 40-second-old "up" as current, which is the one thing a reachability view
// must never do. The agent reposts within one window anyway.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strconv"
	"time"

	"sdwan/controller/internal/wire"
)

// nodeTelemetry is one node's latest window. It REPLACES the previous one on
// every report: the agent already computed the delta, so there is nothing here
// to accumulate, and accumulating would resurrect a lost report's counters into
// the next window's numbers.
type nodeTelemetry struct {
	rows   []wire.TunnelMetrics
	seenAt int64
}

// handleMetrics serves both directions of the same resource:
//
//	POST /v1/metrics  an agent posts its latest window (replaces the old one)
//	GET  /v1/metrics  any node of the tenant reads every node's latest window
//
// The GET is tenant-wide, not peer-scoped: BFD is a session between two nodes
// and a one-ended report cannot say whether a link is down or merely one-sided,
// so the reader has to be able to see both ends side by side.
func (s *server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	tenant, nodeID, err := peerTenantFromCert(r)
	if err != nil {
		http.Error(w, err.Error(), 401)
		return
	}
	switch r.Method {
	case http.MethodPost:
		s.postMetrics(w, r, tenant, nodeID)
	case http.MethodGet:
		s.getMetrics(w, r, tenant, nodeID)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", 405)
	}
}

func (s *server) postMetrics(w http.ResponseWriter, r *http.Request, tenant string, nodeID uint16) {
	var rep wire.MetricsReport
	if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	// Same rule as heartbeat: the certificate names the node, so a report may
	// only ever describe itself. Anything else would let one node overwrite a
	// peer's telemetry -- and "the other end says down" would stop being evidence.
	if rep.NodeID != nodeID {
		http.Error(w, "node id does not match certificate", 403)
		return
	}
	seen := map[uint32]bool{}
	for _, row := range rep.Tunnels {
		// sw_if_index 0 is VPP's "no interface"; a row keyed on it could never be
		// matched to a tunnel or to a probe session, so accepting it would mean
		// storing a row nobody can act on.
		if row.SwIfIndex == 0 {
			http.Error(w, "tunnel row with sw_if_index 0", 400)
			return
		}
		if seen[row.SwIfIndex] {
			http.Error(w, "duplicate sw_if_index "+strconv.FormatUint(uint64(row.SwIfIndex), 10)+" in report", 400)
			return
		}
		seen[row.SwIfIndex] = true
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Nodes[key(tenant, nodeID)]; !ok {
		http.Error(w, "unknown node", 404)
		return
	}
	s.tel[key(tenant, nodeID)] = &nodeTelemetry{
		rows:   rep.Tunnels,
		seenAt: time.Now().Unix(),
	}
	// No save(): see the file header. Responding without touching disk also keeps
	// a 300 ms probe cadence from turning into a 300 ms fsync cadence.
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *server) getMetrics(w http.ResponseWriter, r *http.Request, tenant string, nodeID uint16) {
	var only uint16
	if q := r.URL.Query().Get("node_id"); q != "" {
		n, err := strconv.ParseUint(q, 10, 16)
		if err != nil {
			http.Error(w, "bad node_id", 400)
			return
		}
		only = uint16(n)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if only != 0 {
		if _, ok := s.state.Nodes[key(tenant, only)]; !ok {
			http.Error(w, "unknown node", 404)
			return
		}
	}
	json.NewEncoder(w).Encode(s.telemetryLocked(tenant, nodeID, only, time.Now().Unix()))
}

// telemetryLocked renders the view for one reader.
//
// Stale nodes are omitted: their heartbeat already lapsed, and a window that
// arrived before the lapse describes a box that is no longer answering. Nodes
// with no window yet are INCLUDED with an empty row set -- "this node is not
// reporting telemetry" is exactly the kind of gap an operator needs to see,
// and leaving it out would make a half-instrumented tenant look complete.
//
// Must be called with s.mu held.
func (s *server) telemetryLocked(tenant string, reader, only uint16, now int64) wire.TelemetryView {
	v := wire.TelemetryView{TenantID: tenant, Self: reader, Nodes: []wire.NodeTelemetry{}}
	for _, rec := range s.state.Nodes {
		if rec.TenantID != tenant || rec.Stale {
			continue
		}
		if only != 0 && rec.NodeID != only {
			continue
		}
		row := wire.NodeTelemetry{NodeID: rec.NodeID, Tunnels: []wire.TunnelMetrics{}}
		if t := s.tel[key(tenant, rec.NodeID)]; t != nil {
			row.SeenAt = t.seenAt
			if now > t.seenAt {
				row.AgeSec = uint64(now - t.seenAt)
			}
			if t.rows != nil {
				row.Tunnels = t.rows
			}
		}
		v.Nodes = append(v.Nodes, row)
	}
	sort.Slice(v.Nodes, func(i, j int) bool { return v.Nodes[i].NodeID < v.Nodes[j].NodeID })
	return v
}

// expireTelemetryLocked drops windows older than the TTL. An agent that stops
// reporting (crash, probe plugin unloaded, path break) must not leave its last
// good window in the view forever -- "no data" and "40 seconds old" are
// different facts, and only one of them is worth acting on.
//
// Must be called with s.mu held.
func (s *server) expireTelemetryLocked(now int64) {
	ttl := int64(s.telTTLSec)
	for k, t := range s.tel {
		if now-t.seenAt > ttl {
			log.Printf("metrics: dropping telemetry for %s (%ds old, ttl %ds)", k, now-t.seenAt, ttl)
			delete(s.tel, k)
		}
	}
}
