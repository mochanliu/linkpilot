// B3：异步事件通路。这几条测试锁的是实测来的字节事实，不是实现细节 ——
// 事件帧没有 context（vl_api_bfd_udp_session_event_t 在 msg_id 之后是
// client_index + pid），回调不能跑在 c.mu 里，短帧宁可可计数地丢掉也绝不
// panic 掉读循环。
package vppapi

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

var (
	evLocal = net.ParseIP("172.16.20.1")
	evPeer  = net.ParseIP("172.16.20.2")
)

// collectBFDEvents 订阅事件并把收到的交回一个 channel。
func collectBFDEvents(t *testing.T, c *Client) chan BFDEvent {
	t.Helper()
	events := make(chan BFDEvent, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := c.WatchBFDEvents(ctx, func(ev BFDEvent) { events <- ev })
	if err != nil {
		t.Fatalf("订阅 want_bfd_events: %v", err)
	}
	return events
}

func TestWatchBFDEventsDelivers(t *testing.T) {
	f := newFake(t)
	c := f.dial(t)
	events := collectBFDEvents(t, c)

	// 请求体必须是大端 u8 enable_disable=1 + u32 pid：实测真机收到的正是 5 字节，
	// 短一字节 VPP 一帧都不回（超时和"插件没这条消息"长一模一样，最难查）。
	req := f.lastRequest(t)
	if req.name != "want_bfd_events" {
		t.Fatalf("订阅要发 want_bfd_events，got %s", req.name)
	}
	if len(req.body) != 5 || req.body[0] != 1 {
		t.Errorf("want_bfd_events 请求体 = enable(1)+pid(4)，got %d 字节 %x", len(req.body), req.body)
	}

	conn := f.waitConn(t)
	before := c.Drops()
	writeFrame(conn, f.bfdEventFrame(BFDMultihopSwIfIndex, evLocal, evPeer, BFDStateDown))

	select {
	case ev := <-events:
		if ev.State != BFDStateDown {
			t.Errorf("state=%d 期望 %d(Down)", ev.State, BFDStateDown)
		}
		if ev.SwIfIndex != BFDMultihopSwIfIndex {
			t.Errorf("多跳会话的 sw_if_index 应是 ~0，got %d", ev.SwIfIndex)
		}
		if !ev.LocalIP.Equal(evLocal) || !ev.PeerIP.Equal(evPeer) {
			t.Errorf("地址解错：local=%v peer=%v", ev.LocalIP, ev.PeerIP)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("事件没交到回调：要么按 msg_id 分发没生效，要么帧头偏移又读成了 context")
	}
	if after := c.Drops(); after != before {
		t.Errorf("认得出的事件不该计入 drops：before=%d after=%d", before, after)
	}
	// 连接照常能用：事件之后仍按老路走请求/应答。
	readCtx, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if _, err := c.BFDSessions(readCtx); err != nil {
		t.Errorf("事件之后连接应照常可用: %v", err)
	}
}

// TestBFDEventCallbackOutsideLock 锁住那条最容易写错的：dispatch 若抱着 c.mu
// 调回调，任何在回调里再进这个 Client 的代码都会自锁死。用 TryLock 证明，
// 而不是写一条会挂到测试超时的用例。
func TestBFDEventCallbackOutsideLock(t *testing.T) {
	f := newFake(t)
	c := f.dial(t)

	locked := make(chan bool, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.WatchBFDEvents(ctx, func(BFDEvent) {
		for i := 0; i < 20; i++ {
			if c.mu.TryLock() {
				c.mu.Unlock()
				locked <- true
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		locked <- false
	}); err != nil {
		t.Fatalf("订阅: %v", err)
	}
	writeFrame(f.waitConn(t), f.bfdEventFrame(BFDMultihopSwIfIndex, evLocal, evPeer, BFDStateUp))

	select {
	case ok := <-locked:
		if !ok {
			t.Fatal("回调跑在 c.mu 里：回调里再调本包任何请求都会自锁死")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("事件没交到回调")
	}
}

func TestBFDEventShortFrameCountedNotDelivered(t *testing.T) {
	f := newFake(t)
	c := f.dial(t)
	events := collectBFDEvents(t, c)

	conn := f.waitConn(t)

	// 故意短 3 字节：越界读会 panic 掉读循环，整条连接连同在途请求一起没。
	before := c.Drops()
	short := f.bfdEventFrame(BFDMultihopSwIfIndex, evLocal, evPeer, BFDStateDown)
	writeFrame(conn, short[:len(short)-3])
	deadline := time.Now().Add(2 * time.Second)
	for c.Drops() == before && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if c.Drops() != before+1 {
		t.Fatalf("被丢掉的坏帧必须可计数：before=%d now=%d", before, c.Drops())
	}

	// 丢掉这一帧不许把通路本身弄坏。
	writeFrame(conn, f.bfdEventFrame(BFDMultihopSwIfIndex, evLocal, evPeer, BFDStateUp))

	select {
	case ev := <-events:
		if ev.State != BFDStateUp {
			t.Fatalf("只该收到那条完整的 Up，got state=%d", ev.State)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("短帧之后完整帧也必须交到")
	}
}

func TestWatchBFDEventsSurfacesRetval(t *testing.T) {
	f := newFake(t)
	f.retvals["want_bfd_events"] = -20
	c := f.dial(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := c.WatchBFDEvents(ctx, func(BFDEvent) { t.Error("订阅失败的那次不该收到事件") })
	if err == nil || !strings.Contains(err.Error(), "-20") {
		t.Fatalf("VPP 回非 0 retval 必须报错并点名数值，got: %v", err)
	}

	// 订阅失败就不许留下一个"看着已订阅"的回调：之后到的帧按没人等计数。
	writeFrame(f.waitConn(t), f.bfdEventFrame(BFDMultihopSwIfIndex, evLocal, evPeer, BFDStateDown))
	deadline := time.Now().Add(2 * time.Second)
	for c.Drops() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if c.Drops() == 0 {
		t.Error("失败订阅留下的事件帧必须计入 drops，不能静默交到某处")
	}
}

func TestDialRefusesEventStructDrift(t *testing.T) {
	f := newFake(t).withVersion("bfd_udp_session_event", "deadbeef")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := Dial(ctx, f.path, "test-agent")
	if err == nil || !strings.Contains(err.Error(), "bfd_udp_session_event") {
		t.Fatalf("事件结构体的 CRC 漂移必须在拨号时就拒绝，got: %v", err)
	}
}
