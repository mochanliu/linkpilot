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
	"time"

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
// dead=true 表示 VPP 进程重启后这条 api 连接的状态：所有读写一律 ErrClosed，
// 内存里那三本账也随旧进程一起没了（新建 fakeDP 就是空账）。
type fakeDP struct {
	mu     sync.Mutex
	ifs    []vppapi.SwInterface
	bfd    []vppapi.BFDSession
	tables map[uint32]bool
	labels map[uint32]uint32
	routes map[string]int
	fail   error
	dead   bool
	closes int
}

func newFakeDP() *fakeDP {
	return &fakeDP{
		ifs:    []vppapi.SwInterface{{Name: "wg0", SWIfIndex: 1}, {Name: "wg1", SWIfIndex: 2}},
		tables: map[uint32]bool{},
		labels: map[uint32]uint32{},
		routes: map[string]int{},
	}
}

// errVPPRestarted 是"VPP 重启把连接关掉"的等价错误：sr-fib 只认这个才重连。
var errVPPRestarted = vppapi.ErrClosed

func (d *fakeDP) failErr() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dead {
		return errVPPRestarted
	}
	return d.fail
}

func (d *fakeDP) setFail(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.fail = err
}

// setDead 模拟 VPP 进程重启：这只句柄永久断开，账本换新的一份空内存。
func (d *fakeDP) setDead() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dead = true
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
	if err := d.deadErr(); err != nil {
		return nil, err
	}
	return d.ifs, nil
}

func (d *fakeDP) FIBRoutes(context.Context, uint32) ([]vppapi.IPRoute, error) { return nil, nil }
func (d *fakeDP) CLI(context.Context, string) (string, error)                 { return "", nil }

func (d *fakeDP) BFDSessions(context.Context) ([]vppapi.BFDSession, error) {
	if err := d.deadErr(); err != nil {
		return nil, err
	}
	return d.bfd, nil
}

// deadErr 只在连接真死时报错；普通写失败（fail）不影响这两条只读接口。
func (d *fakeDP) deadErr() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dead {
		return errVPPRestarted
	}
	return nil
}

// Close 记账：扔死句柄要真的关一次，否则每重启一回就漏一条 socket。
func (d *fakeDP) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closes++
	return nil
}

func (d *fakeDP) closeCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closes
}

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
	// 与 main() 构造的那份一一对应：漏掉 stream/view/wake 的话，watch 那几条
	// 路径在这份夹具上跑出来的就不是产品里的行为。
	f := &fiber{
		dp:        dp,
		applier:   fib.NewApplier(dp),
		hc:        srv.Client(),
		stream:    &http.Client{},
		statusURL: srv.URL,
		view:      map[ribKey]fib.RIBRoute{},
		wake:      make(chan struct{}, 1),
	}
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

// ---------------------------------------------------------------- 缺陷⑦：VPP 重启
//
// 现场读数（VM 台，2026-11-05）：CPE1 的 VPP 10:39:06 起来，fib.log 从 10:39:08
// 起每秒一行 broken pipe，一直到 10:42:30 —— 那 203 行里 sr-fib 的进程 pid 没变过，
// 是人工 pkill 之后才恢复的。也就是说"动态更新路由表"这件事机制上没问题（每轮重算
// 整份计划再差分），出问题的是那条 api 连接：VPP 一重启就把所有客户端连接关掉，
// sr-fib 手里那只句柄永久是死的。
//
// 光重连还不够：VPP 的表/标签/路由只在内存里，重启后是空的，而 Applier 的 prev
// 还记着"上次已经下发成功"。所以下面这条测试钉两件事 —— 死句柄要被扔掉、
// 重连后要按整份计划从头写一遍。

const ribWithVPN100 = `{"rib":{"100":[{"device_id":2,"vrf_id":100,"prefix":"10.100.2.0/24","next_hops":["10.255.0.2"]}]}}`

func snapshotOf(d *fakeDP) (labels map[uint32]uint32, tables map[uint32]bool, routes map[string]int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ls := map[uint32]uint32{}
	for k, v := range d.labels {
		ls[k] = v
	}
	ts := map[uint32]bool{}
	for k, v := range d.tables {
		ts[k] = v
	}
	rs := map[string]int{}
	for k, v := range d.routes {
		rs[k] = v
	}
	return ls, ts, rs
}

func convergeOnce(f *fiber) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.converge(context.Background())
}

func TestVPPRestartSelfHealsOnNextRound(t *testing.T) {
	f, dp1, stub := newFiberForTest(t)
	stub.body.Store(ribWithVPN100)
	body := `{"vrfs":[0,100],"tunnels":[{"next_hop":"10.255.0.2","interface":"wg0"}]}`
	if rec, _ := putConfig(t, f, body); rec.Code != http.StatusOK {
		t.Fatalf("前置 PUT = %d: %s", rec.Code, rec.Body.String())
	}
	wantL, wantT, wantR := snapshotOf(dp1)
	if wantL[1100] != 100 || !wantT[100] || wantR["10.100.2.0/24@100"] != 1 {
		t.Fatalf("前置状态就不齐：labels=%v tables=%v routes=%v", wantL, wantT, wantR)
	}

	dp2 := newFakeDP() // VPP 重启后的新进程：内存账本全空
	dials := 0
	f.dial = func(context.Context) (bfdDP, error) {
		dials++
		return dp2, nil
	}
	dp1.setDead()

	if err := convergeOnce(f); !errors.Is(err, vppapi.ErrClosed) {
		t.Fatalf("VPP 重启后第一轮 = %v, want 报 ErrClosed 而不是悄悄成功", err)
	}
	if dials != 0 {
		t.Fatalf("还没确认连接死了就连了（dials=%d）", dials)
	}
	f.mu.Lock()
	held, applier := f.dp, f.applier
	f.mu.Unlock()
	if held != nil || applier != nil {
		t.Fatal("死句柄没扔掉：下一轮还会往它写")
	}

	if err := convergeOnce(f); err != nil {
		t.Fatalf("第二轮应自己重连并收敛成功: %v", err)
	}
	if dials != 1 {
		t.Fatalf("重连次数 = %d, want 1（连接活了以后不该再连）", dials)
	}
	gotL, gotT, gotR := snapshotOf(dp2)
	if gotL[1000] != wantL[1000] || gotL[1100] != wantL[1100] {
		t.Fatalf("重连后标签 = %v, want 与重启前同一份 %v（差分基线没重置就会一条都不写）", gotL, wantL)
	}
	if !gotT[100] {
		t.Fatalf("重连后 table 100 没建出来：%v", gotT)
	}
	if gotR["10.100.2.0/24@100"] != 1 {
		t.Fatalf("重连后客户前缀没装回 FIB：%v（want 10.100.2.0/24@100=1）", gotR)
	}

	// 再走一轮：连接是活的，不该有新连接，也不该有多余的写入。
	if err := convergeOnce(f); err != nil {
		t.Fatalf("重连后的稳定轮收敛失败: %v", err)
	}
	if dials != 1 {
		t.Fatalf("活连接被反复重连（dials=%d）", dials)
	}
	if st := getStatus(t, f); st.Last == nil || !st.Last.Converged {
		t.Fatalf("/status 没如实说恢复成功: %+v", st.Last)
	}
}

// 普通写失败（VPP 忙、sr-route 没起来）不是连接死了：那种时候扔掉句柄只会多一次
// 无谓的重连，把"这一轮为什么没成"糊过去。
func TestOrdinaryFailureKeepsConnection(t *testing.T) {
	f, dp, _ := newFiberForTest(t)
	dials := 0
	f.dial = func(context.Context) (bfdDP, error) {
		dials++
		return newFakeDP(), nil
	}
	dp.setFail(errors.New("vpp 正忙"))
	if err := convergeOnce(f); err == nil || errors.Is(err, vppapi.ErrClosed) {
		t.Fatalf("注入的是普通失败，得到 %v", err)
	}
	if dials != 0 {
		t.Fatalf("普通失败触发了重连（dials=%d）", dials)
	}
	f.mu.Lock()
	held := f.dp
	f.mu.Unlock()
	if held == nil {
		t.Fatal("普通失败把好的连接扔了")
	}
}

// ------------------------------------------------- 缺陷①：惰性断连检测
//
// 现场读数（exp-286, 2026-11-09）：CPE1 重启 VPP 之后 BFD 事件订阅随连接死掉，
// 而 RIB 那时没有变化 —— 没有任何东西会唤醒收敛，sr-fib 抱着死句柄待着，
// FIB 一直空着且一行日志都没有（直到人工 pkill）。事件驱动只覆盖"有人在说话"
// 的时刻，所以这里必须有一条不依赖任何事件的兜底：心跳收敛自己就是探活。
//
// 这条测试不允许调用 f.signal()：它要证明的正是"没有事件也能发现并自愈"。

func TestHealthTickRedialsDeadVPPWithoutAnyEvent(t *testing.T) {
	f, dp1, stub := newFiberForTest(t)
	stub.body.Store(ribWithVPN100)
	body := `{"vrfs":[0,100],"tunnels":[{"next_hop":"10.255.0.2","interface":"wg0"}]}`
	if rec, _ := putConfig(t, f, body); rec.Code != http.StatusOK {
		t.Fatalf("前置 PUT = %d: %s", rec.Code, rec.Body.String())
	}

	dp2 := newFakeDP() // VPP 重启后的新进程
	var dials int32
	f.dial = func(context.Context) (bfdDP, error) {
		atomic.AddInt32(&dials, 1)
		return dp2, nil
	}
	f.healthPoll = 20 * time.Millisecond
	dp1.setDead()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		f.convergeLoop(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, _, routes := snapshotOf(dp2)
		if routes["10.100.2.0/24@100"] == 1 && atomic.LoadInt32(&dials) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("没有事件的情况下 %s 内没自愈：dials=%d routes=%v",
				5*time.Second, atomic.LoadInt32(&dials), routes)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// 自愈之后不该反复重连：连接是活的，心跳轮只做读。
	first := atomic.LoadInt32(&dials)
	time.Sleep(80 * time.Millisecond)
	if got := atomic.LoadInt32(&dials); got != first {
		t.Fatalf("活连接被反复重连（dials %d → %d）", first, got)
	}
	if st := getStatus(t, f); st.Last == nil || !st.Last.Converged {
		t.Fatalf("/status 没如实说恢复成功: %+v", st.Last)
	}
}
