//go:build linux

package krt

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"syscall"
	"time"

	"sdwan/controller/internal/frr"
)

// netlink 常量取自 linux/rtnetlink.h。netlink 的头部字段是**主机字节序**
// （不是网络字节序）：写反了内核会当成另一个命令，或者干脆静默丢弃。
const (
	rtmNewRoute = 24
	rtmDelRoute = 25

	nlmsgError = 2
	nlmsgDone  = 3

	nlmFRequest = 0x01
	nlmFAck     = 0x04
	nlmFReplace = 0x100
	nlmFCreate  = 0x400

	rtProtBoot  = 3 // RTPROT_BOOT：ip route add 的默认协议，zebra 认成 K
	rtScopeLink = 253
	rtnUnicast  = 1

	rtaDst   = 1
	rtaOif   = 4
	rtaTable = 15

	afInet = 2

	ackTimeout = 5 * time.Second
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

// RouteAdd 用 replace 语义（NLM_F_CREATE|NLM_F_REPLACE）：Exporter 只在
// 计划变化时调用，重复下发同一条不该报 EEXIST；出口 ifindex 变了也由这一次
// 替换完成。
func (c *Client) RouteAdd(ctx context.Context, r frr.Route) error {
	return c.request(ctx, rtmNewRoute, nlmFRequest|nlmFAck|nlmFCreate|nlmFReplace, r)
}

// RouteDelete 把 ESRCH 当成功：内核里已经没有了（上一次终于删掉、或者整个
// netns 换了），对账要能继续往下走。
func (c *Client) RouteDelete(ctx context.Context, r frr.Route) error {
	err := c.request(ctx, rtmDelRoute, nlmFRequest|nlmFAck, r)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
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

// encodeRoute 组一条 RTM_NEWROUTE/RTM_DELROUTE：rtmsg + RTA_DST/RTA_OIF/RTA_TABLE。
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
