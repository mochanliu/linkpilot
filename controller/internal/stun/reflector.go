package stun

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"time"
)

// Reflector 是一个零依赖的 RFC 5389 应答器：一条内核 UDP 通道，收到
// Binding Request 就拿**这条数据报的真实源地址**回一条 Binding Success。
//
// 它是 vBOND 的"第三方证人"：设备自己说自己从哪儿能被看到不算数，必须有一个
// 不在它前面的观测点来回答。所以这里唯一的取值来源是 addr（ReadFrom 的第二个
// 返回值），报文里的任何字段都不参与 —— 见 IsBindingRequest 的说明。
//
// 它不认证任何人：入网身份由 /v1/mapping 那条 mTLS 通道负责，反射器只回答
// "我看见这个包从这个源地址来"。一个未入网的设备从这里能问到的只有它自己的
// 公网映射，那是它自己发包就能得到的事实，不构成任何授权。
type Reflector struct {
	requests  atomic.Uint64
	replies   atomic.Uint64
	malformed atomic.Uint64
	// v6 单独计数：IPv6 源地址回答不了（Response 只编 IPv4），必须看得见，
	// 否则一台纯 v6 的盒子会表现为"探测超时"而不是"这台盒子暂不支持"。
	v6 atomic.Uint64

	// readGap 是每次读之间的唤醒间隔：ctx 取消要能被观察到，而 ReadFrom 是阻塞的。
	readGap time.Duration
}

// Serve 在 conn 上应答，直到 ctx 结束或读出一个不可恢复的错误。它不拥有 conn，
// 调用方负责 Close。
func (r *Reflector) Serve(ctx context.Context, conn net.PacketConn) error {
	if r == nil || conn == nil {
		return fmt.Errorf("stun: Serve needs both a reflector and a conn")
	}
	gap := r.readGap
	if gap <= 0 {
		gap = 500 * time.Millisecond
	}
	// 取消时把读超时推到过去，让阻塞中的 ReadFrom 立刻返回 —— 不这么做的话，
	// 一个没有请求进来的反射器会让整个控制器退不出去。
	go func() {
		<-ctx.Done()
		_ = conn.SetReadDeadline(time.Now())
	}()

	buf := make([]byte, 2048)
	for {
		if err := conn.SetReadDeadline(time.Now().Add(gap)); err != nil {
			return err
		}
		n, addr, err := conn.ReadFrom(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				continue // 这一段没人探测，接着等
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if !IsBindingRequest(buf[:n]) {
			r.malformed.Add(1)
			continue
		}
		r.requests.Add(1)
		ip, port, ok := splitSource(addr)
		if !ok {
			r.v6.Add(1)
			continue
		}
		resp, err := Response(txIDOf(buf), ip, port)
		if err != nil {
			r.malformed.Add(1)
			continue
		}
		if _, err := conn.WriteTo(resp, addr); err != nil {
			return err
		}
		r.replies.Add(1)
	}
}

// Stats 是反射器的自述，给控制口和人看的一句话状态用。
func (r *Reflector) Stats() (requests, replies, malformed, v6 uint64) {
	if r == nil {
		return 0, 0, 0, 0
	}
	return r.requests.Load(), r.replies.Load(), r.malformed.Load(), r.v6.Load()
}

// splitSource 把 ReadFrom 的地址拆成 (ipv4, port)。非 IPv4 一律 ok=false：
// Response 编不出 IPv6，而"编不出来"必须是一个看得见的计数，不是静默丢弃。
func splitSource(addr net.Addr) (string, int, bool) {
	udp, ok := addr.(*net.UDPAddr)
	if !ok || udp == nil {
		return "", 0, false
	}
	v4 := udp.IP.To4()
	if v4 == nil {
		return "", 0, false
	}
	return v4.String(), udp.Port, true
}

// txIDOf 复制事务 id：绝不把请求缓冲区本身交出去，反射器会接着复用它。
func txIDOf(pkt []byte) []byte {
	out := make([]byte, 12)
	copy(out, pkt[8:20])
	return out
}
