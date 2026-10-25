package frr

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// writeTimeout 是一次 ZAPI 写的上限。zebra 正常时立刻可写；卡住说明
// 守护进程有问题，让本轮失败、下一轮重连，比无限等下去好。
const writeTimeout = 5 * time.Second

// Client 是到 zebra 的一条 ZAPI 连接。它做两部分事：把我们导出的前缀写成
// TABLE 路由；订阅 BGP 再分发，把客户侧学来的前缀交回调用方。收到的其它
// 消息一律丢弃，只把 ZEBRA_ERROR 留下来 —— 不读 socket 会让 zebra 的发送
// 缓冲反过来堵住它自己。
type Client struct {
	conn net.Conn

	writeMu sync.Mutex

	handlerMu    sync.RWMutex
	routeHandler func(RouteEvent)

	closeOnce sync.Once

	errMu   sync.Mutex
	ioErr   error
	zapiErr error
}

// Dial 连上 zebra 的 ZAPI socket 并发 HELLO。FRR 10 的 socket 默认在
// /run/frr/zserv.api；调用方给的路径必须与 zebra 的 -z 参数一致。
func Dial(ctx context.Context, path string) (*Client, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("frr: 连 %s: %w", path, err)
	}
	c := &Client{conn: conn}
	if err := c.write(ctx, encodeHello(0)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("frr: HELLO: %w", err)
	}
	go c.readLoop()
	return c, nil
}

// Err 返回连接本身的死因（对端关闭、读失败……）。非 nil 后这条连接不能再
// 用：zebra 重启会丢掉所有路由，调用方必须重连并重发整份计划。
func (c *Client) Err() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.ioErr
}

// ZebraError 取走 zebra 最近一次明确拒绝我们的原因。NO_VRF 这类错误只出现
// 在响应里，不看就会以为路由已经下发成功。
func (c *Client) ZebraError() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	err := c.zapiErr
	c.zapiErr = nil
	return err
}

func (c *Client) RouteAdd(ctx context.Context, r Route) error {
	msg, err := encodeRoute(cmdRouteAdd, r)
	if err != nil {
		return err
	}
	return c.write(ctx, msg)
}

func (c *Client) RouteDelete(ctx context.Context, r Route) error {
	msg, err := encodeRoute(cmdRouteDelete, r)
	if err != nil {
		return err
	}
	return c.write(ctx, msg)
}

// SetRouteHandler 注册再分发通知的处理函数。必须在 SubscribeBGP 之前调用：
// zebra 接受订阅后会立刻回放当前已经选中的路由。
func (c *Client) SetRouteHandler(fn func(RouteEvent)) {
	c.handlerMu.Lock()
	c.routeHandler = fn
	c.handlerMu.Unlock()
}

// SubscribeBGP 订阅 zebra 里 BGP 类型的 IPv4 unicast 路由（含立即回放）。
func (c *Client) SubscribeBGP(ctx context.Context) error {
	return c.write(ctx, encodeRedistributeAdd(afiIPv4, routeTypeBGP, 0))
}

func (c *Client) write(ctx context.Context, msg []byte) error {
	if err := c.Err(); err != nil {
		return err
	}
	deadline := time.Now().Add(writeTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	_, err := c.conn.Write(msg)
	return err
}

func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() {
		err = c.conn.Close()
	})
	return err
}

func (c *Client) readLoop() {
	hdr := make([]byte, zapiHeaderSize)
	for {
		if _, err := io.ReadFull(c.conn, hdr); err != nil {
			c.setIOErr(err)
			return
		}
		size := int(binary.BigEndian.Uint16(hdr[0:2]))
		marker := hdr[2]
		version := hdr[3]
		cmd := binary.BigEndian.Uint16(hdr[8:10])
		if marker != zapiHeaderMarker || version != zservVersion {
			c.setIOErr(fmt.Errorf("ZAPI 头不对: marker=0x%02x version=%d", marker, version))
			return
		}
		if size < zapiHeaderSize {
			c.setIOErr(fmt.Errorf("ZAPI 长度非法: %d", size))
			return
		}
		body := make([]byte, size-zapiHeaderSize)
		if _, err := io.ReadFull(c.conn, body); err != nil {
			c.setIOErr(err)
			return
		}
		switch cmd {
		case cmdError:
			c.setZAPIErr(body)
		case cmdRedistRouteAdd, cmdRedistRouteDel:
			ev, err := decodeRedistributeRoute(body, cmd == cmdRedistRouteAdd)
			if err != nil {
				// 线格式对不上就重连：带着一条解歪的流继续跑，后面的
				// 通知只会错得更隐蔽。
				c.setIOErr(fmt.Errorf("解析再分发通知: %w", err))
				return
			}
			c.handlerMu.RLock()
			fn := c.routeHandler
			c.handlerMu.RUnlock()
			if fn != nil {
				fn(ev)
			}
		}
	}
}

func (c *Client) setIOErr(err error) {
	c.errMu.Lock()
	// 主动 Close 也会走到这里；保留第一次的读错误更有信息量。
	if c.ioErr == nil {
		if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
			c.ioErr = fmt.Errorf("zebra 连接已关闭: %w", err)
		} else {
			c.ioErr = err
		}
	}
	c.errMu.Unlock()
}

// setZAPIErr 解码 ZEBRA_ERROR：payload 是 4 字节的 zebra_error_types。
func (c *Client) setZAPIErr(body []byte) {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	if len(body) < 4 {
		c.zapiErr = errors.New("zebra 返回了无法解析的 ZEBRA_ERROR")
		return
	}
	var reason string
	switch binary.BigEndian.Uint32(body[0:4]) {
	case 0:
		reason = "ZEBRA_UNKNOWN_ERROR"
	case 1:
		reason = "ZEBRA_NO_VRF（zebra 没有这个 VRF，检查它是否已经 activate）"
	case 2:
		reason = "ZEBRA_INVALID_MSG_TYPE（版本/命令号不匹配，检查 FRR 版本）"
	default:
		reason = fmt.Sprintf("未知错误码 %d", binary.BigEndian.Uint32(body[0:4]))
	}
	c.zapiErr = errors.New("zebra 拒绝: " + reason)
}
