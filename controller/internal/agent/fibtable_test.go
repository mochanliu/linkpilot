package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"sdwan/controller/internal/vppapi"
)

// fibStub 是假 sr-fib：GET /config 回"现在跑的那份"，PUT /config 把体记下来。
type fibStub struct {
	mu      sync.Mutex
	current string
	puts    []string
	status  int // PUT 的应答码；0 = 200
}

func (s *fibStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/config" {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		io.WriteString(w, s.current)
	case http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		s.puts = append(s.puts, string(b))
		code := s.status
		if code == 0 {
			code = http.StatusOK
		}
		w.WriteHeader(code)
		io.WriteString(w, `{"vrfs":[],"tunnels":[],"escape_all_colors":true,"last":{"converged":true}}`)
	default:
		http.Error(w, "GET or PUT only", http.StatusMethodNotAllowed)
	}
}

func (s *fibStub) putBodies() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.puts...)
}

// fibFixture 造一份"一条隧道已装好"的对账状态 + 一个假 sr-fib。
func fibFixture(t *testing.T, color *uint8, swIf uint32, dump []vppapi.SwInterface, current string) (
	*FibTableSync, *fibStub, *[]string) {

	t.Helper()
	dp := &fakeVPP{dumpIfs: dump}
	l := &Link{cfg: LinkConfig{PeerOverlay: netip.MustParsePrefix("10.250.0.2/32")}}
	l.swIfIndex = swIf
	ts := &TunnelSync{
		links:   map[string]*Link{"t1": l},
		colors:  map[string]*uint8{},
		peerOf:  map[string]uint16{"t1": 2},
		localOf: map[string]netip.Prefix{},
	}
	ts.dp = dp
	if color != nil {
		ts.colors["t1"] = color
	}
	if current == "" {
		current = `{"vrfs":[0],"tunnels":[],"escape_all_colors":true}`
	}
	stub := &fibStub{current: current}
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)

	var logs []string
	var mu sync.Mutex
	f := NewFibTableSync(ts, "/nonexistent/route.json", "/nonexistent/fib.json", func(format string, args ...any) {
		mu.Lock()
		logs = append(logs, fmt.Sprintf(format, args...))
		mu.Unlock()
	})
	f.readVRFs = func() ([]int, error) { return []int{0, 100}, nil }
	f.baseURL = func() (string, error) { return srv.URL, nil }
	return f, stub, &logs
}

// 派生 + 推送：三列各来自不同的地方，这一条钉住它们拼得对。
func TestFibTableDerivesAndPushes(t *testing.T) {
	c := uint8(3)
	f, stub, _ := fibFixture(t, &c, 12,
		[]vppapi.SwInterface{{SWIfIndex: 12, Name: "wg1002"}}, "")
	if err := f.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	bodies := stub.putBodies()
	if len(bodies) != 1 {
		t.Fatalf("PUT 次数 = %d, want 1（体：%v）", len(bodies), bodies)
	}
	var doc fibConfigDoc
	if err := json.Unmarshal([]byte(bodies[0]), &doc); err != nil {
		t.Fatalf("PUT 体不是 JSON: %v", err)
	}
	if len(doc.VRFs) != 2 || doc.VRFs[0] != 0 || doc.VRFs[1] != 100 {
		t.Fatalf("vrfs = %v, want [0 100]（来自 route.json）", doc.VRFs)
	}
	if len(doc.Tunnels) != 1 {
		t.Fatalf("tunnels = %+v, want 1 行", doc.Tunnels)
	}
	row := doc.Tunnels[0]
	if row.NextHop != "10.250.0.2" || row.Interface != "wg1002" || row.Color != 3 {
		t.Fatalf("行 = %+v, want {10.250.0.2 wg1002 3}", row)
	}
	// escape 不是本组件的职责：整份替换时必须原样带回去。
	if !doc.EscapeAllColors {
		t.Fatal("escape_all_colors 被改成了 false：读改写没保住现场")
	}
}

// 表没变就不写：sr-fib 每 10 秒一轮的心跳收敛本来就是幂等的，多写一次只是噪声。
func TestFibTableSkipsPutWhenAlreadyCurrent(t *testing.T) {
	c := uint8(3)
	f, stub, _ := fibFixture(t, &c, 12,
		[]vppapi.SwInterface{{SWIfIndex: 12, Name: "wg1002"}},
		`{"vrfs":[0,100],"tunnels":[{"next_hop":"10.250.0.2","interface":"wg1002","color":3}],"escape_all_colors":true}`)
	if err := f.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got := stub.putBodies(); len(got) != 0 {
		t.Fatalf("表没变却 PUT 了 %d 次：%v", len(got), got)
	}
}

// 老控制器不发 color（wire.TunnelSpec.Color == nil）：整轮拒绝，绝不按 0 装。
func TestFibTableRefusesMissingColor(t *testing.T) {
	f, stub, _ := fibFixture(t, nil, 12,
		[]vppapi.SwInterface{{SWIfIndex: 12, Name: "wg1002"}}, "")
	err := f.Sync(context.Background())
	if err == nil || !strings.Contains(err.Error(), "color") {
		t.Fatalf("缺 color 时 = %v, want 拒绝派生并说明原因", err)
	}
	if got := stub.putBodies(); len(got) != 0 {
		t.Fatalf("拒绝派生了却还 PUT：%v", got)
	}
}

// 口查不到就排除这一行并说明：sr-fib 会因为空接口名拒掉整份配置。
func TestFibTableSkipsRowWithUnknownInterface(t *testing.T) {
	c := uint8(0)
	f, stub, logs := fibFixture(t, &c, 12, nil, "")
	if err := f.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	bodies := stub.putBodies()
	if len(bodies) != 1 {
		t.Fatalf("PUT 次数 = %d, want 1", len(bodies))
	}
	var doc fibConfigDoc
	_ = json.Unmarshal([]byte(bodies[0]), &doc)
	if len(doc.Tunnels) != 0 {
		t.Fatalf("口不在接口表里却推了一行：%+v", doc.Tunnels)
	}
	joined := strings.Join(*logs, "")
	if !strings.Contains(joined, "不在接口表里") {
		t.Fatalf("跳过原因没进日志：%q", joined)
	}
}

// vrfs 空 = 这台盒子还没收到第 4 段：什么都不推，sr-fib 会拒绝空 VPN 集合。
func TestFibTableHoldsWithoutVRFs(t *testing.T) {
	c := uint8(0)
	f, stub, _ := fibFixture(t, &c, 12,
		[]vppapi.SwInterface{{SWIfIndex: 12, Name: "wg1002"}}, "")
	f.readVRFs = func() ([]int, error) { return nil, nil }
	err := f.Sync(context.Background())
	if err == nil || !strings.Contains(err.Error(), "vrfs") {
		t.Fatalf("vrfs 空时 = %v, want 说明原因且不推", err)
	}
	if got := stub.putBodies(); len(got) != 0 {
		t.Fatalf("vrfs 空却 PUT 了：%v", got)
	}
}

// 202 = 已接受、尚未收敛：不是错误，但也绝不能报成"已收敛"。
func TestFibTable202IsAcceptedNotConverged(t *testing.T) {
	c := uint8(1)
	f, stub, logs := fibFixture(t, &c, 12,
		[]vppapi.SwInterface{{SWIfIndex: 12, Name: "wg1002"}}, "")
	stub.status = http.StatusAccepted
	if err := f.Sync(context.Background()); err != nil {
		t.Fatalf("202 不该是错误: %v", err)
	}
	if got := stub.putBodies(); len(got) != 1 {
		t.Fatalf("PUT 次数 = %d, want 1", len(got))
	}
	if joined := strings.Join(*logs, ""); !strings.Contains(joined, "尚未收敛") {
		t.Fatalf("202 被当成收敛了：%q", joined)
	}
}

// sr-fib 连不上（还没起来）不是崩溃信号：记一句原因，下一拍再来。
func TestFibTableRetriesWhenSrFibIsDown(t *testing.T) {
	c := uint8(0)
	f, _, _ := fibFixture(t, &c, 12, []vppapi.SwInterface{{SWIfIndex: 12, Name: "wg1002"}}, "")
	f.baseURL = func() (string, error) { return "http://127.0.0.1:1", nil } // 没人听
	if err := f.Sync(context.Background()); err == nil {
		t.Fatal("连不上 sr-fib 却返回成功")
	}
}

func TestFibRowsFiltersAndSorts(t *testing.T) {
	c := uint8(2)
	ready := &Link{cfg: LinkConfig{PeerOverlay: netip.MustParsePrefix("10.250.0.4/32")}}
	ready.swIfIndex = 21
	notOpen := &Link{cfg: LinkConfig{PeerOverlay: netip.MustParsePrefix("10.250.0.2/32")}}
	ts := &TunnelSync{
		links:  map[string]*Link{"t4": ready, "t2": notOpen},
		colors: map[string]*uint8{"t4": &c},
	}
	rows := ts.FibRows()
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want 只留口已建好的那条", rows)
	}
	if rows[0].TunnelID != "t4" || rows[0].NextHop != "10.250.0.4" || rows[0].SWIfIndex != 21 {
		t.Fatalf("row = %+v", rows[0])
	}
	if rows[0].Color == nil || *rows[0].Color != 2 {
		t.Fatalf("color = %v, want 2", rows[0].Color)
	}
}

func TestReadRouteVRFs(t *testing.T) {
	p := filepath.Join(t.TempDir(), "route.json")
	body := `{"status_addr":"127.0.0.1:8084","tunnels":[{"peer_device_id":2,"next_hop":"10.250.0.2"}],
"vrfs":[{"id":100,"export_rts":["1:1"],"import_rts":["1:1"]},{"id":0,"export_rts":[],"import_rts":[]},{"id":100,"export_rts":[],"import_rts":[]}]}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readRouteVRFs(p)
	if err != nil {
		t.Fatalf("readRouteVRFs: %v", err)
	}
	if len(got) != 2 || got[0] != 0 || got[1] != 100 {
		t.Fatalf("vrfs = %v, want [0 100]（排序、去重、只取 id）", got)
	}
}

func TestFibStatusBase(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "fib.json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		body, want string
	}{
		{`{"status_addr":"127.0.0.1:19011"}`, "http://127.0.0.1:19011"},
		{`{"status_addr":"0.0.0.0:19011"}`, "http://127.0.0.1:19011"},
		{`{"status_addr":"19011"}`, "http://19011"},
	}
	for _, c := range cases {
		got, err := fibStatusBase(write(c.body))
		if err != nil || got != c.want {
			t.Fatalf("%s → %q, %v; want %q", c.body, got, err, c.want)
		}
	}
	if _, err := fibStatusBase(write(`{"status_url":"http://x/status"}`)); err == nil {
		t.Fatal("没有 status_addr 却给出了地址：sr-fib 没开热更入口时要说出来")
	}
}

// emptyFixture：links 为空的 TunnelSync（对账还没跑过，或控制器真的一条隧道都没有）。
func emptyFixture(t *testing.T, synced bool) (*FibTableSync, *fibStub, *[]string) {
	t.Helper()
	ts := &TunnelSync{
		links:   map[string]*Link{},
		colors:  map[string]*uint8{},
		peerOf:  map[string]uint16{},
		localOf: map[string]netip.Prefix{},
	}
	ts.syncedOnce = synced
	ts.dp = &fakeVPP{dumpIfs: []vppapi.SwInterface{}}
	stub := &fibStub{current: `{"vrfs":[0,100],"tunnels":[{"next_hop":"10.250.0.2","interface":"wg1002","color":3}],"escape_all_colors":true}`}
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)

	var logs []string
	var mu sync.Mutex
	f := NewFibTableSync(ts, "/nonexistent/route.json", "/nonexistent/fib.json", func(format string, args ...any) {
		mu.Lock()
		logs = append(logs, fmt.Sprintf(format, args...))
		mu.Unlock()
	})
	f.readVRFs = func() ([]int, error) { return []int{0, 100}, nil }
	f.baseURL = func() (string, error) { return srv.URL, nil }
	return f, stub, &logs
}

// 启动瞬态守卫（2026-11-12 换件现场实测）：TunnelSync 还没完成过一轮对账时，
// rows 是空的 —— 这时推出去的空表会把 sr-fib 里已有的行清掉，sr-fib 按
// 「下一跳不在隧道表里」跳过所有远端路径，直到下一拍补回（实测 ~5s）。
func TestFibTableHoldsWithoutFirstSync(t *testing.T) {
	f, stub, logs := emptyFixture(t, false)
	err := f.Sync(context.Background())
	if err == nil || !strings.Contains(err.Error(), "还没完成过一轮对账") {
		t.Fatalf("首轮对账没跑过时 = %v, want 说明原因且不推", err)
	}
	if got := stub.putBodies(); len(got) != 0 {
		t.Fatalf("隧道事实还没建立却 PUT 了：%v", got)
	}
	if joined := strings.Join(*logs, ""); !strings.Contains(joined, "还没完成过一轮对账") {
		t.Fatalf("原因没进日志：%q", joined)
	}
}

// 对照：对账完成过（哪怕这台盒子真的 0 条隧道），推空表是合法的 —— 那是"删光了"
// 而不是"还没建立"。sr-fib 当前还挂着旧行，所以这次 PUT 必须发生（整份替换语义）。
func TestFibTablePushesEmptyAfterSyncedOnce(t *testing.T) {
	f, stub, _ := emptyFixture(t, true)
	if err := f.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	bodies := stub.putBodies()
	if len(bodies) != 1 {
		t.Fatalf("PUT 次数 = %d, want 1（删光隧道要推空表）", len(bodies))
	}
	var doc fibConfigDoc
	if err := json.Unmarshal([]byte(bodies[0]), &doc); err != nil {
		t.Fatalf("PUT 体不是 JSON: %v", err)
	}
	if len(doc.Tunnels) != 0 {
		t.Fatalf("tunnels = %+v, want 空（控制器删光了）", doc.Tunnels)
	}
}

// weight 行：0/1 不写字节（与老配置同形，diff 不翻行），>1 原样写。
// sr-fib 是 DisallowUnknownFields 的，键名与它的 tunnelConfig 逐字对齐。
func TestFibRowWeightJSON(t *testing.T) {
	b, err := json.Marshal(fibRowDoc{NextHop: "10.0.0.1", Interface: "wg0", Color: 2})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "weight") {
		t.Errorf("默认权重不该出现 weight 键（字节要和老配置同形）: %s", b)
	}
	b, err = json.Marshal(fibRowDoc{NextHop: "10.0.0.1", Interface: "wg0", Color: 2, Weight: 30})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"weight":30`) {
		t.Errorf("weight 30 应写成 \"weight\":30: %s", b)
	}
}

// task 54 的口径：effectiveWeight = 配置值原样（0 = 默认 1，落成"不写"）。
// task 56 接管后 Dynamic 开的行由档位函数输出——那条路径由它自己的测试钉。
func TestEffectiveWeightStatic(t *testing.T) {
	if got := effectiveWeight(FibTunnelRow{Weight: 30}); got != 30 {
		t.Errorf("effectiveWeight(30) = %d, want 30", got)
	}
	if got := effectiveWeight(FibTunnelRow{}); got != 0 {
		t.Errorf("effectiveWeight(缺省) = %d, want 0", got)
	}
}
