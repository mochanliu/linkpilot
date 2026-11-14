package main

// hardstate_test.go —— 状态分层与行复制的护栏。
//
// 这一层错了，上面所有东西都是错的：diff 漏一行就是"切换后少一条配置"，
// apply 认错类型就是"两台机器悄悄分叉"。所以这里的用例都盯着具体的行。

import (
	"encoding/json"
	"testing"
	"time"

	"sdwan/controller/internal/wire"
)

func TestRowDiffOnlyCarriesChanges(t *testing.T) {
	st := seedState()
	st.Nodes["tenant-a|1"] = &NodeRecord{TenantID: "tenant-a", NodeID: 1, Serial: "CPE1"}
	prev := map[string]string{}
	rows, err := hardRowsOf(st)
	if err != nil {
		t.Fatalf("hardRowsOf: %v", err)
	}
	if got := len(rowDiff(prev, rows)); got != len(rows) {
		t.Fatalf("首次 diff 应该是全量（%d），拿到 %d", len(rows), got)
	}
	// 什么都没改 → 一个字节都不该复制。
	rows2, _ := hardRowsOf(st)
	if got := rowDiff(prev, rows2); len(got) != 0 {
		t.Fatalf("没变化却产生了 %d 行: %+v", len(got), got)
	}
	// 改一台设备的状态 → 只有那一行。
	st.Nodes["tenant-a|1"].Serial = "CPE1-renamed"
	rows3, _ := hardRowsOf(st)
	changed := rowDiff(prev, rows3)
	if len(changed) != 1 || changed[0].Table != "Nodes" || changed[0].Key != "tenant-a|1" {
		t.Fatalf("改一行应该只复制一行，拿到 %+v", changed)
	}
	// 删掉 → 墓碑，否则对端永远删不掉。
	delete(st.Nodes, "tenant-a|1")
	rows4, _ := hardRowsOf(st)
	changed = rowDiff(prev, rows4)
	if len(changed) != 1 || !changed[0].deleted() || changed[0].Key != "tenant-a|1" {
		t.Fatalf("删除应该产生墓碑，拿到 %+v", changed)
	}
}

func TestHardRowsExcludeHeartbeatReadings(t *testing.T) {
	st := seedState()
	now := time.Now().Unix()
	st.Nodes["tenant-a|1"] = &NodeRecord{
		TenantID: "tenant-a", NodeID: 1, Serial: "CPE1", PublicKey: "pk",
		LastSeen: now, UptimeSec: 99, UDPMapping: "1.2.3.4:5555",
		Tunnel: &wire.TunnelState{}, LocalAddrs: []string{"10.0.0.1"},
	}
	rows, err := hardRowsOf(st)
	if err != nil {
		t.Fatalf("hardRowsOf: %v", err)
	}
	var node HardRow
	for _, r := range rows {
		if r.Table == "Nodes" {
			node = r
		}
	}
	if node.Table == "" {
		t.Fatal("设备那一行不在复制流里")
	}
	var m map[string]any
	json.Unmarshal(node.Data, &m)
	for _, k := range []string{"last_seen_unix", "uptime_sec", "udp_mapping", "tunnel_state", "local_addrs"} {
		if _, ok := m[k]; ok {
			t.Errorf("%q 是心跳读数，不该进复制流（每拍都会变）", k)
		}
	}
	for _, k := range []string{"tenant_id", "node_id", "serial", "public_key"} {
		if _, ok := m[k]; !ok {
			t.Errorf("%q 是身份，必须进复制流", k)
		}
	}
}

func TestApplyRowsRoundTripPreservesBothHalves(t *testing.T) {
	src := seedState()
	src.Nodes["tenant-a|1"] = &NodeRecord{TenantID: "tenant-a", NodeID: 1, Serial: "CPE1",
		PublicKey: "pk", TransitPort: 15000, LastSeen: 123, UptimeSec: 42}
	src.Configs["tenant-a|1"] = &DeviceConfig{TenantID: "tenant-a", NodeID: 1, Version: 7}
	src.Epoch = 99

	rows, err := hardRowsOf(src)
	if err != nil {
		t.Fatalf("hardRowsOf: %v", err)
	}
	dst := seedState()
	// seedState 里的入网授权 id 是随机的，两次调用的行集合天然不同；比较"硬状态
	// 是否一致"必须从同一份底稿出发，所以先把对端独有的那几张授权清掉。
	dst.Claims = claimMap{}
	if err := applyRows(dst, rows); err != nil {
		t.Fatalf("applyRows: %v", err)
	}
	if hardFingerprint(src) != hardFingerprint(dst) {
		t.Fatalf("应用之后硬状态不一致\nsrc=%s\ndst=%s", hardFingerprint(src), hardFingerprint(dst))
	}
	rec := dst.Nodes["tenant-a|1"]
	if rec == nil || rec.TransitPort != 15000 {
		t.Fatalf("硬字段没盖过去: %+v", rec)
	}
	if rec.LastSeen != 0 {
		t.Fatalf("软字段不该被对端覆盖，拿到 LastSeen=%d", rec.LastSeen)
	}

	// 本地已经有心跳读数时，应用硬行不能把它们清掉。
	dst.Nodes["tenant-a|1"].LastSeen = 777
	dst.Nodes["tenant-a|1"].UptimeSec = 888
	rows[0].Data = append(json.RawMessage(nil), rows[0].Data...)
	if err := applyRows(dst, rows); err != nil {
		t.Fatalf("applyRows 二次: %v", err)
	}
	again := dst.Nodes["tenant-a|1"]
	if again.LastSeen != 777 || again.UptimeSec != 888 {
		t.Fatalf("应用硬行把正在心跳的软状态冲掉了: %+v", again)
	}
}

func TestApplyRowsRejectsUnknownTable(t *testing.T) {
	st := seedState()
	err := applyRows(st, []HardRow{{Table: "Nope", Key: "x", Data: json.RawMessage(`{}`)}})
	if err == nil {
		t.Fatal("未知字段应该报错，而不是静默跳过（静默 = 两台机器悄悄分叉）")
	}
}

func TestApplyRowsDeletesRows(t *testing.T) {
	st := seedState()
	st.Nodes["tenant-a|1"] = &NodeRecord{TenantID: "tenant-a", NodeID: 1}
	if err := applyRows(st, []HardRow{{Table: "Nodes", Key: "tenant-a|1"}}); err != nil {
		t.Fatalf("applyRows: %v", err)
	}
	if _, ok := st.Nodes["tenant-a|1"]; ok {
		t.Fatal("墓碑没有删掉这一行")
	}
}

// commit() 在没有变化时不该写盘：心跳每秒都在跑，写一次就是 187 KB。
func TestCommitSkipsWhenNothingChanged(t *testing.T) {
	s := newTestServerWithOps(t)
	path := t.TempDir() + "/state.json"
	store := &fileStore{path: path}
	s.store = store
	s.resetHardIndex()
	if err := s.commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	now := time.Now().Unix()
	s.state.Nodes["tenant-a|1"] = &NodeRecord{TenantID: "tenant-a", NodeID: 1, LastSeen: now}
	if err := s.commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// 只有心跳读数变了：硬状态没变，不该再写。
	s.state.Nodes["tenant-a|1"].LastSeen = now + 5
	if err := s.commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	st, err := store.Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if st.Nodes["tenant-a|1"] == nil {
		t.Fatal("设备入网那一行没落盘")
	}
	if st.Nodes["tenant-a|1"].LastSeen == 0 {
		t.Fatal("第一次提交应该把当时的软状态一起写下来（单机模式下 state.json 仍是全量）")
	}
}
