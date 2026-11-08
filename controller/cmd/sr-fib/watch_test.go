package main

// sr-fib 这一侧的 /watch 消费测试。钉的是 watch.go 顶上那三条不变量，逐条都对应
// 一种"静默不一致"：
//
//   - 每次挂上都是 INIT（整份视图），没有断点续传；
//   - 流一断立刻作废视图并当场补一次全量，而不是维持旧集合再等一拍（§1.3）；
//   - 看不懂的东西一律当不可信：未知事件名、未知 op、半截帧 —— 都重连取全量。
//
// 数据面用 main_test.go 里那份内存假 VPP，所以"装上了/撤掉了"是前后两本账的对比。
// 空闲判死那条（15s 静默 = 3 个心跳周期）不在这里量：那个时长是常量，端到端要等
// 15s，归 lab 判据。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sdwan/controller/internal/fib"
	"sdwan/controller/internal/route"
)

// watchTestGap 是测试里的 poll_sec（重连间隔）。真产品用 1s，这里要的是"断流之后
// 一眨眼就补上"这个形状，不必等一秒。
const watchTestGap = 20 * time.Millisecond

const ribWithVPN100Other = `{"rib":{"100":[{"device_id":2,"vrf_id":100,"prefix":"10.100.5.0/24","next_hops":["10.255.0.2"]}]}}`

// ------------------------------------------------------------------ 桩

// sseStub 是测试里的 sr-route /watch：一条帧队列，投进来的字符串原样上线（帧格式
// 逐字照抄 serveWatch 的 "event: routes\ndata: %s\n\n"，因为这里要测的就是解析器认不认）。
// status != 0 时它一律回那个码，用来演"订阅位被占用"。
type sseStub struct {
	mu     sync.Mutex
	frames chan string
	drop   chan struct{}
	status int
	hits   atomic.Int64
}

func newSSEStub() *sseStub {
	return &sseStub{frames: make(chan string, 8), drop: make(chan struct{})}
}

func (s *sseStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.hits.Add(1)
	s.mu.Lock()
	status, drop := s.status, s.drop
	s.mu.Unlock()
	if status != 0 {
		http.Error(w, "已有另一个订阅者", status)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-drop:
			return
		case f, open := <-s.frames:
			if !open {
				return
			}
			if _, err := io.WriteString(w, f); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

// sendFrame 投一批事件（真 sr-route 一次 flush 就是一帧）。
func (s *sseStub) sendFrame(t *testing.T, b route.WatchBatch) {
	t.Helper()
	payload, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("编码测试帧: %v", err)
	}
	s.frames <- "event: routes\ndata: " + string(payload) + "\n\n"
}

// hangUp 断掉当下这条流。之后的重挂会挂在那里等新帧 —— 正像订阅位空出来了、暂时
// 没有路由变化，而不是服务端跑路。
func (s *sseStub) hangUp() {
	s.mu.Lock()
	close(s.drop)
	s.drop = make(chan struct{})
	s.mu.Unlock()
}

func (s *sseStub) setStatus(code int) {
	s.mu.Lock()
	s.status = code
	s.mu.Unlock()
}

// countingStatus 给 ribStub 记一笔访问数。"视图可用的时候不该再去全量拉 /status"
// 这句话只有靠计数才说得出证据。
type countingStatus struct {
	stub *ribStub
	hits atomic.Int64
}

func (c *countingStatus) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.hits.Add(1)
	c.stub.ServeHTTP(w, r)
}

// watchEnv 是一份同时有 /status 和 /watch 的 sr-route 桩 + 一台收敛器。
// statusURL 指到 /status：/watch 必须由它推出来，这是这个进程唯一的入口。
type watchEnv struct {
	f      *fiber
	dp     *fakeDP
	rib    *ribStub
	status *countingStatus
	watch  *sseStub
}

func newWatchEnv(t *testing.T) *watchEnv {
	t.Helper()
	dp := newFakeDP()
	rib := &ribStub{}
	rib.body.Store(`{"rib":{}}`)
	st := &countingStatus{stub: rib}
	ws := newSSEStub()
	mux := http.NewServeMux()
	mux.Handle("/status", st)
	mux.Handle("/watch", ws)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	// 与 main() 构造的那份同形：少了 stream/view/wake 任何一个，这里跑的就不是产品。
	f := &fiber{
		dp:        dp,
		applier:   fib.NewApplier(dp),
		hc:        srv.Client(),
		stream:    srv.Client(),
		statusURL: srv.URL + "/status",
		view:      map[ribKey]fib.RIBRoute{},
		wake:      make(chan struct{}, 1),
	}
	f.cur.Store(&liveConfig{
		VRFs:    []uint8{0, 100},
		Tunnels: []fib.Tunnel{{NextHop: "10.255.0.2", Interface: "wg0"}},
	})
	return &watchEnv{f: f, dp: dp, rib: rib, status: st, watch: ws}
}

// ------------------------------------------------------------------ 小工具

func upsert(table uint8, dev uint16, vrf uint8, prefix string, nh ...string) route.RouteEvent {
	return route.RouteEvent{Op: route.OpUpsert, Table: table, DeviceID: dev, VRFID: vrf, Prefix: prefix, NextHops: nh}
}

func withdraw(table uint8, dev uint16, vrf uint8, prefix string) route.RouteEvent {
	return route.RouteEvent{Op: route.OpWithdraw, Table: table, DeviceID: dev, VRFID: vrf, Prefix: prefix}
}

// drained 取走一次"该收敛了"的信号。每步都要清干净，否则下一条断言会被上一帧留下的
// 那颗信号骗过。
func drained(f *fiber) bool {
	select {
	case <-f.wake:
		return true
	default:
		return false
	}
}

func snapshotState(f *fiber) (int, bool) {
	f.vmu.Lock()
	defer f.vmu.Unlock()
	return len(f.view), f.viewOK
}

// viewLines 把视图摊成可比对的规范串。顺序由 sortRIBRoutes 定死 —— 没有它，
// 一次 map 迭代顺序变化就会看起来像一次路由变化。
func viewLines(f *fiber) []string {
	f.vmu.Lock()
	out := make([]fib.RIBRoute, 0, len(f.view))
	for _, r := range f.view {
		out = append(out, r)
	}
	f.vmu.Unlock()
	sortRIBRoutes(out)
	return routeLines(out)
}

func routeLines(rs []fib.RIBRoute) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, fmt.Sprintf("%d|%d|%d|%s|%s", r.Table, r.Origin, r.SourceVRF, r.Prefix, strings.Join(r.NextHops, ",")))
	}
	return out
}

func wantLines(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("视图 = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("视图 = %q, want %q", got, want)
		}
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等不到：%s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func installed(dp *fakeDP, key string) bool {
	_, _, routes := snapshotOf(dp)
	n, ok := routes[key]
	return ok && n == 1
}

// ------------------------------------------------------------------ 测试

func TestWatchURLIsTheStreamNextToStatus(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://127.0.0.1:9100/status", "http://127.0.0.1:9100/watch"},
		// 查询串不能带上流：那条 URL 后面挂的是一条活连接，不是缓存键。
		{"http://127.0.0.1:9100/status?ts=1", "http://127.0.0.1:9100/watch"},
		{"http://host:80/rr/a/status", "http://host:80/rr/a/watch"},
	}
	for _, tc := range cases {
		got, err := watchURL(tc.in)
		if err != nil {
			t.Errorf("%s: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("watchURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if _, err := watchURL("http://[::1/status"); err == nil {
		t.Error("坏 URL 应当当场报错，不能推出一条奇怪的流地址")
	}
}

func TestNextWatchBatchReadsExactlyOneFrame(t *testing.T) {
	var touches int
	br := bufio.NewReader(strings.NewReader(": ping\n\nevent: routes\ndata: {\"init\":true,\"events\":[]}\n\n"))
	b, err := nextWatchBatch(br, func() { touches++ })
	if err != nil {
		t.Fatalf("心跳之后的正常帧读不出来: %v", err)
	}
	if !b.Init || len(b.Events) != 0 {
		t.Fatalf("帧内容 = %+v, want 一份空的 INIT", b)
	}
	// 心跳必须算"这条流还在动"：没有它，"没有路由变化"和"连接不再送字节"是同一个样子。
	if touches < 2 {
		t.Errorf("一行都没碰心跳定时器（touch=%d 次），静默判死会误伤健康流", touches)
	}
}

func TestNextWatchBatchRefusesWhatItCannotTrust(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"未知事件名", "event: rib\ndata: {\"events\":[]}\n\n", "未知事件"},
		{"帧体不是 JSON", "data: 不是 JSON\n\n", "解析"},
		{"只有心跳就结束", ": ping\n\n", "帧中间结束"},
		{"帧写到一半", ": ping\n\ndata: {\"events\":[]}", "帧中间结束"},
	}
	for _, tc := range cases {
		_, err := nextWatchBatch(bufio.NewReader(strings.NewReader(tc.body)), nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want 含 %q", tc.name, err, tc.want)
		}
	}
	// 连接从头到尾没送过字节：那是干净的 EOF，交给上层按"流断了"重挂，不是"半截帧"。
	if _, err := nextWatchBatch(bufio.NewReader(strings.NewReader("")), nil); !errors.Is(err, io.EOF) {
		t.Errorf("空流的错误 = %v, want io.EOF", err)
	}
	// data: 后面那个空格是可选的，两种写法都要吃下（SSE 的允许写法）。
	b, err := nextWatchBatch(bufio.NewReader(strings.NewReader(`data:{"events":[{"op":"upsert","table":0,"device_id":2,"vrf_id":0,"prefix":"10.0.0.0/8"}]}`+"\n\n")), nil)
	if err != nil {
		t.Fatalf("不带空格的 data: 读不出来: %v", err)
	}
	if len(b.Events) != 1 || b.Events[0].Prefix != "10.0.0.0/8" {
		t.Fatalf("帧内容 = %+v", b)
	}
}

func TestApplyBatchKeepsAViewTheConvergerCanTrust(t *testing.T) {
	env := newWatchEnv(t)
	f := env.f

	// INIT：整份视图，一次信号。
	f.applyBatch(route.WatchBatch{Init: true, Events: []route.RouteEvent{
		upsert(100, 2, 100, "10.100.2.0/24", "10.255.0.2"),
		upsert(100, 2, 100, "10.100.3.0/24", "10.255.0.2"),
	}})
	if n, ok := snapshotState(f); n != 2 || !ok {
		t.Fatalf("INIT 之后视图 = %d 条可用=%v", n, ok)
	}
	if !drained(f) {
		t.Fatal("INIT 之后没有要求收敛")
	}
	if drained(f) {
		t.Fatal("一批事件只该催一次收敛")
	}

	// 增量：一条新前缀 + 一条撤除。
	f.applyBatch(route.WatchBatch{Events: []route.RouteEvent{
		upsert(0, 3, 0, "10.9.0.0/16", "10.255.0.2"),
		withdraw(100, 2, 100, "10.100.3.0/24"),
	}})
	drained(f)
	wantLines(t, viewLines(f),
		"0|3|0|10.9.0.0/16|10.255.0.2",
		"100|2|100|10.100.2.0/24|10.255.0.2",
	)

	// 同一身份换下一跳（隧道表变了）：视图吃进新的那一组，差分才有东西可推。
	f.applyBatch(route.WatchBatch{Events: []route.RouteEvent{
		upsert(100, 2, 100, "10.100.2.0/24", "10.255.0.2", "10.255.0.9"),
	}})
	drained(f)
	wantLines(t, viewLines(f),
		"0|3|0|10.9.0.0/16|10.255.0.2",
		"100|2|100|10.100.2.0/24|10.255.0.2,10.255.0.9",
	)

	// 再挂一次 = 再来一份 INIT：整个换掉，不做合并、不做续传。
	f.applyBatch(route.WatchBatch{Init: true, Events: []route.RouteEvent{
		upsert(100, 2, 100, "10.100.9.0/24", "10.255.0.2"),
	}})
	drained(f)
	wantLines(t, viewLines(f), "100|2|100|10.100.9.0/24|10.255.0.2")
}

func TestApplyBatchWithoutASnapshotOnlyAsksForAFullRead(t *testing.T) {
	env := newWatchEnv(t)
	f := env.f

	// 半截增量（没有 INIT 在后）不当视图用：一条都不落地，但要把这一轮收敛催起来，
	// 让它去全量补 —— 这就是"不许拿一份来路不全的集合装表"。
	f.applyBatch(route.WatchBatch{Events: []route.RouteEvent{
		upsert(100, 2, 100, "10.100.2.0/24", "10.255.0.2"),
	}})
	if n, ok := snapshotState(f); n != 0 || ok {
		t.Fatalf("没快照时接受了增量：%d 条可用=%v", n, ok)
	}
	if !drained(f) {
		t.Fatal("没快照时也要催一次收敛（那一轮会去全量拉）")
	}
	env.rib.body.Store(ribWithVPN100)
	got, err := f.currentRoutes(context.Background())
	if err != nil {
		t.Fatalf("currentRoutes: %v", err)
	}
	wantLines(t, routeLines(got), "100|2|100|10.100.2.0/24|10.255.0.2")
}

func TestApplyBatchVoidsTheViewOnAnOpItDoesNotKnow(t *testing.T) {
	env := newWatchEnv(t)
	f := env.f

	// 前一半正常落地，后一半是看不懂的操作。
	f.applyBatch(route.WatchBatch{Init: true, Events: []route.RouteEvent{
		upsert(100, 2, 100, "10.100.2.0/24", "10.255.0.2"),
	}})
	drained(f)
	f.applyBatch(route.WatchBatch{Events: []route.RouteEvent{
		{Op: "mangle", Table: 0, Prefix: "10.9.9.0/24"},
	}})
	// 视图必须整个作废：只跳过一帧的代价是"这份快照少一条，但没人知道"。
	if n, ok := snapshotState(f); n != 0 || ok {
		t.Fatalf("未知 op 之后视图还是可用的：%d 条可用=%v", n, ok)
	}
	if !drained(f) {
		t.Fatal("作废视图之后没有要求收敛")
	}
}

func TestCurrentRoutesUsesTheViewWhileItIsProvable(t *testing.T) {
	env := newWatchEnv(t)
	f := env.f
	env.rib.body.Store(ribWithVPN100)

	// 没有快照：走全量（启动头几轮、断流到重挂之间、以及 409 都是这条路）。
	got, err := f.currentRoutes(context.Background())
	if err != nil {
		t.Fatalf("currentRoutes: %v", err)
	}
	wantLines(t, routeLines(got), "100|2|100|10.100.2.0/24|10.255.0.2")
	if env.status.hits.Load() != 1 {
		t.Fatalf("全量拉了 %d 次, want 1", env.status.hits.Load())
	}

	// 有快照：读事件视图，全量一次都不该再打。两边故意不同一条前缀 ——
	// 视图里是 10.100.7.0/24，/status 里是 10.100.2.0/24，读错来源当场就能看出来。
	f.applyBatch(route.WatchBatch{Init: true, Events: []route.RouteEvent{
		upsert(100, 2, 100, "10.100.7.0/24", "10.255.0.2"),
	}})
	got, err = f.currentRoutes(context.Background())
	if err != nil {
		t.Fatalf("currentRoutes（有快照）: %v", err)
	}
	wantLines(t, routeLines(got), "100|2|100|10.100.7.0/24|10.255.0.2")
	if h := env.status.hits.Load(); h != 1 {
		t.Errorf("事件流可用期间还去全量拉 /status：%d 次", h)
	}
}

// TestStreamBreakFallsBackToTheFullRIB 是 §1.3 在这个进程里的形状：流一断，视图
// 当场不可信，下一轮直接全量补，而不是"先用最后收到的那份继续装表、等重连好了再说"。
func TestStreamBreakFallsBackToTheFullRIB(t *testing.T) {
	env := newWatchEnv(t)
	f := env.f

	f.applyBatch(route.WatchBatch{Init: true, Events: []route.RouteEvent{
		upsert(100, 2, 100, "10.100.7.0/24", "10.255.0.2"),
	}})
	if err := convergeOnce(f); err != nil {
		t.Fatalf("按视图收敛: %v", err)
	}
	if !installed(env.dp, "10.100.7.0/24@100") {
		t.Fatalf("视图里的前缀没装上: %v", installedKeys(env.dp))
	}

	f.dropView(errors.New("测试：流断了"))
	if n, ok := snapshotState(f); n != 0 || ok {
		t.Fatalf("dropView 之后视图还可用：%d 条可用=%v", n, ok)
	}
	if !drained(f) {
		t.Fatal("dropView 没有催收敛：空窗会一直留着")
	}
	env.rib.body.Store(ribWithVPN100Other)
	if err := convergeOnce(f); err != nil {
		t.Fatalf("断流后按全量收敛: %v", err)
	}
	if !installed(env.dp, "10.100.5.0/24@100") {
		t.Errorf("全量补的那条没装上: %v", installedKeys(env.dp))
	}
	if installed(env.dp, "10.100.7.0/24@100") {
		t.Errorf("只在旧视图里的那条还留着 —— 就是留着继续转的那个黑洞: %v", installedKeys(env.dp))
	}
}

func installedKeys(dp *fakeDP) []string {
	_, _, routes := snapshotOf(dp)
	out := make([]string, 0, len(routes))
	for k, n := range routes {
		out = append(out, fmt.Sprintf("%s×%d", k, n))
	}
	sort.Strings(out)
	return out
}

// TestWatchLoopInstallsWhatTheStreamSends 端到端跑一遍这个进程的新泵：
// 挂流 → INIT 装表 → 增量改表，全程不碰 /status；断流后当场换全量。
func TestWatchLoopInstallsWhatTheStreamSends(t *testing.T) {
	env := newWatchEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go env.f.convergeLoop(ctx)
	go env.f.watchLoop(ctx, watchTestGap)

	eventually(t, "sr-fib 挂上了事件流", func() bool { return env.watch.hits.Load() >= 1 })
	env.watch.sendFrame(t, route.WatchBatch{Init: true, Events: []route.RouteEvent{
		upsert(100, 2, 100, "10.100.2.0/24", "10.255.0.2"),
	}})
	eventually(t, "INIT 里的前缀装进了数据面", func() bool {
		return installed(env.dp, "10.100.2.0/24@100")
	})

	// 健康期间：两批增量各改一次表，一次全量都不该发生。
	before := env.status.hits.Load()
	env.watch.sendFrame(t, route.WatchBatch{Events: []route.RouteEvent{
		upsert(100, 2, 100, "10.100.3.0/24", "10.255.0.2"),
	}})
	eventually(t, "增量的前缀装上", func() bool { return installed(env.dp, "10.100.3.0/24@100") })
	env.watch.sendFrame(t, route.WatchBatch{Events: []route.RouteEvent{
		withdraw(100, 2, 100, "10.100.2.0/24"),
	}})
	eventually(t, "撤除的前缀从数据面消失", func() bool {
		return !installed(env.dp, "10.100.2.0/24@100") && installed(env.dp, "10.100.3.0/24@100")
	})
	if got := env.status.hits.Load(); got != before {
		t.Errorf("事件流健康期间还去全量拉了 /status：%d → %d 次", before, got)
	}

	// 断流：换全量补。先把 /status 换成另一份，再断 —— 补上来的必须是这份新的，
	// 且只在流断掉那一轮就补，不等下一次事件。
	env.rib.body.Store(ribWithVPN100Other)
	env.watch.hangUp()
	eventually(t, "断流之后按全量 /status 补齐", func() bool {
		return installed(env.dp, "10.100.5.0/24@100") && !installed(env.dp, "10.100.3.0/24@100")
	})
}

// TestWatchBusySlotStillConvergesFromTheFullRIB：409（订阅位被另一个消费者占着）
// 不是致命错误，也不许把装表堵住。这一条钉的是"没挂上流也得把表收敛对"。
func TestWatchBusySlotStillConvergesFromTheFullRIB(t *testing.T) {
	env := newWatchEnv(t)
	env.watch.setStatus(http.StatusConflict)
	env.rib.body.Store(ribWithVPN100)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go env.f.convergeLoop(ctx)
	go env.f.watchLoop(ctx, watchTestGap)

	eventually(t, "订阅位被占用时仍然从全量装上了路由", func() bool {
		return installed(env.dp, "10.100.2.0/24@100")
	})
	if env.watch.hits.Load() == 0 {
		t.Fatal("一次都没试过挂事件流")
	}
	// 占用期间按重连间隔再试，不是一次性放弃。
	first := env.watch.hits.Load()
	eventually(t, "被占用之后按间隔重试", func() bool { return env.watch.hits.Load() > first })
}
