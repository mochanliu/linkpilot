package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"sdwan/controller/internal/fib"
	"sdwan/controller/internal/vppapi"
)

// fib.json 是设备上的一份本地配置，改一个字段拼写错不会有人报错——
// DisallowUnknownFields 会，但"颜色写进去了却没传给 FIB"不会。这两条测试把
// 这个静默失败钉住：漏掉 Color 的话所有下一跳都变成 preference 230，路由照样装得上，
// 只是颜色再也不起作用。
func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fib.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigReadsTunnelColors(t *testing.T) {
	cfg, err := loadConfig(writeCfg(t, `{
  "status_url": "http://127.0.0.1:19001/status",
  "status_addr": "127.0.0.1:19011",
  "vrfs": [10],
  "tunnels": [
    {"next_hop": "10.255.0.2", "interface": "wg0"},
    {"next_hop": "10.255.0.3", "interface": "wg1", "color": 5}
  ]
}`))
	if err != nil {
		t.Fatalf("带 color 的配置读不了: %v", err)
	}
	ts := fibTunnels(cfg.Tunnels)
	if len(ts) != 2 {
		t.Fatalf("隧道 %d 条, want 2", len(ts))
	}
	// 热更入口的监听地址是可选的盒子键：写了就要读进来，没写就退化成"不能热更"。
	if cfg.StatusAddr != "127.0.0.1:19011" {
		t.Fatalf("status_addr = %q, want 127.0.0.1:19011", cfg.StatusAddr)
	}
	// 没写颜色就是 0：现存设备上那些 fib.json 一条都不用改。
	if ts[0].NextHop != "10.255.0.2" || ts[0].Interface != "wg0" || ts[0].Color != 0 {
		t.Fatalf("第一条 = %+v, want 颜色 0 的 wg0/10.255.0.2", ts[0])
	}
	if ts[1].Color != 5 || ts[1].Interface != "wg1" {
		t.Fatalf("第二条 = %+v, want 颜色 5 的 wg1", ts[1])
	}
}

func TestLoadConfigRejectsUnknownTunnelField(t *testing.T) {
	// 拼错的颜色必须当场报错，不能悄悄变成"这条隧道颜色 0"。
	_, err := loadConfig(writeCfg(t, `{
  "status_url": "http://127.0.0.1:19001/status",
  "vrfs": [10],
  "tunnels": [{"next_hop": "10.255.0.2", "interface": "wg0", "colour": 5}]
}`))
	if err == nil || !strings.Contains(err.Error(), "colour") {
		t.Fatalf("未知字段应被拒并点名，实际 %v", err)
	}
}

// 逃生开关的键名就是草案 §3.1 的 escape_all_colors，这条测试钉的是"键名即契约"：
// O6 控制器打包时用的必须是同一个名字。缺省必须是关（§5.2：默认 false，昂贵颜色
// 不被默认流量占用），写 true 才开。开关读丢了不会报错 —— 路由照样装得上，只是
// 颜色 0 全 down 后主表直接没这条前缀，于是这个静默要由测试来堵。
func TestLoadConfigEscapeSwitch(t *testing.T) {
	off, err := loadConfig(writeCfg(t, `{
  "status_url": "http://127.0.0.1:19001/status",
  "vrfs": [10]
}`))
	if err != nil {
		t.Fatalf("不带开关的配置读不了: %v", err)
	}
	if off.Escape {
		t.Errorf("缺省 Escape = %v, want false（§5.2 默认关）", off.Escape)
	}
	on, err := loadConfig(writeCfg(t, `{
  "status_url": "http://127.0.0.1:19001/status",
  "vrfs": [10],
  "escape_all_colors": true
}`))
	if err != nil {
		t.Fatalf("开关打开的配置读不了: %v", err)
	}
	if !on.Escape {
		t.Errorf("escape_all_colors=true 没读进 Escape（%v）", on.Escape)
	}
}

// ---------------------------------------------------------------- D3: PUT /config
//
// 这一组钉的是"运行中改 VPN 集合"那条路：PUT 之前数据面是什么、之后是什么，
// 都用假 VPP 的前后状态比对；不靠退出码，也不靠"进程有没有重启"。

// fakeDP 是内存里的 VPP：表、标签、路由三本账，写失败可注入。
type fakeDP struct {
	mu     sync.Mutex
	ifs    []vppapi.SwInterface
	bfd    []vppapi.BFDSession
	tables map[uint32]bool
	labels map[uint32]uint32
	routes map[string]int
	fail   error
}

func newFakeDP() *fakeDP {
	return &fakeDP{
		ifs:    []vppapi.SwInterface{{Name: "wg0", SWIfIndex: 1}, {Name: "wg1", SWIfIndex: 2}},
		tables: map[uint32]bool{},
		labels: map[uint32]uint32{},
		routes: map[string]int{},
	}
}

func (d *fakeDP) failErr() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.fail
}

func (d *fakeDP) setFail(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.fail = err
}

func (d *fakeDP) IPTableAddDel(_ context.Context, id uint32, add bool) error {
	if err := d.failErr(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if add {
		d.tables[id] = true
	} else {
		delete(d.tables, id)
	}
	return nil
}

func (d *fakeDP) IPRouteAddDel(_ context.Context, table uint32, pfx netip.Prefix, paths []vppapi.FIBPath, add bool) error {
	if err := d.failErr(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	k := tableKey(table, pfx)
	if add {
		d.routes[k] = len(paths)
	} else {
		delete(d.routes, k)
	}
	return nil
}

func (d *fakeDP) MPLSLabelAddDel(_ context.Context, label, table uint32, add bool) error {
	if err := d.failErr(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if add {
		d.labels[label] = table
	} else {
		delete(d.labels, label)
	}
	return nil
}

func (d *fakeDP) MPLSLabelDel(_ context.Context, label uint32) error {
	if err := d.failErr(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.labels, label)
	return nil
}

func (d *fakeDP) DumpInterfaces(context.Context) ([]vppapi.SwInterface, error) {
	return d.ifs, nil
}

func (d *fakeDP) FIBRoutes(context.Context, uint32) ([]vppapi.IPRoute, error) { return nil, nil }
func (d *fakeDP) CLI(context.Context, string) (string, error)                 { return "", nil }
func (d *fakeDP) BFDSessions(context.Context) ([]vppapi.BFDSession, error)    { return d.bfd, nil }

func tableKey(table uint32, pfx netip.Prefix) string {
	return netip.PrefixFrom(pfx.Addr(), pfx.Bits()).String() + "@" + strconv.FormatUint(uint64(table), 10)
}

// ribStub 是 sr-route 的 /status：body 可以中途换，模拟 RIB 跟着 VPN 表收敛。
type ribStub struct{ body atomic.Value }

func (s *ribStub) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	_, _ = io.WriteString(w, s.body.Load().(string))
}

func newFiberForTest(t *testing.T) (*fiber, *fakeDP, *ribStub) {
	t.Helper()
	dp := newFakeDP()
	stub := &ribStub{}
	stub.body.Store(`{"rib":{}}`)
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	f := &fiber{dp: dp, applier: fib.NewApplier(dp), hc: srv.Client(), statusURL: srv.URL}
	cur := liveConfig{VRFs: []uint8{0}}
	f.cur.Store(&cur)
	return f, dp, stub
}

func putConfig(t *testing.T, f *fiber, body string) (*httptest.ResponseRecorder, configDoc) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/config", strings.NewReader(body))
	configMux(f).ServeHTTP(rec, req)
	var doc configDoc
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	return rec, doc
}

func getStatus(t *testing.T, f *fiber) statusDoc {
	t.Helper()
	rec := httptest.NewRecorder()
	configMux(f).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /status = %d: %s", rec.Code, rec.Body.String())
	}
	var doc statusDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("GET /status body: %v", err)
	}
	return doc
}

func labelsOf(dp *fakeDP) map[uint32]uint32 {
	dp.mu.Lock()
	defer dp.mu.Unlock()
	out := map[uint32]uint32{}
	for k, v := range dp.labels {
		out[k] = v
	}
	return out
}

func TestPutConfigRejectsBadTablesAndKeepsCurrent(t *testing.T) {
	f, dp, _ := newFiberForTest(t)
	cases := []struct{ name, body, want string }{
		{"保留表 250", `{"vrfs":[250]}`, "250"},
		{"没有 VRF", `{"vrfs":[]}`, "没有配置任何 VRF"},
		{"隧道缺接口名", `{"vrfs":[0],"tunnels":[{"next_hop":"10.255.0.2"}]}`, "没有接口名"},
		{"同一对端两条隧道", `{"vrfs":[0],"tunnels":[{"next_hop":"10.255.0.2","interface":"wg0"},{"next_hop":"10.255.0.2","interface":"wg1"}]}`, "配了两次"},
		{"未知字段", `{"vrfs":[0],"status_url":"http://x/status"}`, "unknown field"},
		{"颜色超上限", `{"vrfs":[0],"tunnels":[{"next_hop":"10.255.0.2","interface":"wg0","color":9}]}`, "颜色"},
	}
	for _, tc := range cases {
		rec, _ := putConfig(t, f, tc.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: PUT = %d %q, want 400 含 %q", tc.name, rec.Code, rec.Body.String(), tc.want)
		}
	}
	// 全被拒之后，活配置还是启动时那份，数据面也没被写过。
	st := getStatus(t, f)
	if len(st.VRFs) != 1 || st.VRFs[0] != 0 {
		t.Fatalf("被拒之后 VRF 变成了 %v", st.VRFs)
	}
	if len(labelsOf(dp)) != 0 {
		t.Fatalf("被拒之后写了标签: %v", labelsOf(dp))
	}
}

func TestPutConfigHotAddsVRFWithoutRestart(t *testing.T) {
	f, dp, stub := newFiberForTest(t)
	tunnels := `[{"next_hop":"10.255.0.2","interface":"wg0"}]`

	rec, doc := putConfig(t, f, `{"vrfs":[0,100],"tunnels":`+tunnels+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /config = %d: %s", rec.Code, rec.Body.String())
	}
	if len(doc.VRFs) != 2 || doc.VRFs[1] != 100 {
		t.Fatalf("应答里的 VRF = %v", doc.VRFs)
	}
	st := getStatus(t, f)
	if st.Last == nil || !st.Last.Converged {
		t.Fatalf("/status 没有一次成功的收敛: %+v", st.Last)
	}
	// 新 VPN 的两样东西必须当场出现：本地标签 1000/1100，表 100。
	labels := labelsOf(dp)
	if labels[1000] != 0 || labels[1100] != 100 {
		t.Fatalf("标签 = %v, want 1000→0、1100→100", labels)
	}
	dp.mu.Lock()
	_, has100 := dp.tables[100]
	dp.mu.Unlock()
	if !has100 {
		t.Fatal("PUT 之后没有建 table 100")
	}

	// RIB 里出现 VPN 100 的远端前缀：再 PUT 同一份配置，路由要进 table 100。
	stub.body.Store(`{"rib":{"100":[{"device_id":2,"vrf_id":100,"prefix":"10.100.2.0/24","next_hops":["10.255.0.2"]}]}}`)
	rec, _ = putConfig(t, f, `{"vrfs":[0,100],"tunnels":`+tunnels+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("第二次 PUT = %d: %s", rec.Code, rec.Body.String())
	}
	dp.mu.Lock()
	n, ok := dp.routes["10.100.2.0/24@100"]
	dp.mu.Unlock()
	if !ok || n != 1 {
		t.Fatalf("table 100 里的路由 = %d 条（在:%v），want 1", n, ok)
	}

	// 反向：把 VPN 100 从配置里拿掉（sr-route 那边先撤，所以 RIB 也先空），
	// 标签与路由都要跟着撤，进程本身当然还在。
	stub.body.Store(`{"rib":{}}`)
	rec, doc = putConfig(t, f, `{"vrfs":[0],"tunnels":`+tunnels+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("撤 VPN 的 PUT = %d: %s", rec.Code, rec.Body.String())
	}
	if len(doc.VRFs) != 1 {
		t.Fatalf("撤完之后应答 VRF = %v", doc.VRFs)
	}
	if _, ok := labelsOf(dp)[1100]; ok {
		t.Fatal("撤掉 VPN 100 之后标签 1100 还在")
	}
	dp.mu.Lock()
	_, still := dp.routes["10.100.2.0/24@100"]
	dp.mu.Unlock()
	if still {
		t.Fatal("撤掉 VPN 100 之后 table 100 的路由还在")
	}
}

func TestPutConfigAcceptedButUnconverged(t *testing.T) {
	f, dp, _ := newFiberForTest(t)
	dp.setFail(errors.New("vpp 正忙"))
	rec, doc := putConfig(t, f, `{"vrfs":[0,100],"escape_all_colors":true}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("收敛失败时 PUT = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if len(doc.VRFs) != 2 || !doc.EscapeAllColors {
		t.Fatalf("应答里的配置 = %v escape=%v（配置应该已被接受）", doc.VRFs, doc.EscapeAllColors)
	}
	st := getStatus(t, f)
	if st.Last == nil || st.Last.Converged || !strings.Contains(st.Last.Error, "vpp 正忙") {
		t.Fatalf("/status 没如实说这一轮失败: %+v", st.Last)
	}

	// 故障消失后轮询自己会收敛，不需要再 PUT 一次。
	dp.setFail(nil)
	f.mu.Lock()
	err := f.converge(context.Background())
	f.mu.Unlock()
	if err != nil {
		t.Fatalf("恢复后收敛失败: %v", err)
	}
	if st := getStatus(t, f); st.Last == nil || !st.Last.Converged {
		t.Fatalf("恢复后 /status 仍是失败: %+v", st.Last)
	}
	if _, ok := labelsOf(dp)[1100]; !ok {
		t.Fatal("恢复后 VPN 100 的标签没补上")
	}
}
