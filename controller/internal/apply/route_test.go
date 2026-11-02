package apply

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A box's own route.json, with the keys the controller does not know about. The
// values are deliberately distinctive so a test can prove they survived the merge.
const boxRouteFile = `{
  "tenant": "gui-check",
  "device_id": 2,
  "identity_dir": "/etc/sdwan/identity",
  "serial": "SN-0002",
  "hold_sec": 9,
  "rrs": [{"addr": "203.0.113.9:8081"}],
  "status_addr": "0.0.0.0:8084",
  "tunnels": [{"peer_device_id": 9, "next_hop": "10.68.0.9"}],
  "vrfs": [{"id": 9, "export_rts": ["65000:9999"], "import_rts": []}]
}`

const overlaySegment = `{"tunnels":[{"peer_device_id":3,"next_hop":"10.68.0.3"}],` +
	`"vrfs":[{"id":0,"export_rts":["65000:1000"],"import_rts":["65000:1001"]}]}`

// boxRouteWith returns a route.json whose status_addr points somewhere specific.
func boxRouteWith(t *testing.T, statusAddr string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "route.json")
	text := strings.Replace(boxRouteFile, `"status_addr": "0.0.0.0:8084"`,
		fmt.Sprintf(`"status_addr": %q`, statusAddr), 1)
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func newRouteApplier(t *testing.T, routeConf string) (*Applier, *[]string) {
	t.Helper()
	logs := []string{}
	a := New(Store{Root: t.TempDir()})
	a.RouteConf = routeConf
	a.Logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	return a, &logs
}

func readRouteKeys(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s 合并后不是合法 JSON: %v\n%s", path, err, raw)
	}
	return doc
}

func TestOverlayMergeKeepsBoxKeys(t *testing.T) {
	path := boxRouteWith(t, "0.0.0.0:8084")
	a, _ := newRouteApplier(t, path)

	if note, err := a.applyRoute(context.Background(), Installed{Route: overlaySegment}, false); err != nil || note != "" {
		t.Fatalf("开机合并 = %q, %v", note, err)
	}

	doc := readRouteKeys(t, path)
	for _, k := range []string{"tenant", "device_id", "identity_dir", "serial", "hold_sec",
		"rrs", "status_addr"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("盒子自己的键 %s 被抹掉了", k)
		}
	}
	if got := string(doc["tunnels"]); !strings.Contains(got, "10.68.0.3") || strings.Contains(got, "10.68.0.9") {
		t.Errorf("tunnels 没有被整张替换: %s", got)
	}
	if got := string(doc["vrfs"]); !strings.Contains(got, "65000:1001") || strings.Contains(got, "65000:9999") {
		t.Errorf("vrfs 没有被整张替换: %s", got)
	}
}

func TestOverlayMergeIgnoresUnknownKey(t *testing.T) {
	path := boxRouteWith(t, "0.0.0.0:8084")
	a, logs := newRouteApplier(t, path)

	if _, err := a.applyRoute(context.Background(),
		Installed{Route: `{"vrfs":[],"leak_policy":[{"rt":"65000:1000"}]}`}, false); err != nil {
		t.Fatalf("合并: %v", err)
	}
	// sr-route 用 DisallowUnknownFields 读这个文件：多写一个它不认识的键，
	// 代价是这台盒子的路由面下次根本起不来。
	if _, ok := readRouteKeys(t, path)["leak_policy"]; ok {
		t.Error("不认识的键被写进了盘上那份")
	}
	if !strings.Contains(strings.Join(*logs, "\n"), "leak_policy") {
		t.Errorf("忽略掉的键没有记在日志里: %v", *logs)
	}
}

// overlayRROnly is a 第 4 段 from a tenant that picked one reflector and has
// nothing else to say. The box's own route.json carries an rrs written by hand,
// and a controller-managed list replaces the whole thing -- the same full-
// replacement rule tunnels follows, because a reflector left over from a hand
// 抄 that nobody owns any more is a route path no console can account for.
const overlayRROnly = `{"rrs":[{"id":"rr1","addr":"192.0.2.10:20200"}]}`

// overlayRRSegment has all three keys, which is what a tenant that publishes both
// layers and picked 主+备 sends.
const overlayRRSegment = `{"tunnels":[{"peer_device_id":3,"next_hop":"10.68.0.3"}],` +
	`"vrfs":[{"id":0,"export_rts":["65000:1000"],"import_rts":["65000:1001"]}],` +
	`"rrs":[{"id":"rr1","addr":"192.0.2.10:20200"},{"id":"rr2","addr":"192.0.2.11:20200"}]}`

func TestOverlayRRReplacesTheBoxListAndKeepsTheRest(t *testing.T) {
	path := boxRouteWith(t, "0.0.0.0:8084")
	a, _ := newRouteApplier(t, path)

	if note, err := a.applyRoute(context.Background(), Installed{Route: overlayRROnly}, false); err != nil || note != "" {
		t.Fatalf("合并带 rrs 的第 4 段 = %q, %v", note, err)
	}
	doc := readRouteKeys(t, path)
	got := string(doc["rrs"])
	if !strings.Contains(got, "192.0.2.10:20200") || strings.Contains(got, "203.0.113.9") {
		t.Errorf("rrs 没有整张替换: %s", got)
	}
	// id 也要落盘：设备日志、/status 和以后的热更换都靠它认出这一条是谁。
	if !strings.Contains(got, `"id"`) {
		t.Errorf("rrs 里的 id 被丢了: %s", got)
	}
	for _, k := range []string{"tenant", "device_id", "identity_dir", "serial", "hold_sec",
		"status_addr", "rrs"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("键 %s 没了", k)
		}
	}
	// 这一段没带 tunnels/vrfs，所以盒子那两张表原样留着 —— presence 就是指令。
	if got := string(doc["vrfs"]); !strings.Contains(got, "65000:9999") {
		t.Errorf("没发 vrfs 却动了盒子那张: %s", got)
	}
}

// rrServer is a C-c sr-route: tunnels and vrfs answer 200, and PUT /rrs answers
// 202 with whatever the caller wants the box to read back. `wait` is the exact
// `rr_wait` object, injected as a string because there are three different ones
// worth pinning and a fake that could only say "finished" would hide the half
// this increment is about.
func rrServer(calls *[]pushCall, wait string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*calls = append(*calls, pushCall{r.URL.Path, string(body)})
		switch r.URL.Path {
		case "/rrs":
			w.WriteHeader(http.StatusAccepted)
			if wait == "" {
				fmt.Fprint(w, `{"rrs":[{"id":"rr1","addr":"192.0.2.10:20200"}]}`)
				return
			}
			fmt.Fprintf(w, `{"rrs":[],"rr_wait":%s}`, wait)
		default:
			fmt.Fprint(w, "{}")
		}
	}))
}

const rrWaitPending = `{"pending_addrs":["192.0.2.10:20200"],"retiring_addrs":["203.0.113.9:8081"]}`

// oldRouteServer is an sr-route from before C-c: the two tables are there, the
// reflector seam is not.
func oldRouteServer(calls *[]pushCall) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*calls = append(*calls, pushCall{r.URL.Path, string(body)})
		if r.URL.Path == "/rrs" {
			http.Error(w, "404 page not found", http.StatusNotFound)
			return
		}
		fmt.Fprint(w, "{}")
	}))
}

// C-c 把 rrs 也变成可热更的一张表，顺序必须排在 tunnels/vrfs 之后：新反射器的
// 会话快照里要带隧道表和本地前缀，先推它就等于让新 RR 学到旧策略。
// 换完了（响应没有 rr_wait）不该有任何 note —— 这是三条里唯一一条"都生效了"。
func TestOverlayRRIsHotPushedLastAndSilentWhenDone(t *testing.T) {
	var calls []pushCall
	srv := rrServer(&calls, "")
	defer srv.Close()
	path := boxRouteWith(t, strings.TrimPrefix(srv.URL, "http://"))
	a, logs := newRouteApplier(t, path)

	note, err := a.applyRoute(context.Background(), Installed{Route: overlayRRSegment}, true)
	if err != nil || note != "" {
		t.Fatalf("换完的一版 = %q, %v", note, err)
	}
	if len(calls) != 3 || calls[0].path != "/tunnels" || calls[1].path != "/vrfs" || calls[2].path != "/rrs" {
		t.Fatalf("热推 = %v，想要 /tunnels、/vrfs、/rrs 依次", calls)
	}
	// 整个 rrs 数组原样发过去，id 也在：设备侧 /status 靠它认出这一条是谁。
	if !strings.Contains(calls[2].body, "192.0.2.11:20200") || !strings.Contains(calls[2].body, `"id"`) {
		t.Errorf("/rrs 请求体 = %s", calls[2].body)
	}
	if joined := strings.Join(*logs, "\n"); !strings.Contains(joined, "/rrs（旧会话已摘）") {
		t.Errorf("日志没写清旧会话已摘: %s", joined)
	}
	if got := string(readRouteKeys(t, path)["rrs"]); !strings.Contains(got, "192.0.2.11:20200") {
		t.Errorf("rrs 没落盘: %s", got)
	}
}

// make-before-break 的那一半：sr-route 立刻返回，但新 RR 还没成立、旧反射器还在
// 顶着。此时绝不能记成"已热更"，也不能记成"未热更"——盘上和活表都已经是新的，
// 差的只是那条会话。运维要能在 status.json 里读到「还在换」和去哪看进度。
func TestOverlaySwitchNotFinishedIsANoteNotALie(t *testing.T) {
	var calls []pushCall
	srv := rrServer(&calls, rrWaitPending)
	defer srv.Close()
	path := boxRouteWith(t, strings.TrimPrefix(srv.URL, "http://"))
	a, logs := newRouteApplier(t, path)

	note, err := a.applyRoute(context.Background(), Installed{Route: overlayRRSegment}, true)
	if err != nil {
		t.Fatalf("换 RR 进行中不是 apply 失败: %v", err)
	}
	for _, want := range []string{"已开始热更", "192.0.2.10:20200", "203.0.113.9:8081", "/status"} {
		if !strings.Contains(note, want) {
			t.Fatalf("note = %q，想要带上 %s", note, want)
		}
	}
	if strings.Contains(note, "未热更") {
		t.Errorf("这一版已经推上去了，note 不该说未热更: %q", note)
	}
	if joined := strings.Join(*logs, "\n"); !strings.Contains(joined, "换 RR 已开始") {
		t.Errorf("日志里没说清这一半: %s", joined)
	}
}

// 只有 rrs 的一版（租户只换了 RR，没发隧道表）也要走 /rrs —— presence 就是指令，
// 三条里出现的那条就是要动的那条。开机重放那一半不拨号、不给 note。
func TestOverlayRRAlonePushesOnlyRrs(t *testing.T) {
	var calls []pushCall
	srv := rrServer(&calls, rrWaitPending)
	defer srv.Close()
	path := boxRouteWith(t, strings.TrimPrefix(srv.URL, "http://"))
	a, _ := newRouteApplier(t, path)

	note, err := a.applyRoute(context.Background(), Installed{Route: overlayRROnly}, true)
	if err != nil {
		t.Fatalf("只有 rrs 的一版不该失败: %v", err)
	}
	if len(calls) != 1 || calls[0].path != "/rrs" {
		t.Fatalf("热推 = %v，想要只推 /rrs", calls)
	}
	if !strings.Contains(note, "rrs") {
		t.Fatalf("note = %q，想要带上 rrs", note)
	}
	// 开机重放那一半不需要这条 note：那时 sr-route 还没起来，盘上就是新列表。
	if bootNote, err := a.applyRoute(context.Background(), Installed{Route: overlayRROnly}, false); err != nil || bootNote != "" {
		t.Fatalf("开机重放 = %q, %v", bootNote, err)
	}
}

// 真机上 192.168.1.100 那台 sr-route 还是 C-c 之前的版本，PUT /rrs 是 404。
// 这是一次版本偏差，不是一次错误配置：其余两张表照常热更，rrs 只落盘，
// 并把"要它下次启动才生效"写进 status —— 否则运维以为 RR 已经换了。
func TestOverlayOldSRRouteWithoutRrsEntry(t *testing.T) {
	var calls []pushCall
	srv := oldRouteServer(&calls)
	defer srv.Close()
	path := boxRouteWith(t, strings.TrimPrefix(srv.URL, "http://"))
	a, logs := newRouteApplier(t, path)

	note, err := a.applyRoute(context.Background(), Installed{Route: overlayRRSegment}, true)
	if err != nil {
		t.Fatalf("旧版本 sr-route 没有 /rrs 不该失败: %v", err)
	}
	if len(calls) != 3 || calls[2].path != "/rrs" {
		t.Fatalf("热推 = %v，想要前两张表照常推", calls)
	}
	if !strings.Contains(note, "没有换 RR 的入口") || !strings.Contains(note, "下次启动") {
		t.Fatalf("note = %q，想要明写这台还没升上来", note)
	}
	if strings.Contains(note, "未热更") {
		t.Errorf("tunnels/vrfs 已经生效，不该混进未热更那张列表: %q", note)
	}
	if joined := strings.Join(*logs, "\n"); !strings.Contains(joined, "rrs 已落盘") {
		t.Errorf("日志里没说清 rrs 这一半: %s", joined)
	}
}

// 404 之外的拒绝是真的拒绝了：RR 地址不成对、id 重复之类，设备会把话说清楚。
// 这必须报错并带上 sr-route 的原话——"没换但下次启动就换"和"配置有问题"
// 是两件事，混在一起会让运维去查一个不存在的问题。
func TestOverlayRRRejectedIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rrs" {
			http.Error(w, "rr[1] 与 rr[0] 地址相同，备反射器不是同一条会话", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, "{}")
	}))
	defer srv.Close()
	path := boxRouteWith(t, strings.TrimPrefix(srv.URL, "http://"))
	a, _ := newRouteApplier(t, path)

	note, err := a.applyRoute(context.Background(), Installed{Route: overlayRRSegment}, true)
	if err == nil || !strings.Contains(err.Error(), "地址相同") || !strings.Contains(err.Error(), "已落盘") {
		t.Fatalf("被拒绝 = %q / %v，想要报错并带上 sr-route 的原话", note, err)
	}
	if note != "" {
		t.Errorf("这次是失败，不该同时记成 note: %q", note)
	}
}

func TestOverlayRefusesWhenFileIsAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "route.json")
	a, _ := newRouteApplier(t, path)

	_, err := a.applyRoute(context.Background(), Installed{Route: overlaySegment}, false)
	if err == nil || !strings.Contains(err.Error(), "盘上没有") {
		t.Fatalf("缺文件 = %v, want「盘上没有…得先由运维写一份」", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatal("拒绝之后还是把文件创建出来了")
	}
}

func TestOverlayEmptySegmentChangesNothing(t *testing.T) {
	path := boxRouteWith(t, "0.0.0.0:8084")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	a, logs := newRouteApplier(t, path)

	if note, err := a.applyRoute(context.Background(), Installed{}, false); err != nil || note != "" {
		t.Fatalf("没有第 4 段 = %q, %v", note, err)
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(before) {
		t.Fatalf("O6-b 之前的配置包动了盘上那份: %v", err)
	}
	if len(*logs) != 1 || !strings.Contains((*logs)[0], "没有第 4 段") {
		t.Errorf("没有第 4 段也要说出来: %v", *logs)
	}
}

type pushCall struct{ path, body string }

func recordingServer(calls *[]pushCall) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*calls = append(*calls, pushCall{r.URL.Path, string(body)})
		fmt.Fprint(w, "{}")
	}))
}

func TestOverlayPushSendsBothTables(t *testing.T) {
	var calls []pushCall
	srv := recordingServer(&calls)
	defer srv.Close()
	path := boxRouteWith(t, strings.TrimPrefix(srv.URL, "http://"))
	a, _ := newRouteApplier(t, path)

	note, err := a.applyRoute(context.Background(), Installed{Route: overlaySegment}, true)
	if err != nil || note != "" {
		t.Fatalf("热更 = %q, %v", note, err)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %v, 想要 2 次", calls)
	}
	// tunnels 在前：到表的隧道先把待决的那批路由解出来，反过来会白报一次"还没好"。
	if calls[0].path != "/tunnels" || calls[1].path != "/vrfs" {
		t.Fatalf("顺序 = %s, %s，想要 /tunnels 然后 /vrfs", calls[0].path, calls[1].path)
	}
	if !strings.Contains(calls[0].body, "10.68.0.3") || strings.Contains(calls[0].body, "vrfs") {
		t.Errorf("tunnels 请求体 = %s", calls[0].body)
	}
	if !strings.Contains(calls[1].body, "65000:1001") || strings.Contains(calls[1].body, "tunnels") {
		t.Errorf("vrfs 请求体 = %s", calls[1].body)
	}
	if got := string(readRouteKeys(t, path)["vrfs"]); !strings.Contains(got, "65000:1001") {
		t.Errorf("推了活表却没落盘: %s", got)
	}
}

func TestOverlayPushBootDoesNotDial(t *testing.T) {
	var calls []pushCall
	srv := recordingServer(&calls)
	defer srv.Close()
	path := boxRouteWith(t, strings.TrimPrefix(srv.URL, "http://"))
	a, _ := newRouteApplier(t, path)

	if _, err := a.applyRoute(context.Background(), Installed{Route: overlaySegment}, false); err != nil {
		t.Fatalf("开机重放: %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("开机重放推了 %d 次活表：那时 sr-route 还没起来", len(calls))
	}
}

func TestOverlayPushUnreachableIsNotAnApplyFailure(t *testing.T) {
	// 端口 1 上没人听：这是一次瞬时故障，不是这张表有问题。
	path := boxRouteWith(t, "127.0.0.1:1")
	a, logs := newRouteApplier(t, path)

	note, err := a.applyRoute(context.Background(), Installed{Route: overlaySegment}, true)
	if err != nil {
		t.Fatalf("连不上被判成 apply 失败: %v", err)
	}
	if !strings.Contains(note, "已落盘") || !strings.Contains(note, "未热更") {
		t.Fatalf("note = %q，想要「已落盘…未热更」", note)
	}
	if joined := strings.Join(*logs, "\n"); !strings.Contains(joined, "未热更") {
		t.Errorf("日志里没有明写未热更: %s", joined)
	}
}

func TestOverlayPushWithoutStatusAddrStillWritesDisk(t *testing.T) {
	p := filepath.Join(t.TempDir(), "route.json")
	if err := os.WriteFile(p, []byte(strings.Replace(boxRouteFile,
		`"status_addr": "0.0.0.0:8084",`, "", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	a, _ := newRouteApplier(t, p)

	note, err := a.applyRoute(context.Background(), Installed{Route: overlaySegment}, true)
	if err != nil {
		t.Fatalf("没有 status_addr 不该失败: %v", err)
	}
	if !strings.Contains(note, "status_addr") {
		t.Fatalf("note = %q，想要明写为什么没热更", note)
	}
	if got := string(readRouteKeys(t, p)["vrfs"]); !strings.Contains(got, "65000:1001") {
		t.Errorf("盘上那份没更新: %s", got)
	}
}

func TestOverlayPushRejectedIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "vrfs[0] id 250 是保留表", http.StatusBadRequest)
	}))
	defer srv.Close()
	path := boxRouteWith(t, strings.TrimPrefix(srv.URL, "http://"))
	a, _ := newRouteApplier(t, path)

	note, err := a.applyRoute(context.Background(), Installed{Route: overlaySegment}, true)
	if err == nil || !strings.Contains(err.Error(), "保留表") || !strings.Contains(err.Error(), "已落盘") {
		t.Fatalf("被拒绝 = %q / %v，想要报错并带上 sr-route 的原话", note, err)
	}
	if note != "" {
		t.Errorf("这次是失败，不该同时记成 note: %q", note)
	}
}

func TestRouteStatusBase(t *testing.T) {
	cases := []struct {
		addr, want string
		ok         bool
	}{
		{"0.0.0.0:8084", "http://127.0.0.1:8084", true},
		{"[::]:8084", "http://127.0.0.1:8084", true},
		{"[::1]:8084", "http://127.0.0.1:8084", true},
		{":8084", "http://127.0.0.1:8084", true},
		{"8084", "http://127.0.0.1:8084", true},
		{"127.0.0.1:8084", "http://127.0.0.1:8084", true},
		{"10.68.1.5:8084", "http://10.68.1.5:8084", true},
		{"http://127.0.0.1:8084/status/", "http://127.0.0.1:8084/status", true},
		{"", "", false},
	}
	for _, c := range cases {
		raw := []byte(fmt.Sprintf(`{"status_addr":%q}`, c.addr))
		got, ok := routeStatusBase(raw)
		if ok != c.ok || got != c.want {
			t.Errorf("status_addr %q = %q/%v，想要 %q/%v", c.addr, got, ok, c.want, c.ok)
		}
	}
	if _, ok := routeStatusBase([]byte(`{`)); ok {
		t.Error("坏 JSON 不该推出一个地址")
	}
}

func TestApplyRecordsDiskAheadOfLiveTable(t *testing.T) {
	root := t.TempDir()
	frrConf := filepath.Join(t.TempDir(), "frr.conf")
	path := boxRouteWith(t, "127.0.0.1:1")
	fx := &fakeExec{outputs: map[string]string{}}
	a := newTestApplier(t, root, frrConf, fx)
	a.RouteConf = path

	b := testBundle()
	b.Artifacts.Route = overlaySegment
	if err := a.Apply(context.Background(), b, false); err != nil {
		t.Fatalf("连不上活表不该让 apply 失败: %v", err)
	}
	st, err := Store{Root: root}.ReadStatus()
	if err != nil || !st.OK {
		t.Fatalf("status = %+v, %v", st, err)
	}
	if len(st.Notes) == 0 || !strings.Contains(st.Notes[0], "未热更") {
		t.Fatalf("status.json 没有记下「已落盘、未热更」: %+v", st.Notes)
	}
	if _, err := os.Stat(filepath.Join(root, "versions", "3", "route.json")); err != nil {
		t.Errorf("第 4 段没进版本目录: %v", err)
	}
}

func TestStoreKeepsRouteSegmentPerVersion(t *testing.T) {
	store := Store{Root: t.TempDir()}
	b := testBundle()
	b.Artifacts.Route = overlaySegment
	if _, err := store.Install(b); err != nil {
		t.Fatalf("install: %v", err)
	}
	cur, err := store.Current()
	if err != nil || cur.Route != overlaySegment {
		t.Fatalf("current route = %q, %v", cur.Route, err)
	}

	// 同版本换内容一律拒绝 —— 这正是控制器改一个导通组要另起版本号的原因。
	b2 := b
	b2.Artifacts.Route = `{"vrfs":[]}`
	if _, err := store.Install(b2); err == nil || !strings.Contains(err.Error(), "different content") {
		t.Fatalf("同版本换内容 = %v，想要 different-content", err)
	}

	// O6-b 之前装的版本目录里没有 route.json，读它不能失败。
	oldStore := Store{Root: t.TempDir()}
	if _, err := oldStore.Install(testBundle()); err != nil {
		t.Fatal(err)
	}
	if _, err := oldStore.Current(); err != nil {
		t.Fatalf("旧版本目录读不出来: %v", err)
	}
}
