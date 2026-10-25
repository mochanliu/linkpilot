package vppapi

import (
	"context"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestLiveFIB 用真 VPP 验证三条 FIB 下发消息的**线布局与语义**：
// ip_table_add_del / ip_route_add_del / mpls_route_add_del。
//
// 单测只能证明"我们按自己算的偏移写了字节"；这里要证明的是"VPP 按同一份
// packed 结构读了它们"—— 布局错一个字节，VPP 不会报错，只会超时或收下
// 一个对不上的对象，那种故障在 FIB 加载器上线之后就是"路由看起来装了但不通"。
//
// 号段刻意避开实验室：表 9901、标签 9991/9992、前缀 192.0.2.0/24（TEST-NET-1），
// 跑完全部删掉。
//
//	SRWG_VPP_SOCK=/run/vpp/api.sock go test ./internal/vppapi -run TestLiveFIB -v
func TestLiveFIB(t *testing.T) {
	sock := os.Getenv("SRWG_VPP_SOCK")
	if sock == "" {
		t.Skip("未设 SRWG_VPP_SOCK，跳过真机验证")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := Dial(ctx, sock, "srwg-vppapi-fiblivetest")
	if err != nil {
		t.Fatalf("Dial(%s): %v", sock, err)
	}
	// 关闭必须排在 cleanup 之后：t.Cleanup 按注册顺序的逆序执行，而 defer 比
	// 任何 t.Cleanup 都先跑。直接 defer c.Close() 会让 cleanup 用一条已经关掉
	// 的连接删标签/删表，错误又被 `_ =` 吞掉 —— 实测会留下 9991 + 9901。
	t.Cleanup(func() { c.Close() })

	const (
		testTable = 9901
		testLabel = 9991
	)
	testPrefix := netip.MustParsePrefix("192.0.2.1/32")
	nh := netip.MustParseAddr("192.0.2.2")

	// 找一张 WG 隧道口做下一跳的出口。找不到就不能证明"带标签路由"这条
	// （它和 MPLS 本地标签不同，必须有真实接口）。
	ifaces, err := c.DumpInterfaces(ctx)
	if err != nil {
		t.Fatalf("DumpInterfaces: %v", err)
	}
	var wgIf uint32
	for _, i := range ifaces {
		if strings.HasPrefix(i.Name, "wg") {
			wgIf = i.SWIfIndex
			break
		}
	}
	if wgIf == 0 {
		t.Skip("这台节点上没有 wg 接口，跳过带标签路由的验证")
	}

	cleanup := func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		_ = c.IPRouteAddDel(cctx, testTable, testPrefix, []FIBPath{{SWIfIndex: wgIf, NextHop: nh}}, false)
		_ = c.MPLSLabelDel(cctx, testLabel)
		_ = c.IPTableAddDel(cctx, testTable, false)
	}
	cleanup()
	t.Cleanup(cleanup)

	if err := c.IPTableAddDel(ctx, testTable, true); err != nil {
		t.Fatalf("IPTableAddDel: %v", err)
	}
	if err := c.MPLSLabelAddDel(ctx, testLabel, testTable, true); err != nil {
		t.Fatalf("MPLSLabelAddDel: %v", err)
	}
	if err := c.IPRouteAddDel(ctx, testTable, testPrefix,
		[]FIBPath{{SWIfIndex: wgIf, NextHop: nh, Labels: []uint32{testLabel}}}, true); err != nil {
		t.Fatalf("IPRouteAddDel: %v", err)
	}

	routes, err := c.FIBRoutes(ctx, testTable)
	if err != nil {
		t.Fatalf("FIBRoutes: %v", err)
	}
	found := false
	for _, r := range routes {
		if r.Prefix != testPrefix {
			continue
		}
		found = true
		if len(r.Paths) != 1 {
			t.Fatalf("路径数 = %d，期望 1", len(r.Paths))
		}
		p := r.Paths[0]
		if p.SWIfIndex != wgIf || p.NextHop != nh || len(p.Labels) != 1 || p.Labels[0] != testLabel {
			t.Fatalf("API 读回与写入不一致: %+v（期望 sw_if=%d nh=%s label=%d）", p, wgIf, nh, testLabel)
		}
	}
	if !found {
		t.Fatalf("表 %d 里没有刚写入的 %s", testTable, testPrefix)
	}

	labels, err := c.MPLSRoutes(ctx, 0)
	if err != nil {
		t.Fatalf("MPLSRoutes: %v", err)
	}
	found = false
	for _, r := range labels {
		if r.Label != testLabel {
			continue
		}
		found = true
		if !r.EOS {
			t.Error("本地标签的 eos 读回是 0，期望 1")
		}
		if len(r.Paths) != 1 || r.Paths[0].SWIfIndex != ^uint32(0) || r.Paths[0].TableID != testTable {
			t.Fatalf("lookup-in-table 路径读回不对: %+v", r.Paths)
		}
	}
	if !found {
		t.Fatalf("MPLS 表里没有刚写入的标签 %d", testLabel)
	}

	// 独立实现对一遍：VPP 自己的 CLI 也是按同一张 FIB 渲染的，名字/标签
	// 对不上说明我们读的是另一条（或者是缓存）。
	out, err := exec.Command("vppctl", "show", "ip", "fib", "table", "9901").CombinedOutput()
	if err != nil {
		t.Logf("vppctl 不可用（%v），跳过 CLI 对照", err)
	} else if !strings.Contains(string(out), "192.0.2.1/32") || !strings.Contains(string(out), "9991") {
		t.Fatalf("vppctl show ip fib table 9901 里看不到这条带标签路由:\n%s", out)
	}
	out, err = exec.Command("vppctl", "show", "mpls", "fib").CombinedOutput()
	if err != nil {
		t.Logf("vppctl 不可用（%v），跳过 MPLS CLI 对照", err)
	} else if !strings.Contains(string(out), "9991") {
		t.Fatalf("vppctl show mpls fib 里看不到标签 9991:\n%s", out)
	}

	// 删除必须是幂等的：FIB 加载器撤路由时靠它，失败会让"已经撤掉的路由"
	// 一直留在表里（而且 VPP 不会因为重复删除报错）。
	if err := c.IPRouteAddDel(ctx, testTable, testPrefix, []FIBPath{{SWIfIndex: wgIf, NextHop: nh}}, false); err != nil {
		t.Fatalf("删路由: %v", err)
	}
	routes, err = c.FIBRoutes(ctx, testTable)
	if err != nil {
		t.Fatalf("FIBRoutes: %v", err)
	}
	for _, r := range routes {
		if r.Prefix == testPrefix {
			t.Fatalf("删掉之后读回仍然有 %s", testPrefix)
		}
	}
}
