//go:build linux

package krt

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"net"
	"net/netip"

	"sdwan/controller/internal/frr"
)

// netlink 常量取自 linux/rtnetlink.h。netlink 的头部字段是**主机字节序**
// （不是网络字节序）：写反了内核会当成另一个命令，或者干脆静默丢弃。
const (
	rtmNewRoute = 24
	rtmDelRoute = 25
	rtmGetRoute = 26

	nlmsgError = 2
	nlmsgDone  = 3

	nlmFRequest = 0x01
	nlmFAck     = 0x04
	nlmFExcl    = 0x200
	nlmFReplace = 0x100
	nlmFCreate  = 0x400
	nlmFDump    = 0x300

	rtProtBoot  = 3 // RTPROT_BOOT：ip route add 的默认协议，zebra 认成 K
	rtScopeLink = 253
	rtnUnicast  = 1

	rtaDst   = 1
	rtaOif   = 4
	rtaTable = 15

	iflaIfname = 3

	afInet = 2

	ackTimeout  = 5 * time.Second
	recvTimeout = 500 * time.Millisecond
)

// Client 是一条到内核的 NETLINK_ROUTE socket。请求串行化：一条 socket 上
// 用递增的 seq 对回执，Exporter 本身也是单 goroutine 调用。
type Client struct {
	mu  sync.Mutex
	fd  int
	seq uint32
}

func New() (*Client, error) {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_ROUTE)
	if err != nil {
		return nil, fmt.Errorf("krt: 开 netlink socket: %w", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("krt: bind netlink: %w", err)
	}
	return &Client{fd: fd}, nil
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fd < 0 {
		return nil
	}
	err := syscall.Close(c.fd)
	c.fd = -1
	return err
}

// WatchLinks 订阅链路/地址事件，只把「破坏性」的几种发到返回的 channel：
// 接口删除、接口 down、地址删除。用一条独立 socket：请求/回执那条 socket 按
// seq 对账，异步通知混进它的读循环会把回执认错。
//
// 为什么关心：lab 实测（2026-11-02）接口 down、删接口、删地址都会让 zebra 把
// 自己 RIB 里的 K 路由**整表**清掉（内核里还在、redistribute kernel 却不再
// 宣告），而 zebra 对重复 add 不发通知，只有 delete+add 能让它重新学到。
func WatchLinks(ctx context.Context) (<-chan LinkEvent, error) {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_ROUTE)
	if err != nil {
		return nil, fmt.Errorf("krt: 开链路监听 socket: %w", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK, Groups: rtnlGroups()}); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("krt: bind 链路监听: %w", err)
	}
	tv := syscall.NsecToTimeval(recvTimeout.Nanoseconds())
	_ = syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)

	out := make(chan LinkEvent, 16)
	go func() {
		defer syscall.Close(fd)
		defer close(out)
		buf := make([]byte, 32*1024)
		for ctx.Err() == nil {
			n, _, err := syscall.Recvfrom(fd, buf, 0)
			if err != nil {
				if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EINTR) {
					continue
				}
				return
			}
			msgs, err := syscall.ParseNetlinkMessage(buf[:n])
			if err != nil {
				continue
			}
			for _, m := range msgs {
				ev, ok := parseLinkEvent(m)
				if !ok {
					continue
				}
				select {
				case out <- ev:
				default: // 队列满说明已经有事件在等处理，多丢一个不误事
				}
			}
		}
	}()
	return out, nil
}

// parseLinkEvent 从一条 netlink 消息里认出三种破坏性事件。NEWLINK 是
// 「状态变化」：造一个 down 的接口也会来一条，但那种不触发 zebra 清表，
// 多认一次只是多重放一遍 K 路由，代价可接受；漏认 down 的代价是导出静默停掉。
func parseLinkEvent(m syscall.NetlinkMessage) (LinkEvent, bool) {
	switch m.Header.Type {
	case syscall.RTM_DELLINK:
		return LinkEvent{Name: ifName(&m), Kind: LinkDeleted}, true
	case syscall.RTM_NEWLINK:
		if len(m.Data) < syscall.SizeofIfInfomsg {
			return LinkEvent{}, false
		}
		flags := binary.NativeEndian.Uint32(m.Data[8:12])
		if flags&syscall.IFF_UP != 0 {
			return LinkEvent{}, false
		}
		return LinkEvent{Name: ifName(&m), Kind: LinkDown}, true
	case syscall.RTM_DELADDR:
		if len(m.Data) < syscall.SizeofIfAddrmsg {
			return LinkEvent{}, false
		}
		idx := int(binary.NativeEndian.Uint32(m.Data[4:8]))
		return LinkEvent{Name: ifaceName(idx), Kind: AddrDeleted}, true
	}
	return LinkEvent{}, false
}

// rtnlGroups：RTNLGRP_LINK = 1、RTNLGRP_IPV4_IFADDR = 5；Groups 是位图，
// 组号 n 对应第 n-1 位。
func rtnlGroups() uint32 {
	return 1<<(1-1) | 1<<(5-1)
}

func ifaceName(index int) string {
	ifi, err := net.InterfaceByIndex(index)
	if err != nil {
		return ""
	}
	return ifi.Name
}

func ifName(m *syscall.NetlinkMessage) string {
	attrs, err := syscall.ParseNetlinkRouteAttr(m)
	if err != nil {
		return ""
	}
	for _, a := range attrs {
		if a.Attr.Type == iflaIfname {
			return strings.TrimRight(string(a.Value), "\x00")
		}
	}
	return ""
}

// RouteAdd 只建不覆盖（NLM_F_CREATE|NLM_F_EXCL）：同前缀已经在表里就回
// ErrRouteExists，由 Exporter 决定是收编（我们自己的残留）还是跳过（别人的）。
// 绝不用 replace —— 那会把盒子的 connected 路由换成 dev lo（2026-11-02 实测
// 把 mgmt 网段顶掉，盒子当场连不上 RR）。
func (c *Client) RouteAdd(ctx context.Context, r frr.Route) error {
	err := c.request(ctx, rtmNewRoute, nlmFRequest|nlmFAck|nlmFCreate|nlmFExcl, r)
	if errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("%w: %s（表 %d）", frr.ErrRouteExists, r.Prefix, r.Table)
	}
	return err
}

// RouteReplace 只用于我们自己写过的前缀（出口 ifindex 变了）。同前缀的键
// 会被原地替换，所以调用方必须先确认那条是我们的（Exporter 用 prev 保证）。
func (c *Client) RouteReplace(ctx context.Context, r frr.Route) error {
	return c.request(ctx, rtmNewRoute, nlmFRequest|nlmFAck|nlmFCreate|nlmFReplace, r)
}

// RouteDelete 把 ESRCH 当成功：内核里已经没有了（上一次终于删掉、或者整个
// netns 换了），对账要能继续往下走。删除消息带出口 ifindex，内核按
// （前缀, 表, 出口）匹配，不会误删挂在别的接口上的同前缀路由。
func (c *Client) RouteDelete(ctx context.Context, r frr.Route) error {
	err := c.request(ctx, rtmDelRoute, nlmFRequest|nlmFAck, r)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// Owns 判断表里这条前缀是不是我们写的：proto=RTPROT_BOOT、scope=LINK、
// type=unicast，而且出口 ifindex 与计划一致。只在 RouteAdd 报 EEXIST 时调用
// （罕见），平时不做全表扫描。
func (c *Client) Owns(ctx context.Context, r frr.Route) (bool, error) {
	es, err := c.dump(ctx, r.Table)
	if err != nil {
		return false, err
	}
	for _, e := range es {
		if e.Prefix != r.Prefix {
			continue
		}
		if e.Proto == rtProtBoot && e.Scope == rtScopeLink && e.Type == rtnUnicast && e.OIF == r.OIF {
			return true, nil
		}
	}
	return false, nil
}

// LocalSubnets 返回该表所属 VRF 里盒子自己配了地址的前缀。VPN 0（main）
// 看默认 VRF 的全部接口，VPN N 看 vpn<N> 设备与挂在它下面的接口。
// 这些前缀碰不得：K 与 connected 同前缀会把 connected 换成 dev lo，地址还在、
// 路由没了，盒子自己到这一段就断了。
func (c *Client) LocalSubnets(table uint32) map[netip.Prefix]bool {
	out := map[netip.Prefix]bool{}
	want := ""
	if table != 254 {
		want = fmt.Sprintf("vpn%d", table)
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifi := range ifaces {
		if want == "" {
			if masterOf(ifi.Name) != "" {
				continue
			}
		} else if ifi.Name != want && masterOf(ifi.Name) != want {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil {
				continue
			}
			ones, bits := ipn.Mask.Size()
			if bits != 32 {
				continue
			}
			addr := netip.AddrFrom4([4]byte{ip4[0], ip4[1], ip4[2], ip4[3]})
			out[netip.PrefixFrom(addr, ones).Masked()] = true
		}
	}
	return out
}

// masterOf 返回接口挂在哪个 VRF 设备下面（/sys/class/net/<if>/master），
// 默认 VRF 的接口返回空串。
func masterOf(name string) string {
	p, err := os.Readlink(filepath.Join("/sys/class/net", name, "master"))
	if err != nil {
		return ""
	}
	return filepath.Base(p)
}

func (c *Client) request(ctx context.Context, msgType, flags uint16, r frr.Route) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	msg, err := encodeRoute(msgType, flags, r)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fd < 0 {
		return errors.New("krt: 客户端已关闭")
	}
	c.seq++
	seq := c.seq
	binary.NativeEndian.PutUint32(msg[8:12], seq)

	if err := setDeadline(c.fd, ctx); err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		err := syscall.Sendto(c.fd, msg, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK})
		if err == nil {
			break
		}
		if errors.Is(err, syscall.EINTR) && attempt == 0 {
			continue
		}
		return fmt.Errorf("krt: 下发 %s 表 %d: %w", r.Prefix, r.Table, err)
	}
	return c.waitAck(ctx, seq, r)
}

func setDeadline(fd int, ctx context.Context) error {
	d := ackTimeout
	if t, ok := ctx.Deadline(); ok {
		if left := time.Until(t); left < d {
			d = left
		}
	}
	if d <= 0 {
		return context.DeadlineExceeded
	}
	tv := syscall.NsecToTimeval(d.Nanoseconds())
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv); err != nil {
		return fmt.Errorf("krt: 设置收包超时: %w", err)
	}
	return nil
}

// waitAck 读到与我们 seq 匹配的 NLMSG_ERROR 为止。errno 为 0 是成功回执；
// 非 0 时以 syscall.Errno 返回，让调用方用 errors.Is 判 ESRCH。
func (c *Client) waitAck(ctx context.Context, seq uint32, r frr.Route) error {
	buf := make([]byte, 8192)
	for {
		n, _, err := syscall.Recvfrom(c.fd, buf, 0)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
				if cerr := ctx.Err(); cerr != nil {
					return cerr
				}
				return fmt.Errorf("krt: %s 表 %d 的 netlink 回执超过 %s", r.Prefix, r.Table, ackTimeout)
			}
			return fmt.Errorf("krt: 读 netlink 回执: %w", err)
		}
		for off := 0; off+16 <= n; {
			mlen := int(binary.NativeEndian.Uint32(buf[off : off+4]))
			if mlen < 16 || off+mlen > n {
				return fmt.Errorf("krt: netlink 报文长度 %d 非法（收到 %d 字节）", mlen, n)
			}
			typ := binary.NativeEndian.Uint16(buf[off+4 : off+6])
			got := binary.NativeEndian.Uint32(buf[off+8 : off+12])
			if got != seq {
				// 不是这次请求的回执（netlink 上可能夹着别的消息），跳过。
				off += (mlen + 3) &^ 3
				continue
			}
			switch typ {
			case nlmsgError:
				if mlen < 20 {
					return errors.New("krt: NLMSG_ERROR 短于 20 字节")
				}
				errno := int32(binary.NativeEndian.Uint32(buf[off+16 : off+20]))
				if errno == 0 {
					return nil
				}
				return syscall.Errno(-errno)
			case nlmsgDone:
				return nil
			}
			off += (mlen + 3) &^ 3
		}
	}
}

// dumpEntry 是 RTM_GETROUTE dump 里一条路由的判断字段。
type dumpEntry struct {
	Prefix netip.Prefix
	Proto  uint8
	Scope  uint8
	Type   uint8
	OIF    int
}

// dump 拉一张表的全部 IPv4 路由。只用于 EEXIST 时查归属；正常对账不做全表
// 扫描（客户侧可能有几十万条路由）。
func (c *Client) dump(ctx context.Context, table uint32) ([]dumpEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	msg, err := encodeDump(table)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fd < 0 {
		return nil, errors.New("krt: 客户端已关闭")
	}
	c.seq++
	seq := c.seq
	binary.NativeEndian.PutUint32(msg[8:12], seq)
	if err := setDeadline(c.fd, ctx); err != nil {
		return nil, err
	}
	if err := syscall.Sendto(c.fd, msg, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		if errors.Is(err, syscall.EINTR) {
			return nil, err
		}
		return nil, fmt.Errorf("krt: 拉表 %d: %w", table, err)
	}

	var out []dumpEntry
	buf := make([]byte, 1<<16)
	for {
		n, _, err := syscall.Recvfrom(c.fd, buf, 0)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, fmt.Errorf("krt: 拉表 %d 超过 %s", table, ackTimeout)
			}
			return nil, fmt.Errorf("krt: 读表 %d 的 dump: %w", table, err)
		}
		for off := 0; off+16 <= n; {
			mlen := int(binary.NativeEndian.Uint32(buf[off : off+4]))
			if mlen < 16 || off+mlen > n {
				return nil, fmt.Errorf("krt: netlink 报文长度 %d 非法（收到 %d 字节）", mlen, n)
			}
			typ := binary.NativeEndian.Uint16(buf[off+4 : off+6])
			got := binary.NativeEndian.Uint32(buf[off+8 : off+12])
			if got != seq {
				off += (mlen + 3) &^ 3
				continue
			}
			switch typ {
			case nlmsgDone:
				return out, nil
			case nlmsgError:
				if mlen < 20 {
					return nil, errors.New("krt: NLMSG_ERROR 短于 20 字节")
				}
				errno := int32(binary.NativeEndian.Uint32(buf[off+16 : off+20]))
				if errno == 0 {
					return out, nil
				}
				return nil, syscall.Errno(-errno)
			case rtmNewRoute:
				if e, ok := parseDumpEntry(buf[off : off+mlen]); ok {
					out = append(out, e)
				}
			}
			off += (mlen + 3) &^ 3
		}
	}
}

// encodeDump 组一条 RTM_GETROUTE + NLM_F_DUMP：rtmsg 里给 family 与表号，
// 其它字段留空表示"不过这几种"。表号 1..255 用 rtm_table 一个字节即可。
func encodeDump(table uint32) ([]byte, error) {
	if table == 0 || table > 255 {
		return nil, fmt.Errorf("krt: 表号 %d 不在 1..255", table)
	}
	b := make([]byte, 16+12)
	binary.NativeEndian.PutUint32(b[0:4], uint32(len(b)))
	binary.NativeEndian.PutUint16(b[4:6], rtmGetRoute)
	binary.NativeEndian.PutUint16(b[6:8], nlmFRequest|nlmFDump)
	b[16] = afInet
	b[20] = uint8(table)
	return b, nil
}

// parseDumpEntry 解一条 RTM_NEWROUTE。属性区在 nlmsghdr(16)+rtmsg(12) 之后。
func parseDumpEntry(b []byte) (dumpEntry, bool) {
	if len(b) < 28 || b[16] != afInet {
		return dumpEntry{}, false
	}
	dstLen := int(b[17])
	table := uint32(b[20])
	e := dumpEntry{Proto: b[21], Scope: b[22], Type: b[23]}
	var dst netip.Addr
	attrs := b[28:]
	for len(attrs) >= 4 {
		alen := int(binary.NativeEndian.Uint16(attrs[0:2]))
		atyp := binary.NativeEndian.Uint16(attrs[2:4])
		if alen < 4 || alen > len(attrs) {
			break
		}
		payload := attrs[4:alen]
		switch atyp {
		case rtaDst:
			if len(payload) >= 4 {
				dst = netip.AddrFrom4([4]byte{payload[0], payload[1], payload[2], payload[3]})
			}
		case rtaOif:
			if len(payload) >= 4 {
				e.OIF = int(binary.NativeEndian.Uint32(payload[:4]))
			}
		case rtaTable:
			if len(payload) >= 4 {
				table = binary.NativeEndian.Uint32(payload[:4])
			}
		}
		attrs = attrs[(alen+3)&^3:]
	}
	if table > 255 || dstLen > 32 {
		return dumpEntry{}, false
	}
	if dstLen == 0 {
		dst = netip.AddrFrom4([4]byte{})
	}
	if !dst.IsValid() || !dst.Is4() {
		return dumpEntry{}, false
	}
	e.Prefix = netip.PrefixFrom(dst, dstLen).Masked()
	return e, true
}

// 表号只有 0..255，rtmsg.rtm_table 一个字节就装得下；RTA_TABLE 仍然带上，
// 与 iproute2 对 32 位表号的处理保持一致。
func encodeRoute(msgType, flags uint16, r frr.Route) ([]byte, error) {
	if !r.Prefix.IsValid() {
		return nil, fmt.Errorf("krt: 前缀无效: %s", r.Prefix)
	}
	addr := r.Prefix.Addr()
	if !addr.Is4() || addr.Is4In6() {
		return nil, fmt.Errorf("krt: 只支持 IPv4 前缀，收到 %s", r.Prefix)
	}
	if r.Prefix != r.Prefix.Masked() {
		return nil, fmt.Errorf("krt: 前缀未规范（掩码外有有效位）: %s", r.Prefix)
	}
	if r.Table == 0 || r.Table > 255 {
		return nil, fmt.Errorf("krt: 表号 %d 不在 1..255", r.Table)
	}
	if r.OIF <= 0 {
		return nil, fmt.Errorf("krt: %s 的出口 ifindex %d 非法", r.Prefix, r.OIF)
	}

	var attrs []byte
	a4 := addr.As4()
	attrs = appendAttr(attrs, rtaDst, a4[:])
	attrs = appendAttr(attrs, rtaOif, u32(uint32(r.OIF)))
	attrs = appendAttr(attrs, rtaTable, u32(r.Table))

	// nlmsghdr(16) + rtmsg(12)
	total := 16 + 12 + len(attrs)
	b := make([]byte, total)
	binary.NativeEndian.PutUint32(b[0:4], uint32(total))
	binary.NativeEndian.PutUint16(b[4:6], msgType)
	binary.NativeEndian.PutUint16(b[6:8], flags)
	b[16] = afInet
	b[17] = uint8(r.Prefix.Bits())
	b[18] = 0 // rtm_src_len
	b[19] = 0 // rtm_tos
	b[20] = uint8(r.Table)
	b[21] = rtProtBoot
	b[22] = rtScopeLink
	b[23] = rtnUnicast
	// rtm_flags = 0（b[24:28] 已经是 0）
	copy(b[28:], attrs)
	return b, nil
}

func u32(v uint32) []byte {
	var b [4]byte
	binary.NativeEndian.PutUint32(b[:], v)
	return b[:]
}

// appendAttr 追加一个 rtattr：rta_len 不含对齐填充，整条属性按 4 字节对齐。
func appendAttr(b []byte, typ uint16, payload []byte) []byte {
	var hdr [4]byte
	binary.NativeEndian.PutUint16(hdr[0:2], uint16(4+len(payload)))
	binary.NativeEndian.PutUint16(hdr[2:4], typ)
	b = append(b, hdr[:]...)
	b = append(b, payload...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

var _ frr.DP = (*Client)(nil)
