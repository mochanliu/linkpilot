package main

import (
	"context"
	"fmt"
	"net"
	"strings"

	"sdwan/controller/internal/agent"
	"sdwan/controller/internal/wire"
)

// reportSender carries the three facts the controller can only get from this
// box: which addresses it actually owns, which attested WG key it holds, and
// which tunnels its dataplane really runs. Everything here is a reading or a
// signed statement -- nothing is inferred from the bundle the controller sent,
// because "the controller configured it" and "this box runs it" are exactly the
// two facts a console must not conflate.
//
// Empty values are deliberate where the agent has no measurement: an address
// is only reported when a reflector actually answered for it and the verdict is
// still fresh (see agent.StunSync), so UDPMapping/Reflector/ProbeLocal stay
// empty and SymmetricKnown stays false until that happens. Sending the zero
// value is honest only because that flag says "not measured".
type reportSender struct {
	client *agent.Client
	id     *agent.Identity
	tun    *agent.TunnelSync // nil: this agent has no tunnel applier
	// stun is nil only when the binary was started with --stun-reflectors 0: no
	// probe socket, so no third-party observation of this box is available and
	// the mapping report carries the binding and the declared addresses alone.
	stun *agent.StunSync
	logf func(string, ...any)

	lastErr string // the last warning, so an unchanged failure is not repeated
}

func (r *reportSender) send(ctx context.Context, ifaces *[]wire.IfaceInfo) {
	if r == nil || r.client == nil || r.id == nil {
		return
	}
	r.reportMapping(ctx, ifaces)
	r.reportObservations(ctx, ifaces)
}

func (r *reportSender) reportMapping(ctx context.Context, ifaces *[]wire.IfaceInfo) {
	bs, err := r.id.Bindings()
	if err != nil {
		r.warnf("映射回报跳过：取不出 WG 密钥担保: %v", err)
		return
	}
	req := wire.MappingReport{
		Bindings:   bs,
		LocalAddrs: localAddrs(ifaces),
	}
	// The observation half comes from the probe loop, and only while it is fresh:
	// this is the one field in the report the device cannot assert for itself.
	r.stun.Apply(&req)
	// A tunnel table is declared only when a tunnel applier exists: nil means
	// "not reported" (keep what the registry has), while an empty list means
	// "I run no tunnels" and clears the next hops peers resolve routes against.
	if r.tun != nil {
		req.Tunnels = r.tun.Endpoints()
	}
	ack, err := r.client.ReportMapping(ctx, req)
	if err != nil {
		r.warnf("映射回报失败: %v", err)
		return
	}
	if ack.BindingErr != "" {
		// The node stays registered but its key binding could not be verified:
		// peers cannot build a peer entry for this box, and the symptom is
		// "tunnel is up, no traffic". That must be loud, not swallowed.
		r.warnf("控制器没接受本机的密钥担保: %s", ack.BindingErr)
		return
	}
	if req.UDPMapping == "" {
		// A failed probe is not a failed report, but it is the only sentence that
		// explains why the console still says 未上报 for this box's outer address.
		if why := r.stun.Snapshot().Err; why != "" {
			r.warnf("地址探测没有结果（本机外层地址将保持未上报）: %s", why)
			return
		}
	}
	r.lastErr = ""
}

// dataPlane is the connection the observation table is read over. It belongs to
// the tunnel applier, so the reader follows the reconciler across a VPP restart
// instead of holding a handle to a socket that no longer exists. Without a
// tunnel applier there is nothing to read it from, and the report is skipped
// rather than sent as "no observations seen".
func (r *reportSender) dataPlane() agent.DataPlane {
	if r.tun == nil {
		return nil
	}
	return r.tun.DataPlane()
}

func (r *reportSender) reportObservations(ctx context.Context, ifaces *[]wire.IfaceInfo) {
	dp := r.dataPlane()
	if dp == nil {
		return
	}
	obs, err := dp.Observations(ctx)
	if err != nil {
		r.warnf("读数据面观测表失败: %v", err)
		return
	}
	// Clear BEFORE reporting: Reset=true is only truthful when the table really
	// was emptied, and that flag is what lets the controller drop a source this
	// box no longer sees (otherwise a dead mapping stays advertised forever).
	if err := dp.ClearObservations(ctx); err != nil {
		r.warnf("清空数据面观测表失败: %v", err)
		return
	}
	names := ifaceNames(ifaces)
	rep := wire.ObserveReport{Reset: true, Observations: make([]wire.Observation, 0, len(obs))}
	for _, o := range obs {
		rep.Observations = append(rep.Observations, wire.Observation{
			SrcIP:    o.SrcIP.String(),
			SrcPort:  o.SrcPort,
			DstPort:  o.DstPort,
			RxIf:     names[o.RXIf],
			AgeSec:   uint64(o.AgeSec),
			Verified: o.Verified,
			Hits:     uint64(o.Hits),
		})
	}
	resp, err := r.client.ReportObservations(ctx, rep)
	if err != nil {
		r.warnf("观测回报失败（本轮 %d 条已从数据面清掉）: %v", len(obs), err)
		return
	}
	if len(resp.Unattributed) > 0 {
		// Unattributed means nobody claimed that source over a certificate. It
		// is not this node's fault, but it is the sentence that explains a
		// tunnel that will not come up, so it belongs in the box's log too.
		parts := make([]string, 0, len(resp.Unattributed))
		for _, u := range resp.Unattributed {
			parts = append(parts, fmt.Sprintf("%s（对端端口 %d：%s）", u.Mapping, u.DstPort, u.Reason))
		}
		r.warnf("观测里有对不上的外层源: %s", strings.Join(parts, "；"))
		return
	}
	r.lastErr = ""
}

func (r *reportSender) warnf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if msg == r.lastErr {
		return
	}
	r.lastErr = msg
	if r.logf != nil {
		r.logf("%s", msg)
	}
}

// localAddrs is the address set the controller attributes observed outer
// sources against. Prefix lengths are dropped because that table is keyed by
// address, and link-local addresses are dropped because no peer can see one.
func localAddrs(ifaces *[]wire.IfaceInfo) []string {
	if ifaces == nil {
		return nil
	}
	// Non-nil so callers can tell "read it, nothing qualified" from "could not
	// read it" (the field itself is omitempty, so the wire carries both alike).
	out := []string{}
	for _, i := range *ifaces {
		for _, a := range i.Addrs {
			host := a
			if j := strings.IndexByte(host, '/'); j >= 0 {
				host = host[:j]
			}
			ip := net.ParseIP(host)
			if ip == nil || ip.To4() == nil || ip.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, host)
		}
	}
	return out
}

func ifaceNames(ifaces *[]wire.IfaceInfo) map[uint32]string {
	out := map[uint32]string{}
	if ifaces == nil {
		return out
	}
	for _, i := range *ifaces {
		out[i.SWIfIndex] = i.Name
	}
	return out
}
