package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
