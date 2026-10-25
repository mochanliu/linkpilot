package frr

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"path/filepath"
	"testing"
	"time"
)

// fakeZebra 是最小 ZAPI 服务端：读 HELLO 和路由报文，可选地回一条 ZEBRA_ERROR。
type fakeZebra struct {
	ln     net.Listener
	path   string
	hello  chan []byte
	routes chan []byte
	conns  chan net.Conn
}

func newFakeZebra(t *testing.T, reject bool) *fakeZebra {
	t.Helper()
	path := filepath.Join(t.TempDir(), "zserv.api")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeZebra{
		ln:     ln,
		path:   path,
		hello:  make(chan []byte, 4),
		routes: make(chan []byte, 16),
		conns:  make(chan net.Conn, 1),
	}
	go f.serve(reject)
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeZebra) serve(reject bool) {
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	f.conns <- conn
	for {
		hdr := make([]byte, zapiHeaderSize)
		if _, err := io.ReadFull(conn, hdr); err != nil {
			return
		}
		size := int(binary.BigEndian.Uint16(hdr[0:2]))
		body := make([]byte, size-zapiHeaderSize)
		if _, err := io.ReadFull(conn, body); err != nil {
			return
		}
		frame := append(append([]byte{}, hdr...), body...)
		switch binary.BigEndian.Uint16(hdr[8:10]) {
		case cmdHello:
			f.hello <- frame
		default:
			f.routes <- frame
			if reject {
				rejectBody := make([]byte, 4)
				binary.BigEndian.PutUint32(rejectBody, 1) // ZEBRA_NO_VRF
				if _, err := conn.Write(message(cmdError, 0, rejectBody)); err != nil {
					return
				}
			}
		}
	}
}

func TestClientHelloAndWrites(t *testing.T) {
	fz := newFakeZebra(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	c, err := Dial(ctx, fz.path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	select {
	case frame := <-fz.hello:
		// HELLO 的 VRF 必须是 0，payload 是 proto/instance/session/notify。
		if binary.BigEndian.Uint32(frame[4:8]) != 0 {
			t.Errorf("HELLO 的 vrf_id 应为 0，实际 %d", binary.BigEndian.Uint32(frame[4:8]))
		}
		if frame[10] != protoSharp || frame[17] != 0 {
			t.Errorf("HELLO payload 不对: %x", frame[10:])
		}
	case <-ctx.Done():
		t.Fatal("没收到 HELLO")
	}

	r := Route{Prefix: netip.MustParsePrefix("10.20.0.0/16"), TableID: 1000, Distance: 1, Metric: 20}
	if err := c.RouteAdd(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := c.RouteDelete(ctx, r); err != nil {
		t.Fatal(err)
	}
	for _, wantCmd := range []uint16{cmdRouteAdd, cmdRouteDelete} {
		select {
		case frame := <-fz.routes:
			if got := binary.BigEndian.Uint16(frame[8:10]); got != wantCmd {
				t.Errorf("命令号 %d want %d", got, wantCmd)
			}
		case <-ctx.Done():
			t.Fatalf("没收到命令 %d", wantCmd)
		}
	}
	if err := c.ZebraError(); err != nil {
		t.Fatalf("不该有 zebra 错误: %v", err)
	}
}

func TestClientReportsZebraError(t *testing.T) {
	fz := newFakeZebra(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := Dial(ctx, fz.path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	r := Route{Prefix: netip.MustParsePrefix("10.20.0.0/16"), TableID: 1000, Distance: 1}
	if err := c.RouteAdd(ctx, r); err != nil {
		t.Fatal(err)
	}
	<-fz.routes
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := c.ZebraError(); err != nil {
			if c.Err() != nil {
				t.Fatalf("连接本身不该出错: %v", c.Err())
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("没有读到 ZEBRA_ERROR")
}

// TestClientDetectsDisconnect 验证 zebra 死后 Err 能报出来：调用方靠它决定
// 重连并重发整份计划。
func TestClientDetectsDisconnect(t *testing.T) {
	fz := newFakeZebra(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := Dial(ctx, fz.path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	<-fz.hello

	conn := <-fz.conns
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.Err() != nil {
			// 连接已死时再写必须立刻报错，而不是静默成功。
			r := Route{Prefix: netip.MustParsePrefix("10.20.0.0/16"), TableID: 1000, Distance: 1}
			if err := c.RouteAdd(ctx, r); err == nil {
				t.Fatal("死连接上的写入没有报错")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("对端关闭后没有观察到连接结束")
}
