package frr

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "state.json")
	routes := []Route{
		{Prefix: netip.MustParsePrefix("10.1.0.0/24"), TableID: 1000, Distance: 1, Metric: 20},
		{Prefix: netip.MustParsePrefix("10.2.0.0/16"), TableID: 1001, Distance: 250, Metric: 0},
	}
	if err := SaveState(path, routes); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != routes[0] || got[1] != routes[1] {
		t.Fatalf("往返不一致: %+v", got)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"prefix": "10.1.0.0/24"`) {
		t.Fatalf("状态文件不是可读 JSON: %s", b)
	}
}

func TestLoadStateMissing(t *testing.T) {
	got, err := LoadState(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || got != nil {
		t.Fatalf("缺失文件应返回空: %v %v", got, err)
	}
}

func TestLoadStateRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"不是 JSON": `not json`,
		"表号为 0":   `{"routes":[{"prefix":"10.0.0.0/8","table_id":0,"distance":1,"metric":0}]}`,
		"未规范前缀":   `{"routes":[{"prefix":"10.0.0.1/8","table_id":1000,"distance":1,"metric":0}]}`,
		"IPv6":    `{"routes":[{"prefix":"2001:db8::/32","table_id":1000,"distance":1,"metric":0}]}`,
	}
	for name, body := range cases {
		path := filepath.Join(dir, "s.json")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadState(path); err == nil {
			t.Errorf("%s: 期望报错", name)
		}
	}
}

func TestRemoveState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := RemoveState(path); err != nil {
		t.Fatalf("删不存在的文件应成功: %v", err)
	}
	if err := SaveState(path, nil); err != nil {
		t.Fatal(err)
	}
	if err := RemoveState(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("文件还在: %v", err)
	}
}
