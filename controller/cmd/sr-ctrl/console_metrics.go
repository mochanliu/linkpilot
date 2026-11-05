// The console's monitoring read: device liveness plus the latest in-tunnel
// telemetry window.
//
// It is a SEPARATE resource from /api/state for two reasons. Telemetry has its
// own freshness -- a window expires after --metrics-ttl, long before a node goes
// stale -- and folding it into the state document would make every 10s refetch
// carry numbers that change on a 300ms probe cadence. And the operator role is
// scoped to inventory by the same rule that keeps runtime out of /api/state:
// how a customer's links are performing is the customer's view, not the
// operator's.
//
// Every field here is a fact the controller holds right now. There is no
// synthesized "unknown" tunnel row: a device that has not reported gets an
// EMPTY row set and the page says so, because an invented row is
// indistinguishable from a measured one in a screenshot.
package main

import (
	"net/http"
	"sort"
	"time"

	"sdwan/controller/internal/wire"
)

type consoleMetrics struct {
	Controller consoleController `json:"controller"`
	NowUnix    int64             `json:"now_unix"`
	// MetricsTTLSec is the read-side window policy (--metrics-ttl). The page
	// states it instead of hardcoding a number that could drift from the flag.
	MetricsTTLSec int `json:"metrics_ttl_sec"`
	// Telemetry is the BFD/probe policy that goes out in the bundle, so it is
	// what is installed on the devices right now -- not what a draft form on
	// the settings page says. It is a pointer because a tenant with no
	// published underlay has no policy at all: the page must then say the
	// verdict window is unknown, not quote the built-in defaults.
	Telemetry *wire.TelemetryPolicy `json:"telemetry"`
	Nodes     []consoleNodeMetrics  `json:"nodes"`
}

type consoleNodeMetrics struct {
	NodeID   uint16 `json:"node_id"`
	Serial   string `json:"serial"`
	AgentVer string `json:"agent_version"`
	Status   string `json:"status"` // online | stale | pending

	LastSeen        int64 `json:"last_seen_unix"`
	HeartbeatAgeSec int64 `json:"heartbeat_age_sec"`
	// UptimeSec is the agent's last reported process uptime. Zero means it never
	// reported one; it is NOT a statement that the device just booted.
	UptimeSec int64 `json:"uptime_sec"`

	UDPMapping string `json:"udp_mapping"`
	VpnPort    uint16 `json:"vpn_port"`

	// Telemetry is this node's latest window, unjoined from any peer's. A zero
	// TelemetrySeenAt means "never reported", which is different from "reported
	// and then went quiet"; AgeSec is computed at read time for that reason.
	TelemetrySeenAt int64                `json:"telemetry_seen_at_unix"`
	TelemetryAgeSec int64                `json:"telemetry_age_sec"`
	Tunnels         []wire.TunnelMetrics `json:"tunnels"`
}

func (s *server) handleAdminMetrics(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	// Tenants monitor their own network; the operator role sees inventory only.
	if id.Role != roleTenant {
		http.Error(w, "tenant role required", 403)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", 405)
		return
	}

	now := time.Now().Unix()
	// Empty collections must serialize as [] rather than null: the page filters
	// these slices unconditionally, and a tenant with no device yet is normal.
	out := consoleMetrics{
		NowUnix:       now,
		MetricsTTLSec: s.telTTLSec,
		Nodes:         []consoleNodeMetrics{},
	}
	s.mu.Lock()
	out.Controller = consoleController{
		TLSAddrs:     append([]string{}, s.tlsAddrs...),
		HeartbeatSec: s.hbSec,
		Epoch:        s.state.Epoch,
	}
	// Same gate as the bundle: an underlay that was never published has no
	// policy to show, and quoting DefaultSettings() here would be inventing a
	// number the devices never received.
	if uc := s.state.Underlays[id.TenantID]; uc != nil && uc.State != nil && uc.State.Configured() {
		p := uc.State.Settings.TelemetryPolicy()
		out.Telemetry = &p
	}
	for _, n := range s.state.Nodes {
		if n.TenantID != id.TenantID {
			continue
		}
		row := consoleNodeMetrics{
			NodeID: n.NodeID, Serial: n.Serial, AgentVer: n.AgentVer,
			Status: nodeStatus(n), LastSeen: n.LastSeen, UptimeSec: n.UptimeSec,
			UDPMapping: n.UDPMapping, VpnPort: n.VpnPort,
			Tunnels: []wire.TunnelMetrics{},
		}
		if n.LastSeen > 0 && now > n.LastSeen {
			row.HeartbeatAgeSec = now - n.LastSeen
		}
		// A stale node is still LISTED -- "which device fell off" is the whole
		// point of the health table -- but its last window is not shown. The
		// sweeper usually deleted it already; this guard also covers the two
		// seconds between the lapse and the sweep.
		if !n.Stale {
			if t := s.tel[key(n.TenantID, n.NodeID)]; t != nil {
				row.TelemetrySeenAt = t.seenAt
				if now > t.seenAt {
					row.TelemetryAgeSec = now - t.seenAt
				}
				if t.rows != nil {
					row.Tunnels = append([]wire.TunnelMetrics{}, t.rows...)
				}
			}
		}
		out.Nodes = append(out.Nodes, row)
	}
	s.mu.Unlock()

	// Node order is the operator's mental order, not enrollment order: node 1,
	// node 2, ... within a tenant.
	sort.Slice(out.Nodes, func(i, j int) bool { return out.Nodes[i].NodeID < out.Nodes[j].NodeID })
	writeJSON(w, out)
}
