package ctrladdr

import (
	"os"
	"path/filepath"
	"testing"
)

// 种子是配置里写的那一个地址，表里没有它也必须在候选里。这条是"回得去"的
// 唯一保证：设备被下发到别的节点之后，只有种子还能把它带回起点。
func TestBookKeepsSeedWhenTheTableDropsIt(t *testing.T) {
	b := New("a:8444")
	if got := b.Learn([]string{"b:8444", "c:8444"}, "b:8444"); got != "https://b:8444" {
		t.Fatalf("leader 变了应立刻切过去，得到 %q", got)
	}
	if got := b.List(); len(got) != 3 || got[0] != "https://b:8444" {
		t.Fatalf("候选应是 [b a c]（当前在用的排第一），得到 %v", got)
	}

	// 下一轮表里只剩 c：种子 a 与当前 b 都不能因为"表里没有"被丢掉。
	b.Learn([]string{"c:8444"}, "")
	want := map[string]bool{"https://a:8444": false, "https://b:8444": false, "https://c:8444": false}
	for _, u := range b.List() {
		if _, ok := want[u]; !ok {
			t.Fatalf("候选里出现了不该有的地址 %q（%v）", u, b.List())
		}
		want[u] = true
	}
	for u, seen := range want {
		if !seen {
			t.Fatalf("%s 从候选里消失了（%v）", u, b.List())
		}
	}
}

// 连不上就立刻试下一个，一轮里每个地址只试一次；全试完才交回退避。
func TestBookRotatesThroughEveryCandidateOnce(t *testing.T) {
	b := New("a:8444")
	b.Learn([]string{"b:8444", "c:8444"}, "")

	seen := []string{b.Current()}
	for {
		next, ok := b.Next()
		if !ok {
			break
		}
		seen = append(seen, next)
	}
	if len(seen) != 3 {
		t.Fatalf("一轮应该恰好试 3 个地址，得到 %v", seen)
	}
	if dup := map[string]bool{}; func() bool {
		for _, u := range seen {
			if dup[u] {
				return true
			}
			dup[u] = true
		}
		return false
	}() {
		t.Fatalf("一轮里出现了重复地址 %v", seen)
	}

	// 试完一轮之后 tried 归零，下一轮重新从表头开始（而不是永远返回 false）。
	if _, ok := b.Next(); !ok {
		t.Fatalf("一轮试完之后应重置，下一次要能再给出候选")
	}

	// 成功一次之后，"试过"集合清空，故障可以从零开始数。
	b.OK()
	if _, ok := b.Next(); !ok {
		t.Fatalf("OK() 之后应能从表头重新开始试")
	}
}

// 落盘往返：重启先试上次成功的那个，而不是回到种子。
func TestBookSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ctrl-addrs.json")
	b, err := Load("a:8444", path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	b.Learn([]string{"b:8444", "c:8444"}, "b:8444")
	if b.Current() != "https://b:8444" {
		t.Fatalf("前置条件：当前应是 b，得到 %s", b.Current())
	}
	if err := b.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("落盘文件应存在且 0600，得到 %v %v", fi, err)
	}

	got, err := Load("a:8444", path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Current() != "https://b:8444" {
		t.Fatalf("重启后应先试上次成功的 b，得到 %s", got.Current())
	}
	if l := got.List(); len(l) != 3 || l[0] != "https://b:8444" {
		t.Fatalf("候选应完整恢复（b 在前），得到 %v", l)
	}
}

// 种子换了 = 运维把设备指到别处了。旧候选属于旧集群，不能留在表里。
func TestBookSeedChangeStartsFresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ctrl-addrs.json")
	b, err := Load("a:8444", path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	b.Learn([]string{"b:8444"}, "b:8444")
	if err := b.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if fi, err := os.Stat(path); err != nil {
		t.Fatalf("前置条件：应已落盘，%v", err)
	} else if fi.Size() == 0 {
		t.Fatal("前置条件：落盘文件不该是空的")
	}

	got, err := Load("d:8444", path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cur := got.Current(); cur != "https://d:8444" {
		t.Fatalf("换种子后应从新种子起，得到 %s", cur)
	}
	if l := got.List(); len(l) != 1 || l[0] != "https://d:8444" {
		t.Fatalf("换种子后候选里不该留旧集群的地址，得到 %v", l)
	}
}

// 通配地址永远不进候选：单机 -listen 0.0.0.0 曾经就是被当成"控制器地址"
// 下发的，客户端会去连自己。
func TestBookIgnoresWildcardAddresses(t *testing.T) {
	b := New("a:8444")
	b.Learn([]string{"0.0.0.0:8444", "[::]:8444", "b:8444"}, "")

	for _, u := range b.List() {
		if u == "https://0.0.0.0:8444" || u == "https://[::]:8444" {
			t.Fatalf("通配地址不该进候选：%v", b.List())
		}
	}
	if !contains(b.List(), "https://b:8444") {
		t.Fatalf("正常地址应保留：%v", b.List())
	}
}

// 坏文件不该让进程起不来：候选表是缓存，读不出来就从种子重来。
func TestBookLoadToleratesBadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ctrl-addrs.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load("a:8444", path)
	if err == nil {
		t.Fatalf("坏文件应报错（调用方记一条日志）")
	}
	if cur := got.Current(); cur != "https://a:8444" {
		t.Fatalf("坏文件后应从种子起，得到 %s", cur)
	}
}

func TestNorm(t *testing.T) {
	cases := map[string]string{
		"":                         "",
		" a:8444 ":                 "https://a:8444",
		"https://a:8444/":          "https://a:8444",
		"http://a:8444":            "http://a:8444",
		"https://ctrl.example/":    "https://ctrl.example",
		"192.168.1.100:8444":       "https://192.168.1.100:8444",
		"https://[2001:db8::1]:84": "https://[2001:db8::1]:84",
	}
	for in, want := range cases {
		if got := Norm(in); got != want {
			t.Errorf("Norm(%q) = %q，应是 %q", in, got, want)
		}
	}
}

func contains(list []string, want string) bool {
	for _, u := range list {
		if u == want {
			return true
		}
	}
	return false
}
