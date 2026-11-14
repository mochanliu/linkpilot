package main

// addrs.go —— 控制器的地址簿。
//
// 集群之后"控制器的地址"不再是常量：leader 会换、节点会加、节点会下线。
// 心跳应答每次都带一份最新地址表（wire.HeartbeatResponse.TLSAddrs + Leader），
// 这里负责把它变成"下一次该连谁"。
//
// 两条规则：
//  1. 换主（leader 变了）→ 直接切过去，不等失败。写只在 leader 上成立，
//     跟着 leader 走能少一次 307 往返。
//  2. 连不上 → 立刻试下一个，**不退避**。退避留给"整个列表都试过一轮"，
//     否则热切换名义上自动、实际要等 30 秒（这正是 P2 要修的那条）。

import (
	"strings"
	"sync"
)

type addrBook struct {
	mu    sync.Mutex
	list  []string        // 完整 base URL（https://host:port）
	cur   string          // 正在用的
	tried map[string]bool // 这一轮已经试过的
}

func newAddrBook(seed string) *addrBook {
	seed = normCtrlURL(seed)
	return &addrBook{list: []string{seed}, cur: seed, tried: map[string]bool{}}
}

func normCtrlURL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if !strings.HasPrefix(s, "https://") && !strings.HasPrefix(s, "http://") {
		s = "https://" + s
	}
	return strings.TrimSuffix(s, "/")
}

func (b *addrBook) current() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cur
}

// learn 吸收一次心跳应答里的地址表，返回"应该立刻切过去"的地址（空 = 不动）。
func (b *addrBook) learn(addrs []string, leader string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	merged := []string{b.cur}
	seen := map[string]bool{b.cur: true}
	push := func(u string) {
		u = normCtrlURL(u)
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		merged = append(merged, u)
	}
	for _, a := range addrs {
		push(a)
	}
	push(leader)
	b.list = merged
	want := normCtrlURL(leader)
	if want != "" && want != b.cur && seen[want] {
		b.cur = want
		b.tried = map[string]bool{}
		return want
	}
	return ""
}

// next 给出下一个要试的地址。返回 false = 这一轮全试过了（该退避了）。
func (b *addrBook) next() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, u := range b.list {
		if u == b.cur || b.tried[u] {
			continue
		}
		b.tried[u] = true
		b.cur = u
		return u, true
	}
	b.tried = map[string]bool{}
	return "", false
}

// ok 在成功之后清空"试过"的集合：下一轮故障从零开始数。
func (b *addrBook) ok() {
	b.mu.Lock()
	b.tried = map[string]bool{}
	b.mu.Unlock()
}

// size 是当前列表长度（日志用）。
func (b *addrBook) size() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.list)
}
