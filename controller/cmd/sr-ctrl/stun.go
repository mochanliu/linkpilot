package main

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"sdwan/controller/internal/stun"
)

// The built-in STUN reflector is the third-party witness the address discovery
// needs: a device's own claim about where it can be reached is worthless, and
// the one fact nobody else can supply is "I received that packet from this
// source address". So the controller answers Binding Requests itself rather
// than depending on an external service.
//
// It runs on a KERNEL UDP socket, not in the dataplane (定案 2026-11-03). The
// consequence, stated here so nobody has to rediscover it: an observation made
// this way describes the path the probe took out of the kernel, and on a box
// whose WAN address lives in VPP that is not the path a tunnel would use. That
// is why the report carries the probe's local source too (wire.MappingReport.
// ProbeLocal) and why the planner refuses an observation that does not match
// the device's own interface inventory, instead of quietly building a tunnel
// from a public address that belongs to the wrong box.
//
// It is disabled by default: a controller started without -stun advertises an
// empty list, agents probe nothing, and every device keeps reporting "no
// observation" -- which the console shows as 未上报 rather than as a zero.
//
// Why two addresses are worth configuring: the symmetric-NAT verdict is defined
// as "the same local socket gets a different mapping for a different
// destination", so ONE reflector can never measure it. Two ports on this one
// process are enough (two distinct destinations), and no second machine is
// required.

// serveStun binds every address in binds, answers on each, and returns the list
// to advertise to agents. It fails (rather than half-starts) if any address
// cannot be bound: an agent told to probe a dead reflector reports "no
// observation", which is indistinguishable from a device behind a firewall --
// the one error a control plane must not make at boot.
func serveStun(ctx context.Context, binds []string, logf func(string, ...any)) ([]string, error) {
	out := make([]string, 0, len(binds))
	refs := make([]*stun.Reflector, 0, len(binds))
	conns := make([]net.PacketConn, 0, len(binds))
	for _, b := range binds {
		conn, err := net.ListenPacket("udp", b)
		if err != nil {
			for _, c := range conns {
				_ = c.Close()
			}
			return nil, fmt.Errorf("stun: 反射器绑定 %s 失败: %w", b, err)
		}
		r := &stun.Reflector{}
		conns = append(conns, conn)
		refs = append(refs, r)
		out = append(out, conn.LocalAddr().String())
		// LocalAddr after a real bind carries the resolved port, so a bind on ":0"
		// would advertise a port nobody else can reach -- parseStunAddrs forbids
		// that, and the log line below is where an operator sees the truth.
		go func(b string, r *stun.Reflector, conn net.PacketConn) {
			if err := r.Serve(ctx, conn); err != nil && ctx.Err() == nil {
				logf("stun: 反射器 %s 停止应答: %v", b, err)
			}
		}(b, r, conn)
		logf("stun: 反射器在 %s 上应答（内核 UDP）", conn.LocalAddr())
	}
	// One reporter for the whole service: the numbers that matter operationally
	// are "requests arrived, replies went out, and did any of them not fit",
	// which is a per-port reading nobody asks for.
	go func() {
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				var req, rep, bad, v6 uint64
				for _, r := range refs {
					a, b, c, d := r.Stats()
					req += a
					rep += b
					bad += c
					v6 += d
				}
				logf("stun: 累计 请求=%d 应答=%d 无法解析=%d 非IPv4源=%d", req, rep, bad, v6)
			}
		}
	}()
	return out, nil
}

// advertiseStun is the list this controller hands a device, capped at however
// many it said it can probe (max < 0 means the caller has no offer to honour,
// so the whole list goes out and the agent applies its own cap).
//
// The cap is not decoration. An agent that was built without a probe socket
// offers 0 and must get nothing: being told to probe a reflector it cannot
// reach produces a permanent "discovery failed" row for a device that simply
// has no client, which is indistinguishable on the page from a firewall.
func (s *server) advertiseStun(max int) []string {
	if len(s.reflectors) == 0 {
		return nil
	}
	if max >= 0 && max < len(s.reflectors) {
		return append([]string{}, s.reflectors[:max]...)
	}
	return append([]string{}, s.reflectors...)
}

// parseStunAddrs turns the -stun value into the list to bind AND advertise.
//
// Every entry must name an explicit host: "0.0.0.0:3478" or ":3478" binds
// fine but says nothing about which of this machine's addresses an agent can
// reach, and guessing from the interface table is exactly what 控制器不猜客户的网络
// forbids. The operator names the address; we bind that address.
func parseStunAddrs(list string) ([]string, error) {
	var out []string
	for _, raw := range strings.Split(list, ",") {
		a := strings.TrimSpace(raw)
		if a == "" {
			continue
		}
		host, port, err := net.SplitHostPort(a)
		if err != nil {
			return nil, fmt.Errorf("stun: %q 不是 ip:port: %w", a, err)
		}
		if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
			return nil, fmt.Errorf("stun: %q 的主机部分必须是一个明确的 IP（反射器要把这个地址原样告诉设备，绑 0.0.0.0 猜不出设备该拨哪个地址）", a)
		}
		// 端口当场数一遍：非数字的端口要在这里说清楚，而不是等绑定失败时变成
		// 一句 "listen udp: lookup notaport" —— 那读起来像 DNS 坏了。
		// 0 是允许的：让内核选口，代价是每次重启广告出去的端口都不同。
		p, err := strconv.Atoi(port)
		if err != nil || p < 0 || p > 65535 || strconv.Itoa(p) != port {
			return nil, fmt.Errorf("stun: %q 的端口部分不是一个可用的端口号（0～65535，不写前导零）", a)
		}
		if joined := net.JoinHostPort(host, port); joined != a {
			return nil, fmt.Errorf("stun: %q 写法不规范，请用 %q", a, joined)
		}
		out = append(out, a)
	}
	if len(out) == 0 {
		// Only reachable with a value that was all separators and spaces. A
		// non-empty flag that produced no listener must not read as success.
		return nil, fmt.Errorf("stun: --stun=%q 里没有一个可用的 ip:port", list)
	}
	return out, nil
}
