package main

// B4 的这一半：开机向本机 sr-frr 的控制口拉一份客户侧前缀现值。
//
// 和 statusMux 那批用例同一个立场 —— 跑真的 route.Client，不造替身。要验的
// 恰恰是"拉回来的那份有没有真的装进 c.dynamic"，替身只能复述处理函数自己。
//
// 时间预算按本仓库的既有口径放宽到 5–10s：重试节拍（pullRetryEvery = 2s）是
// 生产常量，用例不为了跑得快去改它。

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"sdwan/controller/internal/route"
)

// serveUnix 把一个 handler 挂到一个 unix 套子上，返回套子路径。
// 用 /tmp 下的短目录而不是 t.TempDir()：unix 套子的名字有长度上限
// （Linux 108 字节、macOS 104），带测试名的临时目录很容易越过去。
func serveUnix(t *testing.T, h http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "srp")
	if err != nil {
		t.Fatalf("临时目录: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "c.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("监听 %s: %v", path, err)
	}
	srv := &http.Server{Handler: h}
	go func() {
		_ = srv.Serve(ln)
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = ln.Close()
	})
	return path
}

// vrf5 是一个够格的 VRF：有 export RT，所以 localRoute 会收下它的前缀。
func vrf5() route.ClientConfig {
	return route.ClientConfig{
		VRFs: []route.VRFConfig{{ID: 5, ExportRTs: []string{"65000:5"}, ImportRTs: []string{"65000:5"}}},
	}
}

func TestPullLocalPrefixesAppliesOnce(t *testing.T) {
	var hits int32
	sock := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/local-prefixes" {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		atomic.AddInt32(&hits, 1)
		writeJSON(w, localPrefixesDoc{Prefixes: []localPrefixJSON{
			{VRFID: 5, Prefix: "10.10.0.0/16"},
			{VRFID: 5, Prefix: "10.30.0.0/16"},
		}})
	}))

	c := testClient(t, vrf5())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pullLocalPrefixes(ctx, c, sock)

	got := localPrefixes(c)
	if len(got) != 2 || got[0].Prefix != "10.10.0.0/16" || got[1].Prefix != "10.30.0.0/16" {
		t.Fatalf("拉完的本地前缀集 = %+v，想要那两条", got)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("拉到以后就该退出，不该继续敲（hits=%d）", n)
	}
}

// sr-frr 起得比 sr-route 晚是开机清单的正常次序（start.sh 的 do_route 在
// do_bridge 之前），所以"未就绪"必须等，等到为止。
func TestPullLocalPrefixesWaitsForSRFrrReady(t *testing.T) {
	var hits int32
	sock := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n < 3 {
			http.Error(w, "反向导入未就绪（zebra 未连接或 VRF 设备缺失），请稍后重试", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, localPrefixesDoc{Prefixes: []localPrefixJSON{{VRFID: 5, Prefix: "10.20.0.0/16"}}})
	}))

	c := testClient(t, vrf5())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pullLocalPrefixes(ctx, c, sock)

	if got := localPrefixes(c); len(got) != 1 || got[0].Prefix != "10.20.0.0/16" {
		t.Fatalf("等就绪之后 = %+v，想要那一条", got)
	}
	if n := atomic.LoadInt32(&hits); n != 3 {
		t.Fatalf("hits=%d，想要第 3 次成功即止", n)
	}
}

func TestPullLocalPrefixesStopsWithContext(t *testing.T) {
	sock := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "反向导入未就绪", http.StatusServiceUnavailable)
	}))
	c := testClient(t, vrf5())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pullLocalPrefixes(ctx, c, sock)
		close(done)
	}()
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ctx 结束以后拉取没退出")
	}
}

// 一份拼错了键的应答解出来就是"这一份是空的"，而这是一次整表替换：会把已经
// 宣告出去的前缀全撤掉。所以拉取侧的严格度和 PUT 请求体那条一致。
func TestFetchSRFrrPrefixesRejectsUnknownShape(t *testing.T) {
	sock := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 与 sr-frr 应答同一形状，只把 prefixes 拼成 prefix。
		_ = json.NewEncoder(w).Encode(map[string]any{
			"prefix": []map[string]any{{"vrf_id": 5, "prefix": "10.10.0.0/16"}},
		})
	}))
	_, err := fetchSRFrrPrefixes(context.Background(), unixClient(sock), sock)
	if err == nil {
		t.Fatal("拼错的键必须当错误，不能解成一份空集")
	}
}

// 控制口的阶梯与 sr-apply 的 -srfrr-sock 同一条：显式指定 > 盘上那份的
// control_socket > 缺省。ok=false 只有一种含义：这台盒子没配 sr-frr。
func TestSRFrrControlSocketLadder(t *testing.T) {
	dir := t.TempDir() // 这里只读文件，不监听，路径长度无所谓

	withKey := filepath.Join(dir, "with-key.json")
	if err := os.WriteFile(withKey, []byte(`{"status_url":"http://127.0.0.1:1/status",`+
		`"control_socket":"/var/run/sr-frr/alt.sock","export_vrfs":[5]}`), 0o600); err != nil {
		t.Fatalf("写: %v", err)
	}
	noKey := filepath.Join(dir, "no-key.json")
	if err := os.WriteFile(noKey, []byte(`{"status_url":"http://127.0.0.1:1/status"}`), 0o600); err != nil {
		t.Fatalf("写: %v", err)
	}
	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte(`{这不是 JSON`), 0o600); err != nil {
		t.Fatalf("写: %v", err)
	}

	if s, ok := srfrrControlSocket(withKey, ""); !ok || s != "/var/run/sr-frr/alt.sock" {
		t.Fatalf("盘上写了 control_socket 就该用它：%q ok=%v", s, ok)
	}
	if s, ok := srfrrControlSocket(withKey, "/tmp/explicit.sock"); !ok || s != "/tmp/explicit.sock" {
		t.Fatalf("显式指定要赢：%q ok=%v", s, ok)
	}
	if s, ok := srfrrControlSocket(noKey, ""); !ok || s != defaultSRFrrSocket {
		t.Fatalf("没这个键时退到缺省：%q ok=%v", s, ok)
	}
	if s, ok := srfrrControlSocket(broken, ""); !ok || s != defaultSRFrrSocket {
		t.Fatalf("那份读不懂时也要去缺省口问一次，而不是当作没配：%q ok=%v", s, ok)
	}
	if _, ok := srfrrControlSocket(filepath.Join(dir, "missing.json"), ""); ok {
		t.Fatal("盘上没有 sr-frr 的配置 = 这台盒子不跑它，不该有个循环去敲没人听的套子")
	}
}
