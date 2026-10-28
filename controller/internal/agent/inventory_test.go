package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"sdwan/controller/internal/vppapi"
)

// fakeInventory 是数据面那条只读接缝的假实现：单测不该为了读一张接口表去起 VPP。
type fakeInventory struct {
	mu     sync.Mutex
	ifaces []vppapi.SwInterface
	err    error
	calls  int
	closed int
}

func (f *fakeInventory) Inventory(context.Context) ([]vppapi.SwInterface, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.ifaces, nil
}

func (f *fakeInventory) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return nil
}

func (f *fakeInventory) stats() (calls, closed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.closed
}

// newTestReporter 造一个上报器并收集日志；dial 为空时用默认（真 vppapi.Dial）。
func newTestReporter(t *testing.T, dial func(context.Context, string, string) (InventorySource, error)) (*InventoryReporter, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var logs []string
	r := NewInventoryReporter("/run/vpp/api.sock", "sr-agent-test", func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	})
	if dial != nil {
		r.dial = dial
	}
	t.Cleanup(func() { r.Close() })
	return r, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), logs...)
	}
}

func TestInventoryReporterIsOffWithoutSocket(t *testing.T) {
	// 不带 -vpp-api 的 agent 不碰数据面：Report 必须直接返回 nil，连拨号都不该试。
	r := NewInventoryReporter("", "sr-agent-test", nil)
	r.dial = func(context.Context, string, string) (InventorySource, error) {
		t.Fatal("没配 socket 时不该连 VPP")
		return nil, nil
	}
	if got := r.Report(context.Background()); got != nil {
		t.Fatalf("没开功能时应报 nil（未上报），得到 %#v", got)
	}
}

func TestInventoryReporterConvertsDataplaneReading(t *testing.T) {
	src := &fakeInventory{ifaces: []vppapi.SwInterface{
		{SWIfIndex: 0, Name: "local0", L3MTU: 0, AdminUp: false, LinkUp: false},
		{SWIfIndex: 1, Name: "host-eth1", L3MTU: 1500, AdminUp: true, LinkUp: true,
			IPv4: []vppapi.Prefix{{IP: []byte{10, 10, 1, 2}, Len: 24}}},
		{SWIfIndex: 2, Name: "wg0", L3MTU: 1360, AdminUp: true, LinkUp: true,
			IPv4: []vppapi.Prefix{{IP: []byte{192, 168, 99, 1}, Len: 24}}},
	}}
	r, _ := newTestReporter(t, func(context.Context, string, string) (InventorySource, error) {
		return src, nil
	})

	got := r.Report(context.Background())
	if got == nil {
		t.Fatal("读成功时必须返回清单，不能是 nil")
	}
	list := *got
	if len(list) != 3 {
		t.Fatalf("报了 %d 张接口，期望 3", len(list))
	}
	if list[1].Name != "host-eth1" || list[1].SWIfIndex != 1 || list[1].MTU != 1500 ||
		!list[1].AdminUp || !list[1].LinkUp {
		t.Errorf("host-eth1 报成了 %+v", list[1])
	}
	if addrs := list[1].Addrs; len(addrs) != 1 || addrs[0] != "10.10.1.2/24" {
		t.Errorf("host-eth1 的地址 = %v，期望 [10.10.1.2/24]", addrs)
	}
	// 没有地址的接口必须报空，而不是一条 0.0.0.0/0：后者会被控制器当成真实地址。
	if len(list[0].Addrs) != 0 {
		t.Errorf("local0 不该有地址，得到 %v", list[0].Addrs)
	}
	if calls, _ := src.stats(); calls != 1 {
		t.Errorf("读了 %d 次，期望 1", calls)
	}
}

func TestInventoryReporterKeepsNilAndEmptyApart(t *testing.T) {
	// 契约：nil = 没上报（agent 没开 / VPP 读不到）；空列表 = 数据面确实没有接口。
	src := &fakeInventory{ifaces: []vppapi.SwInterface{}}
	r, _ := newTestReporter(t, func(context.Context, string, string) (InventorySource, error) {
		return src, nil
	})
	got := r.Report(context.Background())
	if got == nil {
		t.Fatal("空接口表必须报成空清单（非 nil），否则控制器分不清“没上报”和“没有接口”")
	}
	if len(*got) != 0 {
		t.Errorf("期望空清单，得到 %#v", *got)
	}
}

func TestInventoryReporterRetriesDialNextHeartbeat(t *testing.T) {
	var mu sync.Mutex
	dials := 0
	src := &fakeInventory{ifaces: []vppapi.SwInterface{{SWIfIndex: 1, Name: "host-eth1", L3MTU: 1500}}}
	r, _ := newTestReporter(t, func(context.Context, string, string) (InventorySource, error) {
		mu.Lock()
		defer mu.Unlock()
		dials++
		if dials == 1 {
			return nil, errors.New("dial unix /run/vpp/api.sock: connect: no such file")
		}
		return src, nil
	})

	if got := r.Report(context.Background()); got != nil {
		t.Fatalf("VPP 连不上时应报 nil，得到 %#v", got)
	}
	// 下一次心跳必须重试，而不是把第一次的失败当成永久状态。
	if got := r.Report(context.Background()); got == nil {
		t.Fatal("第二次应当重连成功并带上清单")
	}
	mu.Lock()
	defer mu.Unlock()
	if dials != 2 {
		t.Errorf("拨号 %d 次，期望 2", dials)
	}
}

func TestInventoryReporterDropsConnectionAfterReadFailure(t *testing.T) {
	// 读失败（超时/连接断开）后必须丢掉这条连接：留着它，后面每次心跳都会在同一个
	// 死 socket 上失败，而且再也不会重新注册。
	bad := &fakeInventory{err: errors.New("vppapi: 连接已关闭")}
	good := &fakeInventory{ifaces: []vppapi.SwInterface{{SWIfIndex: 1, Name: "host-eth1", L3MTU: 1500}}}
	var mu sync.Mutex
	n := 0
	r, _ := newTestReporter(t, func(context.Context, string, string) (InventorySource, error) {
		mu.Lock()
		defer mu.Unlock()
		n++
		if n == 1 {
			return bad, nil
		}
		return good, nil
	})

	if got := r.Report(context.Background()); got != nil {
		t.Fatalf("读失败时应报 nil，得到 %#v", got)
	}
	if _, closed := bad.stats(); closed != 1 {
		t.Errorf("失败后应关闭连接（closed=%d，期望 1）", closed)
	}
	if got := r.Report(context.Background()); got == nil {
		t.Fatal("第二次应当换了新连接并读成功")
	}
}

func TestInventoryReporterLogsTheSameFailureOnce(t *testing.T) {
	// 一个每 10 秒心跳一次的 agent，VPP 停摆一小时就是 360 条一模一样的日志；
	// 真正需要看见的是"第一次"和"恢复后的第一次"。
	r, logs := newTestReporter(t, func(context.Context, string, string) (InventorySource, error) {
		return nil, errors.New("no such file")
	})
	for i := 0; i < 3; i++ {
		if got := r.Report(context.Background()); got != nil {
			t.Fatalf("第 %d 次应报 nil", i)
		}
	}
	got := logs()
	if len(got) != 1 {
		t.Fatalf("同一句错误打了 %d 次，期望 1 次: %v", len(got), got)
	}
	if !strings.Contains(got[0], "接口清单") {
		t.Errorf("日志应当点明是接口清单的问题，得到 %q", got[0])
	}
}
