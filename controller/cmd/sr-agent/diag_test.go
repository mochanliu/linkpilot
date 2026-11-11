package main

// diag_test.go —— 盒子侧执行器的回归面：白名单/校验的边界 + argv 形状 +
// 忙闲互斥。执行器是这台上唯一"替别人跑命令"的代码，白名单就是它的全部
// 攻击面，所以边界逐个钉。

import (
	"reflect"
	"sync/atomic"
	"testing"

	"sdwan/controller/internal/wire"
)

func TestDiagValidate(t *testing.T) {
	ok := []wire.DiagCommand{
		{Tool: "ping", Target: "172.16.3.1", Count: 4},
		{Tool: "ping", Target: "host.example.com", Iface: "tap12"},
		{Tool: "traceroute", Target: "2001:db8::1"},
		{Tool: "vpp-show", VppShow: "show ip fib"},
		{Tool: "vpp-show", VppShow: "  show   interface  "}, // 空白归一后命中
	}
	for _, c := range ok {
		if err := diagValidate(&c); err != nil {
			t.Fatalf("应通过: %+v → %v", c, err)
		}
	}
	bad := []wire.DiagCommand{
		{Tool: "curl", Target: "http://x"},
		{Tool: "ping", Target: "a b"},
		{Tool: "ping", Target: "x;y"},
		{Tool: "ping", Target: ""},
		{Tool: "ping", Target: "a.b", Iface: "tap; rm"},
		{Tool: "ping", Target: "a.b", Iface: "sixteen-char-long"}, // >IFNAMSIZ
		{Tool: "traceroute", Target: ""},
		{Tool: "vpp-show", VppShow: ""},
		{Tool: "vpp-show", VppShow: "show ip fib detail extra"},
		{Tool: "vpp-show", VppShow: "show run"},
		{},
	}
	for _, c := range bad {
		if err := diagValidate(&c); err == nil {
			t.Fatalf("应拒绝: %+v", c)
		}
	}
}

func TestDiagArgv(t *testing.T) {
	cases := []struct {
		c    wire.DiagCommand
		want []string
	}{
		{wire.DiagCommand{Tool: "ping", Target: "1.2.3.4", Count: 3},
			[]string{"ping", "-c", "3", "-w", "20", "1.2.3.4"}},
		{wire.DiagCommand{Tool: "ping", Target: "1.2.3.4", Iface: "tap12"},
			[]string{"ping", "-c", "4", "-w", "20", "-I", "tap12", "1.2.3.4"}},
		{wire.DiagCommand{Tool: "ping", Target: "1.2.3.4", Count: 99},
			[]string{"ping", "-c", "10", "-w", "20", "1.2.3.4"}}, // 夹回上限
		{wire.DiagCommand{Tool: "traceroute", Target: "1.2.3.4"},
			[]string{"traceroute", "-n", "-w", "2", "-q", "1", "-m", "15", "1.2.3.4"}},
		{wire.DiagCommand{Tool: "traceroute", Target: "1.2.3.4", Iface: "tap12"},
			[]string{"traceroute", "-n", "-w", "2", "-q", "1", "-m", "15", "-i", "tap12", "1.2.3.4"}},
		{wire.DiagCommand{Tool: "vpp-show", VppShow: "show bfd session"},
			[]string{"vppctl", "show", "bfd", "session"}},
	}
	for _, tc := range cases {
		if got := diagArgv(&tc.c); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("argv: %+v → %v, want %v", tc.c, got, tc.want)
		}
	}
}

func TestDiagShowAllowed(t *testing.T) {
	for _, ok := range []string{"show interface", "show ip fib", "show ip6 fib",
		"show ip tunnel", "show bfd session", "show acl-plugin acl", "show abf"} {
		if !diagShowAllowed(ok) {
			t.Fatalf("白名单内应放行: %s", ok)
		}
	}
	for _, bad := range []string{"", "show", "show interface attach", "show ip fib x", "reload"} {
		if diagShowAllowed(bad) {
			t.Fatalf("应拒绝: %q", bad)
		}
	}
}

func TestDiagRunnerBusyMutex(t *testing.T) {
	d := newDiagRunner(nil, nil, func(string, ...any) {})
	if d.Busy() {
		t.Fatal("空闲时 Busy 应为 false")
	}
	// 模拟占用：CAS 置位后 Busy 翻真；清掉又翻回。
	if !atomic.CompareAndSwapInt32(&d.busy, 0, 1) {
		t.Fatal("空闲时 CAS 应成功")
	}
	if !d.Busy() {
		t.Fatal("占位后 Busy 应为 true")
	}
	atomic.StoreInt32(&d.busy, 0)
	if d.Busy() {
		t.Fatal("清位后 Busy 应为 false")
	}
}
