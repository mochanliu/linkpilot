package vppapi

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// 用一个本机 unix socket 假 VPP，把"连上 → 收 fd → mmap → 解析"的整条链路
// 在宿主机上走通（fd 用普通文件冒充 memfd，mmap 语义相同）。段内容用
// statseg_test.go 的合成器生成。
func TestStatSegSocketEndToEnd(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "stats.sock")

	// 1. 生成段内容并写进"memfd"（普通临时文件）。
	b := newSegBuilder()
	row := b.vec(2, func(i int) []byte {
		v := make([]byte, 16)
		binary.LittleEndian.PutUint64(v[0:8], uint64(i+1)) // 接口 j: rx pkts j+1
		return v
	})
	thread := b.vec(2, func(i int) []byte {
		v := make([]byte, 8)
		if i == 1 {
			binary.LittleEndian.PutUint64(v, b.base+row)
		}
		return v // 线程 0 的指针是 0
	})
	b.alloc += 8
	dirStart := b.alloc
	entry := func(typ int32, union uint64, name string) {
		e := b.alloc
		binary.LittleEndian.PutUint32(b.buf[e:e+4], uint32(typ))
		binary.LittleEndian.PutUint64(b.buf[e+8:e+16], union)
		copy(b.buf[e+16:e+16+statSegNameLen], name)
		b.alloc += statSegDirStride
	}
	entry(3, b.base+thread, "/if/rx")
	entry(3, b.base+thread, "/if/tx")
	binary.LittleEndian.PutUint32(b.buf[dirStart-8:dirStart-4], 2)
	binary.LittleEndian.PutUint64(b.buf[32:40], b.base+dirStart)

	segFile, err := os.CreateTemp(dir, "seg")
	if err != nil {
		t.Fatal(err)
	}
	defer segFile.Close()
	if _, err := segFile.Write(b.buf[:b.alloc+statSegDirStride]); err != nil {
		t.Fatal(err)
	}

	// 2. 假 VPP：accept 后立刻把段文件的 fd 递过去（SCM_RIGHTS）。
	ln, err := net.Listen("unixpacket", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		uc := conn.(*net.UnixConn)
		fh, _ := segFile.Stat()
		payload := make([]byte, fh.Size())
		// 只发 fd，不发数据（和 VPP 一样）。
		rights := syscall.UnixRights(int(segFile.Fd()))
		uc.WriteMsgUnix(payload[:0], rights, nil)
	}()

	// 3. 客户端整条链路。
	s, err := OpenStatSeg(sock)
	if err != nil {
		t.Fatalf("OpenStatSeg: %v", err)
	}
	defer s.Close()
	rows, err := s.IfStats()
	if err != nil {
		t.Fatalf("IfStats: %v", err)
	}
	if len(rows) != 2 || rows[0].RxPackets != 1 || rows[1].RxPackets != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	fmt.Println("socket e2e rows:", rows)
}
