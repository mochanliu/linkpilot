package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"sdwan/controller/internal/ctrladdr"
)

// 控制器挂了要换下一个，而不是对着同一个地址退避重试到天亮。这条以前是不成立
// 的：mesh.addr 是常量，控制器一挂，网格就冻在最后一份视图上（既有会话还在，
// 但新增/删除 RR、改 RR 地址都不再生效）。
func TestMeshRotatesToTheNextControllerWhenTheFirstIsDown(t *testing.T) {
	var self string
	var hits atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/rr/config" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		_ = json.NewEncoder(w).Encode(rrConfigResponse{
			Tenant: "t1", RRID: "rr1",
			Controllers: []string{self},
		})
	}))
	defer srv.Close()
	self = srv.URL

	book := ctrladdr.New("http://127.0.0.1:1") // 没有监听者的地址
	book.Learn([]string{self}, "")             // 上一轮成功时学到过这台

	m := newMesh(meshParams{book: book, poll: 20 * time.Millisecond, tenant: "t1", rrID: "rr1",
		hc: srv.Client(), logf: t.Logf})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = m.run(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if hits.Load() == 0 {
		t.Fatalf("没有切到活着的控制器（当前 %s，候选 %v）", book.Current(), book.List())
	}
	if got := book.Current(); got != self {
		t.Fatalf("切换后当前地址应是 %s，实际 %s", self, got)
	}
}

// 应答里的 controllers[] 与 leader 要被吸收：表里有别的节点（下次可以试），
// leader 排在表头，候选落盘（重启先试它）。
func TestMeshLearnsControllersAndPersistsThem(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ctrl-addrs-rr1.json")
	var self string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(rrConfigResponse{
			Tenant: "t1", RRID: "rr1",
			Controllers: []string{"10.9.9.9:8444", self},
			Leader:      self,
		})
	}))
	defer srv.Close()
	self = srv.URL

	book, err := ctrladdr.Load(self, path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	m := newMesh(meshParams{book: book, poll: 20 * time.Millisecond, tenant: "t1", rrID: "rr1",
		hc: srv.Client(), logf: t.Logf})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = m.run(ctx) }()

	deadline := time.Now().Add(1500 * time.Millisecond)
	for book.Size() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if book.Size() < 2 {
		t.Fatalf("controllers[] 没有被吸收：候选 %v", book.List())
	}
	if book.Current() != self {
		t.Fatalf("leader 应是当前地址，实际 %s", book.Current())
	}
	// 落盘：候选表必须在盘上，否则重启回到种子（可能已经下线）。
	cancel()
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("候选表没有落盘：%v", err)
	}
	got, err := ctrladdr.Load(self, path)
	if err != nil {
		t.Fatalf("重新 Load: %v", err)
	}
	if got.Size() < 2 {
		t.Fatalf("重启后候选应完整恢复，实际 %v", got.List())
	}
}

// 被控制器删除（404）是致命答案：继续跑就是一个"幽灵 RR"，比退出更糟。
func TestMeshStopsOnFatalAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "RR 不存在", http.StatusNotFound)
	}))
	defer srv.Close()

	book := ctrladdr.New(srv.URL)
	m := newMesh(meshParams{book: book, poll: 20 * time.Millisecond, tenant: "t1", rrID: "rr1",
		hc: srv.Client(), logf: t.Logf})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := m.run(ctx)
	if !errors.Is(err, errMeshFatal) {
		t.Fatalf("404 应返回 errMeshFatal，实际 %v", err)
	}
}
