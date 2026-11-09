// 异步事件订阅。这一整个文件的存在理由是一条实测事实：
//
// VPP 的事件帧不是应答。它把 msg_id 之后第 2 字节写的是 client_index（实测这台
// 盒子上干脆就是 0，因为 send_bfd_udp_session_event 只 memset 后填字段、不填
// client_index），第 6 字节起是 pid —— 没有 context。所以按 context 找等待者的
// 那条主路径永远认不出它，在 dispatch 里只会被计入 drops（B3 之前就是这样：
// 订阅发了、retval 回了 0、事件一帧也没交到任何人手上，而 sr-fib 那边照旧每
// 一轮才看一次 BFD）。
package vppapi

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
)

// BFDEvent 是一条 bfd_udp_session_event 里我们真正会用的四个字段。
//
// 不复制 BFDSession 的定时器字段：事件只是"该重看一眼了"的触发器，会话的当前
// 真值一律以下一条 bfd_udp_session_dump 为准（见 sr-fib 的 converge），把定时器
// 一起交出去迟早会让人拿它当现值用。
type BFDEvent struct {
	SwIfIndex uint32
	LocalIP   net.IP
	PeerIP    net.IP
	State     uint32
}

// bfdEventLen 是 vl_api_bfd_udp_session_event_t 的整帧长度：
// msg_id(2) client_index(4) pid(4) sw_if_index(4) local_addr(17) peer_addr(17)
// state(4) is_authenticated(1) bfd_key_id(1) conf_key_id(4) required_min_rx(4)
// desired_min_tx(4) detect_mult(1) = 67 B。
//
// 2026-11-08 在 CPE3（VPP 26.06）上实测：抓到的每一帧都是 67 字节，且按这个
// 布局解出来的 local/peer/state 与 show bfd sessions 逐条对得上。
const bfdEventLen = 67

// WatchBFDEvents 订阅本机 BFD 会话的状态变化，fn 在 readLoop 里被调用。
//
// 三条要紧的语义：
//
//  1. fn 必须非阻塞。它跑在读循环线程上，堵住它 = 整条 api 连接停止收帧，
//     所有在途请求一起挂到超时。正确用法是往一个容量 1 的 channel 里
//     select-default 塞一个"该收敛了"，别在里头做 RPC。
//  2. 事件是**边沿**，不是状态。VPP 只在 bfd_set_state 真变了的时候发
//     （bfd_main.c 的 bfd_on_state_change），会话建起来时停在 down 且对端从未
//     回话，就一帧都不会有 —— 2026-11-08 实测：新会话 add 之后零事件，直到把
//     隧道接口 down/up 才等到 Down/Init/Up。所以"没收到事件"绝不等于"没变"，
//     收到事件也只说明"有个会话变过"，变回什么还得读 dump。
//  3. 一条连接一份订阅，且注册与连接同生共死：VPP 侧的注册表按
//     (pid, client_index) 去重，bfd_event() 会跳过指向已断开连接的旧条目
//     （实测的 send 路径里有 vl_api_client_index_to_registration 这道判空）。
//     连接断了以后旧条目再也发不出来，重连时重新调一次本函数即可 —— sr-fib
//     就是把它放在 dial 闭包里，让每一次重连都自带一次重新注册。
func (c *Client) WatchBFDEvents(ctx context.Context, fn func(BFDEvent)) error {
	if fn == nil {
		return errors.New("vppapi: WatchBFDEvents 需要一个回调")
	}
	evID, err := c.id("bfd_udp_session_event")
	if err != nil {
		return err
	}

	// 先挂回调再发注册请求：反过来的话，中间到达的事件会被当成没人等的帧丢掉。
	//
	// 长度检查不是防御性摆设：回调跑在读循环线程上，越界读会 panic 掉整个进程。
	// 消息表的 CRC 已经把结构体尺寸锁在拨号阶段（requiredVersions 里有
	// bfd_udp_session_event），走到这里还短说明帧和注册表不一致 —— 那种帧宁可
	// 计入 drops 丢掉，也不要解出一个看起来合法的对端地址。
	handler := func(frame []byte) {
		if len(frame) < bfdEventLen {
			c.drops.Add(1)
			return
		}
		fn(BFDEvent{
			SwIfIndex: binary.BigEndian.Uint32(frame[10:14]),
			LocalIP:   readAddress(frame[14:31]),
			PeerIP:    readAddress(frame[31:48]),
			State:     binary.BigEndian.Uint32(frame[48:52]),
		})
	}
	c.mu.Lock()
	if c.asyncs == nil {
		c.asyncs = map[uint16]func([]byte){}
	}
	c.asyncs[evID] = handler
	c.mu.Unlock()

	// vl_api_want_bfd_events_t = u8 enable_disable + u32 pid。pid 不参与投递
	// （bfd_event() 遍历全部注册、不看 pid，实测 pid 取 0/unix pid/client_index
	// 都能收到），它只是 VPP 给注册去重用的键，照 govpp 的写法传自己的进程号。
	body := appendU32([]byte{1}, uint32(os.Getpid()))
	frame, err := c.call(ctx, "want_bfd_events", "want_bfd_events_reply", body)
	if err != nil {
		c.unwatchAsync(evID)
		return err
	}
	if err := c.retval("want_bfd_events", frame); err != nil {
		c.unwatchAsync(evID)
		return err
	}
	return nil
}

func (c *Client) unwatchAsync(msgID uint16) {
	c.mu.Lock()
	delete(c.asyncs, msgID)
	c.mu.Unlock()
}

// String 给日志用的一行人话：哪个对端、变成什么状态。
func (e BFDEvent) String() string {
	return fmt.Sprintf("BFD %s -> %s（本机 %s，sw_if_index=%d）",
		e.PeerIP, BFDStateName(e.State), e.LocalIP, e.SwIfIndex)
}
