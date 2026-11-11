package vppapi

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// 复刻 VPP stats socket 服务端的两件事：监听 socket 开 SO_PASSCRED、
// accept 后 sendmsg(零长数据+fd) 然后**立即 close**。找到 Go 客户端 EOF 的真因。
func TestStatSegVppStyleServer(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "stats.sock")

	b := newSegBuilder()
	row := b.vec(2, func(i int) []byte {
		v := make([]byte, 16)
		binary.LittleEndian.PutUint64(v[0:8], uint64(i+1))
		return v
	})
	thread := b.vec(2, func(i int) []byte {
		v := make([]byte, 8)
		if i == 1 {
			binary.LittleEndian.PutUint64(v, b.base+row)
		}
		return v
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

	ln, err := net.Listen("unixpacket", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// SO_PASSCRED 必须设在 LISTENER 上（VPP 就是这么干的，accept 出来的连接继承）。
	raw, err := ln.(*net.UnixListener).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	raw.Control(func(fd uintptr) {
		if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_PASSCRED, 1); err != nil {
			t.Errorf("SO_PASSCRED: %v", err)
		}
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			uc := conn.(*net.UnixConn)
			rights := syscall.UnixRights(int(segFile.Fd()))
			// 与 VPP 相同：零长数据 + fd，发完立即关。
			uc.WriteMsgUnix(nil, rights, nil)
			uc.Close()
		}
	}()

	// 服务端起来再连。
	time.Sleep(100 * time.Millisecond)
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
	fmt.Println("vpp-style server e2e rows:", rows)
}
