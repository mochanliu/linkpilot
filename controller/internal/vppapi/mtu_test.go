package vppapi

import (
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"
)

// MTU 走的是 **core 的 sw_interface_set_mtu**，不是本插件的消息。这一点本身就要测：
// 客户端的 CRC 核对表原本只有插件消息，core 消息一旦漏入门禁，VPP 会用不同的结构
// 尺寸收下这个请求 —— 表现是 retval 正常、MTU 却没变，比崩溃更难查。

func TestSetInterfaceMTUEncoding(t *testing.T) {
	f := newFake(t)
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()
	if err := c.SetInterfaceMTU(ctx, 42, 1360); err != nil {
		t.Fatal(err)
	}
	r := f.lastRequest(t)
	if r.name != "sw_interface_set_mtu" {
		t.Fatalf("请求名 = %s，应当是 core 的 sw_interface_set_mtu", r.name)
	}
	// vl_api_sw_interface_set_mtu_t 体 = sw_if_index(u32) + mtu[4]（4 个 u32 槽位：
	// L3 / IP4 / IP6 / MPLS）。20 字节这个长度就是"客户端与 VPP 的头文件没脱节"
	// 这件事最直接的证据。
	if len(r.body) != 20 {
		t.Fatalf("体长 = %d，期望 20（sw_if_index 4 + 4 个 MTU 槽位各 4）", len(r.body))
	}
	if got := binary.BigEndian.Uint32(r.body[0:4]); got != 42 {
		t.Errorf("sw_if_index = %d，期望 42", got)
	}
	if got := binary.BigEndian.Uint32(r.body[4:8]); got != 1360 {
		t.Errorf("L3 槽位 = %d，期望 1360", got)
	}
	// 后三个槽位**必须留 0**。实机验证的语义是：packet=N 把 L3 设成 N 并把 IP4/IP6/
	// MPLS 一起清零，于是 IP4 回落到 L3；填了 IP4 反而会让转发按那个更小的数走。
	// 这里清零 = 复刻 CLI 的行为，不多做决定。
	for i, slot := range []string{"IP4", "IP6", "MPLS"} {
		if got := binary.BigEndian.Uint32(r.body[8+i*4:]); got != 0 {
			t.Errorf("%s 槽位 = %d，期望 0（回落到 L3）", slot, got)
		}
	}
}

func TestSetInterfaceMTURejectsZeroBeforeSend(t *testing.T) {
	// 0 不是"没要求"，实机语义是**把四个槽位一起清零**（连 `packet 0` 都照收），
	// 等于取消这张接口的 MTU 记录。VPP 不会拦，所以客户端必须拦 —— 拦在这儿，
	// 后面那条"VIF 记 9000、网卡只有 1500"的发送侧卡死就没有重演的机会。
	f := newFake(t)
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()
	if err := c.SetInterfaceMTU(ctx, 42, 0); err == nil {
		t.Fatal("MTU=0 必须在发出前报错")
	}
	// 上面那一次不该有任何字节出去：紧跟一次合法调用，收到的**第一**条请求必须
	// 是这次合法调用，否则就说明 0 也发了。
	if err := c.SetInterfaceMTU(ctx, 43, 1500); err != nil {
		t.Fatal(err)
	}
	r := f.lastRequest(t)
	if binary.BigEndian.Uint32(r.body[0:4]) != 43 {
		t.Errorf("MTU=0 那次请求真的发出去了（先收到的是 sw_if_index=%d）",
			binary.BigEndian.Uint32(r.body[0:4]))
	}
}

func TestSetInterfaceMTUPropagatesRetval(t *testing.T) {
	// 接口号不存在（或这张接口不接受改 MTU）时 VPP 回非 0 retval。不能当成功返回：
	// agent 会以为 MTU 落地了，Open 之后整条链路带着一条没有 MTU 记录的接口跑起来。
	f := newFake(t)
	f.retvals["sw_interface_set_mtu"] = -6
	c := f.dial(t)
	ctx, cancel := shortCtx()
	defer cancel()
	err := c.SetInterfaceMTU(ctx, 99, 1360)
	var apiErr APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("期望 APIError，got: %T %v", err, err)
	}
	if apiErr.Retval != -6 {
		t.Errorf("retval = %d，期望 -6", apiErr.Retval)
	}
}

// core 消息也在 CRC 门禁里：这条测的是"门禁的范围"，不是插件。
func TestDialRefusesCRCDriftOnCoreMTUMessage(t *testing.T) {
	f := newFake(t).withVersion("sw_interface_set_mtu", "deadbeef")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := Dial(ctx, f.path, "test-agent")
	if err == nil {
		t.Fatal("core 消息的 CRC 漂移也必须拒绝建连")
	}
	if !strings.Contains(err.Error(), "sw_interface_set_mtu") {
		t.Errorf("错误要点名哪条消息，got: %v", err)
	}
}

// 反方向同样要紧：VPP 太旧、根本没有这条消息时，必须在 Dial 就说不，而不是等到
// 第一次改 MTU 才报"unknown message"。
func TestDialRefusesMissingMTUMessage(t *testing.T) {
	f := newFake(t).dropVersion("sw_interface_set_mtu")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := Dial(ctx, f.path, "test-agent"); err == nil {
		t.Fatal("VPP 没有 sw_interface_set_mtu 时必须拒绝建连")
	}
}
