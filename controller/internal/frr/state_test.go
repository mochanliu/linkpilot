package frr

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "state.json")
	routes := []Route{
		{Prefix: netip.MustParsePrefix("10.0.0.0/24"), Table: 254, OIF: 1},
		{Prefix: netip.MustParsePrefix("10.2.0.0/16"), Table: 100, OIF: 5},
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
	for _, want := range []string{`"mode": "krt"`, `"table": 100`, `"oif": 5`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("状态文件缺 %s:\n%s", want, b)
		}
	}
}

func TestLoadStateMissing(t *testing.T) {
	got, err := LoadState(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || got != nil {
		t.Fatalf("缺失文件应返回空: %v %v", got, err)
	}
}

// 旧文件属于已退役的 ZAPI/1250 导出：必须被认出来，而不是当新格式去撤
// （那些条目在 zebra 的非主表里，内核 netlink 删不到）。
func TestLoadStateLegacy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	body := `{"routes":[{"prefix":"10.0.0.0/8","table_id":1250,"distance":1,"metric":20}]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(path); !errors.Is(err, ErrLegacyState) {
		t.Fatalf("旧格式应返回 ErrLegacyState，实际 %v", err)
	}
}

func TestLoadStateRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"不是 JSON":      `not json`,
		"未知 mode":      `{"mode":"zapi","routes":[]}`,
		"表号为 0":        `{"mode":"krt","routes":[{"prefix":"10.0.0.0/8","table":0,"oif":1}]}`,
		"表号超过 255":     `{"mode":"krt","routes":[{"prefix":"10.0.0.0/8","table":256,"oif":1}]}`,
		"出口 ifindex 0": `{"mode":"krt","routes":[{"prefix":"10.0.0.0/8","table":100,"oif":0}]}`,
		"未规范前缀":        `{"mode":"krt","routes":[{"prefix":"10.0.0.1/8","table":100,"oif":1}]}`,
		"IPv6":         `{"mode":"krt","routes":[{"prefix":"2001:db8::/32","table":100,"oif":1}]}`,
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
