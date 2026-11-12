// sr-svc: the box's own DHCPv4 server + DNS forwarder.
//
// Why a user-space daemon and not VPP: VPP 26.06 has no DHCP server (only a
// client and a relay) and its dns_plugin is a resolver cache, not a server for
// clients. The service plane works because linux-cp's interface pairs come with
// `ip punt redirect rx <客户口> via <tap>` pre-installed: packets addressed to
// the box's own customer-port address are handed to the kernel TAP, and replies
// from the TAP are routed back into VPP. Measured on lab-cpe1 (2026-11-11):
// dig @<客户口地址> answered, a DHCP DISCOVER broadcast reached a kernel :67
// listener, and a broadcast OFFER from the kernel reached the customer. sr-svc
// binds those same sockets and serves.
//
// Config: /etc/sdwan/svc.json (written by sr-apply from the bundle's services
// segment). Rows carry everything resolved: TAP name, listen address, pool
// bounds, gateway/DNS. SIGHUP reloads rows without dropping leases; leases live
// in a JSON file so a daemon restart keeps them.
//
// What this binary deliberately is not: no dnsmasq/kea/unbound dependency (the
// image ships none of them and a branch box must not need packages), no DHCPv6,
// no DNS cache (v1 forwards every query upstream).
package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const (
	// statusAddr is sr-svc's local-only status port. 127.0.0.1 on purpose: the
	// lease table is not something the customer side should reach, and the
	// operator reads it from the box itself (or the next telemetry hop).
	statusAddr    = "127.0.0.1:8092"
	leasesFile    = "/var/lib/sr-svc/leases.json"
	dhcpServerPort = 67
	dhcpClientPort = 68
)

// DHCPConfig mirrors the services segment's dhcp rows (internal/apply
// services.go). The JSON shapes are the contract; the box re-derives nothing.
type DHCPConfig struct {
	VPN      uint32 `json:"vpn"`
	VPP      string `json:"vpp"`
	LCP      string `json:"lcp"`
	Listen   string `json:"listen"`
	Prefix   uint8  `json:"prefix"`
	Start    string `json:"start"`
	End      string `json:"end"`
	LeaseSec int    `json:"lease_sec"`
	Router   string `json:"router"`
	DNS      string `json:"dns"`
}

// DNSConfig mirrors the services segment's dns rows.
type DNSConfig struct {
	VPN      uint32   `json:"vpn"`
	VPP      string   `json:"vpp"`
	LCP      string   `json:"lcp"`
	Listen   string   `json:"listen"`
	Prefix   uint8    `json:"prefix"`
	Upstream []string `json:"upstream"`
}

type svcConfig struct {
	DHCP []DHCPConfig `json:"dhcp"`
	DNS  []DNSConfig  `json:"dns"`
}

// lease is one DHCP lease. Keyed by client MAC: a MAC is the identity a client
// keeps across reboots, and one lease per MAC is what a small branch pool wants.
type lease struct {
	IP       string `json:"ip"`
	MAC      string `json:"mac"`
	Hostname string `json:"hostname,omitempty"`
	VPN      uint32 `json:"vpn"`
	VPP      string `json:"vpp"`
	Expires  int64  `json:"expires_unix"`
}

type server struct {
	cfg     svcConfig
	leases  map[string]*lease // MAC -> lease
	leaseMu sync.Mutex

	// counters, only for the status page
	mu       sync.Mutex
	dhcpReq  uint64
	dhcpAck  uint64
	dnsQuery uint64
	dnsReply uint64
	dnsFail  uint64

	leasesPath string
	log        *log.Logger

	stopMu sync.Mutex
	stop   map[string]chan struct{}
}

func main() {
	conf := flag.String("config", "/etc/sdwan/svc.json", "svc 配置文件（sr-apply 从 bundle 第 8 段写出来）")
	leasePath := flag.String("leases", leasesFile, "租约持久化文件")
	flag.Parse()

	logger := log.New(os.Stdout, "sr-svc: ", log.LstdFlags|log.Lmicroseconds)
	s := &server{
		leases:     map[string]*lease{},
		leasesPath: *leasePath,
		log:        logger,
		stop:       map[string]chan struct{}{},
	}
	s.loadLeases()
	if err := s.loadConfig(*conf); err != nil {
		logger.Printf("配置读不了: %v", err)
		os.Exit(1)
	}

	// SIGHUP 重载：行变了就换 socket，租约表不动 —— 客户端在用的地址不能因为
	// 一次配置重载被回收。
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	go func() {
		for range sighup {
			logger.Printf("SIGHUP: 重载 %s", *conf)
			if err := s.loadConfig(*conf); err != nil {
				logger.Printf("重载失败（继续用旧配置）: %v", err)
			}
		}
	}()
	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigterm
		logger.Printf("收到退出信号，保存租约")
		s.saveLeases()
		os.Exit(0)
	}()

	go s.expireLoop()
	go func() {
		mux := httpStatusMux(s)
		if err := serveStatus(mux); err != nil {
			logger.Printf("状态口退出: %v", err)
		}
	}()

	logger.Printf("在跑：dhcp %d 行，dns %d 行（状态口 %s）",
		len(s.cfg.DHCP), len(s.cfg.DNS), statusAddr)
	select {} // 常驻；行的工作在 loadConfig 起的 goroutine 里
}

// loadConfig swaps the running rows. Every restart of a row is a fresh socket:
// the old one is closed so the new bind never races.
func (s *server) loadConfig(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读 %s: %w", path, err)
	}
	var cfg svcConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("解析 %s: %w", path, err)
	}
	if cfg.DHCP == nil {
		cfg.DHCP = []DHCPConfig{}
	}
	if cfg.DNS == nil {
		cfg.DNS = []DNSConfig{}
	}

	s.stopMu.Lock()
	for key, ch := range s.stop {
		close(ch)
		delete(s.stop, key)
	}
	s.stopMu.Unlock()

	s.cfg = cfg
	for i := range cfg.DHCP {
		row := cfg.DHCP[i]
		ch := make(chan struct{})
		s.stopMu.Lock()
		s.stop["dhcp:"+row.VPP] = ch
		s.stopMu.Unlock()
		go s.serveDHCP(row, ch)
	}
	for i := range cfg.DNS {
		row := cfg.DNS[i]
		ch := make(chan struct{})
		s.stopMu.Lock()
		s.stop["dns:"+row.VPP] = ch
		s.stopMu.Unlock()
		go s.serveDNS(row, ch)
	}
	s.log.Printf("行已装载：dhcp %d 行，dns %d 行", len(cfg.DHCP), len(cfg.DNS))
	return nil
}

// bindToDevice UDP socket bound to one kernel netdev. SO_BINDTODEVICE is what
// keeps rows on the same box from hearing each other's broadcast segments, and
// what lets two rows both own :67/:53 (the TAP, not the address, is the scope).
func (s *server) bindToDevice(network, laddr, dev string) (*net.UDPConn, error) {
	addr, err := net.ResolveUDPAddr(network, laddr)
	if err != nil {
		return nil, fmt.Errorf("解析 %s: %w", laddr, err)
	}
	conn, err := net.ListenUDP(network, addr)
	if err != nil {
		return nil, err
	}
	if dev != "" {
		// SO_BINDTODEVICE = 25 on Linux. The name must be NUL-terminated;
		// net.UDPConn has no SetsockoptString wrapper, so the raw call goes
		// through the fd.
		raw, rerr := conn.SyscallConn()
		if rerr != nil {
			conn.Close()
			return nil, fmt.Errorf("拿 %s 的 fd: %w", dev, rerr)
		}
		var serr error
		if cerr := raw.Control(func(fd uintptr) {
			serr = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, 25, dev+"\x00")
		}); cerr != nil {
			conn.Close()
			return nil, cerr
		}
		if serr != nil {
			conn.Close()
			return nil, fmt.Errorf("SO_BINDTODEVICE %s: %w", dev, serr)
		}
	}
	return conn, nil
}

// ---------------- DHCP ----------------

// serveDHCP is one row's :67 listener. DHCP is UDP broadcast in, broadcast (or
// unicast) out; both directions were measured to cross the TAP.
func (s *server) serveDHCP(row DHCPConfig, stop chan struct{}) {
	conn, err := s.bindToDevice("udp4", fmt.Sprintf(":%d", dhcpServerPort), row.LCP)
	if err != nil {
		s.log.Printf("dhcp[%s] 绑不了 :67（%s）: %v —— 该行不服务", row.VPP, row.LCP, err)
		return
	}
	// OFFER/NAK 的目的地是 255.255.255.255：内核要求 socket 显式带 SO_BROADCAST
	// 才肯发受限广播，没有它 WriteToUDP 直接 EACCES。
	raw, rerr := conn.SyscallConn()
	if rerr == nil {
		_ = raw.Control(func(fd uintptr) {
			_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
		})
	}
	defer conn.Close()
	s.log.Printf("dhcp[%s] 监听 %s:67 (tap %s, 池 %s-%s)", row.VPP, row.Listen, row.LCP, row.Start, row.End)

	buf := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-stop:
				return
			default:
			}
			if isNetClosed(err) {
				return
			}
			s.log.Printf("dhcp[%s] 读包: %v", row.VPP, err)
			continue
		}
		select {
		case <-stop:
			return
		default:
		}
		pkt := parseDHCP(buf[:n])
		if pkt == nil {
			continue
		}
		s.bump(&s.dhcpReq)
		s.log.Printf("dhcp[%s] 收到包 %d 字节 xid=%08x msgType=%v chaddr=%s",
			row.VPP, n, pkt.xid, pkt.options[53], macString(pkt.chaddr))
		if resp := s.handleDHCP(row, pkt); resp != nil {
			out, dst := resp.encode(row)
			if out == nil {
				continue
			}
			raddr := &net.UDPAddr{IP: net.ParseIP(dst), Port: dhcpClientPort}
			if raddr.IP == nil {
				s.log.Printf("dhcp[%s] 回包地址解析失败 %q", row.VPP, dst)
				continue
			}
			// 源地址用客户口自己的地址（dhcp option 54 的值与它一致），出方向
			// 的路由由 VPP 从同一 TAP 送回客户。
			if _, werr := conn.WriteToUDP(out, &net.UDPAddr{IP: raddr.IP, Port: dhcpClientPort}); werr != nil {
				s.log.Printf("dhcp[%s] 回包: %v", row.VPP, werr)
				continue
			}
			s.log.Printf("dhcp[%s] 回包 %d 字节 → %s:%d", row.VPP, len(out), dst, dhcpClientPort)
			s.bump(&s.dhcpAck)
		} else {
			s.log.Printf("dhcp[%s] %s: 不响应（msgType=%d，池满/不支持的消息类型）",
				row.VPP, macString(pkt.chaddr), pkt.options[53])
		}
	}
}

// dhcpPacket is the subset of BOOTP/DHCPv4 sr-svc reads and writes. The magic
// cookie and option parsing accept only what a client needs to obtain an
// address: everything else passes through untouched.
type dhcpPacket struct {
	op      byte
	xid     uint32
	chaddr  []byte // 6 bytes
	ciaddr  net.IP
	flags   uint16
	options map[byte][]byte
	raw     []byte
}

func parseDHCP(b []byte) *dhcpPacket {
	// BOOTP fixed part: op(1) htype(1) hlen(1) hops(1) xid(4) secs(2) flags(2)
	// ciaddr(4) yiaddr(4) siaddr(4) giaddr(4) chaddr(16) sname(64) file(128)
	// magic(4) = 240.
	if len(b) < 240 {
		return nil
	}
	if b[0] != 1 { // BOOTREQUEST
		return nil
	}
	magic := []byte{0x63, 0x82, 0x53, 0x63}
	if string(b[236:240]) != string(magic) {
		return nil
	}
	p := &dhcpPacket{
		op:      b[0],
		xid:     binary.BigEndian.Uint32(b[4:8]),
		flags:   binary.BigEndian.Uint16(b[10:12]),
		ciaddr:  net.IP(b[12:16]),
		chaddr:  append([]byte(nil), b[28:34]...),
		options: map[byte][]byte{},
		raw:     append([]byte(nil), b...),
	}
	i := 240
	for i < len(b) {
		o := b[i]
		if o == 0 { // pad
			i++
			continue
		}
		if o == 255 { // end
			break
		}
		if i+1 >= len(b) {
			break
		}
		l := int(b[i+1])
		if i+2+l > len(b) {
			break
		}
		p.options[o] = append([]byte(nil), b[i+2:i+2+l]...)
		i += 2 + l
	}
	return p
}

// optMsg types this server acts on.
const (
	dhcpDiscover = 1
	dhcpOffer    = 2
	dhcpRequest  = 3
	dhcpAck      = 5
	dhcpNak      = 6
	dhcpRelease  = 7
)

func (s *server) handleDHCP(row DHCPConfig, p *dhcpPacket) *dhcpReply {
	msgType := byte(0)
	if m, ok := p.options[53]; ok && len(m) == 1 {
		msgType = m[0]
	}
	mac := macString(p.chaddr)
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()

	switch msgType {
	case dhcpDiscover:
		ip := s.allocateLocked(row, p, false)
		if ip == "" {
			s.log.Printf("dhcp[%s] %s: 池满了，DISCOVER 无法响应", row.VPP, mac)
			return nil
		}
		return &dhcpReply{row: row, req: p, msgType: dhcpOffer, yiaddr: ip}
	case dhcpRequest:
		// REQUEST 里的 option 50（客户端想要的地址）优先；没有就按 MAC 的现有租约。
		ip := ""
		if req, ok := p.options[50]; ok && len(req) == 4 {
			ip = net.IP(req).String()
		}
		if ip == "" {
			if l := s.leases[mac]; l != nil && l.VPP == row.VPP {
				ip = l.IP
			}
		}
		if ip == "" {
			ip = s.allocateLocked(row, p, false)
		}
		if !s.inPoolLocked(row, ip) || s.ipTakenLocked(mac, row.VPP, ip) {
			s.log.Printf("dhcp[%s] %s: 请求 %s 不在池内或已被占用，NAK", row.VPP, mac, ip)
			return &dhcpReply{row: row, req: p, msgType: dhcpNak}
		}
		s.commitLocked(row, mac, ip, p)
		return &dhcpReply{row: row, req: p, msgType: dhcpAck, yiaddr: ip}
	case dhcpRelease:
		if l := s.leases[mac]; l != nil {
			s.log.Printf("dhcp[%s] %s 释放 %s", row.VPP, mac, l.IP)
			delete(s.leases, mac)
			s.saveLeases()
		}
		return nil
	}
	return nil
}

// allocateLocked picks the lowest free address in the pool. A client's existing
// lease wins first (it must keep its address across DISCOVER), then an address
// the client explicitly asks for (option 50), then the first unleased address.
func (s *server) allocateLocked(row DHCPConfig, p *dhcpPacket, _ bool) string {
	mac := macString(p.chaddr)
	if l := s.leases[mac]; l != nil && l.VPP == row.VPP && s.inPoolLocked(row, l.IP) {
		return l.IP
	}
	start := ipUint(net.ParseIP(row.Start).To4())
	end := ipUint(net.ParseIP(row.End).To4())
	if start == 0 || end == 0 || start > end {
		return ""
	}
	if req, ok := p.options[50]; ok && len(req) == 4 {
		want := net.IP(req).String()
		if s.inPoolLocked(row, want) && !s.ipTakenLocked(mac, row.VPP, want) {
			return want
		}
	}
	for v := start; v <= end; v++ {
		ip := uintIP(v).String()
		if !s.ipTakenLocked(mac, row.VPP, ip) {
			return ip
		}
	}
	return ""
}

// inPoolLocked reports whether ip lies in [Start, End] of the row's pool.
func (s *server) inPoolLocked(row DHCPConfig, ip string) bool {
	a := net.ParseIP(ip)
	if a == nil {
		return false
	}
	v := ipUint(a.To4())
	start := ipUint(net.ParseIP(row.Start).To4())
	end := ipUint(net.ParseIP(row.End).To4())
	return v >= start && v <= end
}

// ipTakenLocked reports whether ip is leased to a DIFFERENT mac on this port.
func (s *server) ipTakenLocked(mac, vpp, ip string) bool {
	now := time.Now().Unix()
	for m, l := range s.leases {
		if l.VPP == vpp && l.IP == ip && m != mac && l.Expires > now {
			return true
		}
	}
	return false
}

func (s *server) commitLocked(row DHCPConfig, mac, ip string, p *dhcpPacket) {
	now := time.Now().Unix()
	host := ""
	if h, ok := p.options[12]; ok {
		host = string(h)
	}
	l := &lease{IP: ip, MAC: mac, Hostname: host, VPN: row.VPN, VPP: row.VPP,
		Expires: now + int64(row.LeaseSec)}
	prev := s.leases[mac]
	if prev == nil || prev.IP != ip {
		s.log.Printf("dhcp[%s] %s (%s) 获得租约 %s，%d 秒", row.VPP, mac, host, ip, row.LeaseSec)
	}
	s.leases[mac] = l
	s.saveLeases()
}

// dhcpReply is the server's answer to one packet. Encoding writes the BOOTP
// fixed part and a minimal option set: 53 (type), 54 (server id = listen),
// 51 (lease), 1 (mask), 3 (router), 6 (dns). Broadcast replies go to
// 255.255.255.255 when the client set the broadcast bit (flags bit 15) or its
// ciaddr is unset; otherwise they go to the client's address.
type dhcpReply struct {
	row      DHCPConfig
	req      *dhcpPacket
	msgType  byte
	yiaddr   string
}

func (r *dhcpReply) encode(row DHCPConfig) ([]byte, string) {
	out := make([]byte, 240)
	out[0] = 2 // BOOTREPLY
	out[1], out[2] = 1, 6
	binary.BigEndian.PutUint32(out[4:8], r.req.xid)
	copy(out[28:34], r.req.chaddr)
	if r.yiaddr != "" {
		copy(out[16:20], net.ParseIP(r.yiaddr).To4())
	}
	copy(out[236:240], []byte{0x63, 0x82, 0x53, 0x63})

	var opts []byte
	addOpt := func(code byte, val []byte) {
		opts = append(opts, code, byte(len(val)))
		opts = append(opts, val...)
	}
	addOpt(53, []byte{r.msgType})
	if r.msgType == dhcpOffer || r.msgType == dhcpAck {
		addOpt(54, net.ParseIP(row.Listen).To4())
		lease := make([]byte, 4)
		binary.BigEndian.PutUint32(lease, uint32(row.LeaseSec))
		addOpt(51, lease)
		if mask := prefixMask(row.Prefix); mask != nil {
			addOpt(1, mask)
		}
		if row.Router != "" {
			addOpt(3, net.ParseIP(row.Router).To4())
		}
		if row.DNS != "" {
			addOpt(6, net.ParseIP(row.DNS).To4())
		}
	}
	out = append(out, opts...)
	out = append(out, 255)

	dst := "255.255.255.255"
	if r.req.flags&0x8000 == 0 && !r.req.ciaddr.IsUnspecified() {
		dst = r.req.ciaddr.String()
	}
	return out, dst
}

func prefixMask(p uint8) net.IP {
	if p == 0 || p > 32 {
		return nil
	}
	m := make(net.IP, 4)
	for i := 0; i < 4; i++ {
		bit := uint8(0)
		for b := 0; b < 8; b++ {
			if p > 0 {
				bit |= 1 << (7 - b)
				p--
			}
		}
		m[i] = byte(bit)
	}
	return m
}

func macString(c []byte) string {
	hex := "0123456789abcdef"
	out := make([]byte, 0, 17)
	for i, b := range c {
		if i > 0 {
			out = append(out, ':')
		}
		out = append(out, hex[b>>4], hex[b&0xf])
	}
	return string(out)
}

func ipUint(ip net.IP) uint32 {
	if ip == nil {
		return 0
	}
	return binary.BigEndian.Uint32(ip)
}

func uintIP(v uint32) net.IP {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return net.IP(b)
}

// ---------------- DNS forwarder ----------------

// serveDNS is one row's :53 listener. Each query is forwarded verbatim to the
// row's upstreams in order; the first reply with a matching transaction ID wins.
// No cache: a branch forwarder's job is correctness first, and upstreams here
// are the tenant's own resolvers.
func (s *server) serveDNS(row DNSConfig, stop chan struct{}) {
	conn, err := s.bindToDevice("udp4", net.JoinHostPort(row.Listen, "53"), row.LCP)
	if err != nil {
		s.log.Printf("dns[%s] 绑不了 %s:53（%s）: %v —— 该行不服务", row.VPP, row.Listen, row.LCP, err)
		return
	}
	defer conn.Close()
	s.log.Printf("dns[%s] 监听 %s:53 (tap %s, 上游 %v)", row.VPP, row.Listen, row.LCP, row.Upstream)

	buf := make([]byte, 4096)
	for {
		n, raddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-stop:
				return
			default:
			}
			if isNetClosed(err) {
				return
			}
			s.log.Printf("dns[%s] 读包: %v", row.VPP, err)
			continue
		}
		select {
		case <-stop:
			return
		default:
		}
		q := buf[:n]
		s.bump(&s.dnsQuery)
		go func(q []byte, raddr *net.UDPAddr) {
			if reply := s.forwardDNS(row, q); reply != nil {
				if _, werr := conn.WriteToUDP(reply, raddr); werr == nil {
					s.bump(&s.dnsReply)
				}
				return
			}
			s.bump(&s.dnsFail)
			if resp := servfail(q); resp != nil {
				_, _ = conn.WriteToUDP(resp, raddr)
			}
		}(append([]byte(nil), q...), raddr)
	}
}

// forwardDNS sends q to each upstream in order and returns the first reply
// carrying the same transaction ID. A short timeout per upstream keeps a dead
// first resolver from stalling the client longer than one query should.
func (s *server) forwardDNS(row DNSConfig, q []byte) []byte {
	if len(q) < 12 || len(row.Upstream) == 0 {
		return nil
	}
	xid := binary.BigEndian.Uint16(q[0:2])
	for _, up := range row.Upstream {
		raddr := &net.UDPAddr{IP: net.ParseIP(up), Port: 53}
		conn, err := net.DialUDP("udp4", nil, raddr)
		if err != nil {
			continue
		}
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := conn.Write(q); err != nil {
			conn.Close()
			continue
		}
		buf := make([]byte, 4096)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				break
			}
			if n >= 12 && binary.BigEndian.Uint16(buf[0:2]) == xid {
				conn.Close()
				return append([]byte(nil), buf[:n]...)
			}
		}
		conn.Close()
	}
	return nil
}

// servfail builds a minimal DNS response header for an unanswerable query.
func servfail(q []byte) []byte {
	if len(q) < 12 {
		return nil
	}
	out := append([]byte(nil), q[0:2]...)
	out = append(out, 0x81, 0x82) // QR=1, RD from query, RCODE=2 (SERVFAIL)
	out = append(out, q[4:6]...)  // QDCOUNT echoed
	out = append(out, 0, 0, 0, 0) // ANCOUNT
	out = append(out, 0, 0)       // NSCOUNT
	out = append(out, 0, 0)       // ARCOUNT
	out = append(out, q[12:]...)  // question echoed
	return out
}

// ---------------- leases / counters / status ----------------

func (s *server) loadLeases() {
	raw, err := os.ReadFile(s.leasesPath)
	if err != nil {
		return
	}
	var ls []*lease
	if err := json.Unmarshal(raw, &ls); err != nil {
		s.log.Printf("租约文件读不了（当作空表）: %v", err)
		return
	}
	now := time.Now().Unix()
	for _, l := range ls {
		if l.Expires > now {
			s.leases[l.MAC] = l
		}
	}
	s.log.Printf("租约载入 %d 条（过期 %d 条丢弃）", len(s.leases), len(ls)-len(s.leases))
}

// saveLeases persists the lease table. Callers hold leaseMu.
func (s *server) saveLeases() {
	ls := make([]*lease, 0, len(s.leases))
	for _, l := range s.leases {
		ls = append(ls, l)
	}
	if err := os.MkdirAll(filepath.Dir(s.leasesPath), 0o755); err != nil {
		s.log.Printf("租约目录建不了: %v", err)
		return
	}
	data, err := json.MarshalIndent(ls, "", "  ")
	if err != nil {
		return
	}
	tmp := s.leasesPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		s.log.Printf("租约写不了: %v", err)
		return
	}
	os.Rename(tmp, s.leasesPath)
}

func (s *server) expireLoop() {
	for {
		time.Sleep(time.Minute)
		now := time.Now().Unix()
		s.leaseMu.Lock()
		dropped := 0
		for m, l := range s.leases {
			if l.Expires <= now {
				s.log.Printf("租约过期：[%s] %s %s", l.VPP, m, l.IP)
				delete(s.leases, m)
				dropped++
			}
		}
		if dropped > 0 {
			s.saveLeases()
		}
		s.leaseMu.Unlock()
	}
}

func (s *server) bump(c *uint64) {
	s.mu.Lock()
	*c++
	s.mu.Unlock()
}

type statusView struct {
	DHCPRows int                 `json:"dhcp_rows"`
	DNSRows  int                 `json:"dns_rows"`
	DHCPReq  uint64              `json:"dhcp_requests"`
	DHCPAck  uint64              `json:"dhcp_acks"`
	DNSQuery uint64              `json:"dns_queries"`
	DNSReply uint64              `json:"dns_replies"`
	DNSFail  uint64              `json:"dns_failures"`
	Leases   []*lease            `json:"leases"`
}

func isNetClosed(err error) bool {
	return err != nil && (err == net.ErrClosed || fmt.Sprint(err) == "use of closed network connection")
}
