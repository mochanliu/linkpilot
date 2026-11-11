package vppapi

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"sort"
	"syscall"
	"time"
)

// VPP stats 共享内存段（/run/vpp/stats.sock）的最小只读客户端。
//
// VPP 的二进制 API（api.sock）没有每接口计数器的 dump——接口字节数只有一条
// 正路：stats 段。协议流程（govpp statsclient 同款，VPP ≥ 21.06 是 v2）：
//
//	连接 unixpacket → VPP 不等任何请求，直接把 memfd 通过 SCM_RIGHTS 递过来
//	→ 客户端 mmap 整段 → 段头 → 目录 → 按名字找条目 → 读计数向量。
//
// 布局全部按 26.06 实测钉死（exp-296）：
//   - 段头：version u64@0（=2）、base u64@8、epoch i64@16、in_progress i64@24、
//     dir_vector 指针@32。共享内存里存的都是 VPP 进程的绝对指针，客户端地址 =
//     mmap 基址 + (存值 - header.base)。
//   - 目录：向量长度 u32 在数据前 8 字节（VPP vec 头）；每条 144 字节：
//     type i32@0、union u64@8、name[128]@16。/if/names 是 type 4（名字向量）、
//     /if/rx 与 /if/tx 是 type 3（combined 计数向量）。
//   - combined 向量：数据区是"每线程一个 u64 指针"的数组；每线程向量长度
//     u32 在数据前 8 字节，元素 16 字节 = {packets u64, bytes u64}，下标即
//     sw_if_index。接口总数 = 跨线程求和（多核机器每线程各数各的）。
//
// 读的时候 VPP 可能正在写。单核上 u64 读写在 x86_64 是原子的，最坏读到一帧
// 之内的旧值；跨线程求和的瞬时偏移对 10 秒一个窗口的监控毫无意义。目录被
// 重建的那一瞬间（in_progress ≠ 0）才需要等——重试到它归零为止。

// IfCounter 是一个接口的 rx/tx 累计计数（跨线程求和后的）。SwIfIndex 是
// VPP 的接口号；Name 只在 /if/names 里找得到时才有（排障用，不是数据键）。
type IfCounter struct {
	SwIfIndex uint32
	Name      string
	RxPackets uint64
	RxBytes   uint64
	TxPackets uint64
	TxBytes   uint64
}

const (
	statSegDirStride  = 144 // 目录条目定长：type i32 + pad + union u64 + name[128]
	statSegNameLen    = 128
	statSegMaxIfaces  = 1 << 20 // names/rx/tx 向量长度的防御上限
	statSegMaxThreads = 4096    // 线程向量个数的防御上限
)

// StatSeg 是一个已 mmap 的 stats 段。一个 VPP 进程一生一个段（目录扩容会换
// 段，但那在运行中不会发生），持有者负责 Close（munmap）。
type StatSeg struct {
	data   []byte
	socket string
}

// OpenStatSeg 连上 stats.sock、接住 VPP 递来的 memfd 并 mmap。
// socketPath 一般是 api.sock 同目录下的 stats.sock。
func OpenStatSeg(socketPath string) (*StatSeg, error) {
	conn, err := net.DialTimeout("unixpacket", socketPath, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("vppapi: 连 stats 段 %s: %w", socketPath, err)
	}
	defer conn.Close()
	// VPP 在 accept 后立刻发 fd（数据体为空）。等一半也行——它不发就是坏了。
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	fd, err := recvOneFD(conn)
	if err != nil {
		return nil, fmt.Errorf("vppapi: stats 段 %s 没递 fd: %w", socketPath, err)
	}
	f := os.NewFile(uintptr(fd), "vpp-stats")
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("vppapi: stats 段 fstat: %w", err)
	}
	size := st.Size()
	if size <= 0 || size > 1<<30 {
		f.Close()
		return nil, fmt.Errorf("vppapi: stats 段尺寸可疑（%d B）", size)
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("vppapi: mmap stats 段: %w", err)
	}
	// mmap 拿到了自己的引用，fd 可以关了（Linux mmap 不依赖打开的 fd）。
	// 只走 f.Close() 这一条路，别再 syscall.Close 同一个号——双重 close 会
	// 在别的 goroutine 刚拿到同号 fd 时把它误关。
	f.Close()
	return &StatSeg{data: data, socket: socketPath}, nil
}

// Close 解除 mmap。之后这个对象不能再读。
func (s *StatSeg) Close() error {
	if s == nil || s.data == nil {
		return nil
	}
	err := syscall.Munmap(s.data)
	s.data = nil
	return err
}

// recvOneFD 从 unixpacket 连接上收一个 SCM_RIGHTS fd。
func recvOneFD(conn net.Conn) (int, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return -1, fmt.Errorf("不是 unix 连接（%T）", conn)
	}
	buf := make([]byte, 32)
	oob := make([]byte, 64) // 一个 SCM_RIGHTS 消息远用不了这么多
	n, oobn, _, _, err := uc.ReadMsgUnix(buf, oob)
	// 注意：VPP 发完 fd 立即 close，Go 会把"最后一条消息"和 io.EOF **一起**
	// 返回（oobn=24 且 err=EOF，实测 exp-296）。所以 err 不等于"没消息"——
	// 先解控制消息，fd 到手就赢；真没 fd 才把 err 交出去。
	if err != nil && oobn == 0 {
		return -1, err
	}
	// 另一个坑：Go 自己 Sendmsg 空载荷+fd 会垫一个哑字节，VPP（C 端
	// sendmsg）是真零长包——数据体长度不算数，fd 在不在才算数。
	if oobn == 0 {
		return -1, fmt.Errorf("收到了 %d B 数据但没带控制消息", n)
	}
	scms, perr := syscall.ParseSocketControlMessage(oob[:oobn])
	if perr != nil && err == nil {
		return -1, perr
	}
	for _, scm := range scms {
		fds, ferr := syscall.ParseUnixRights(&scm)
		if ferr != nil {
			continue
		}
		if len(fds) > 0 {
			return fds[0], nil
		}
	}
	if err != nil {
		return -1, err
	}
	return -1, fmt.Errorf("控制消息里没有 fd")
}

// IfStats 读回全部接口的 rx/tx 计数。VPP 正在重建目录时短暂等待重试
//（总计约 100ms），拿不到干净目录就报错——调用方把这一窗丢掉即可，下一个
// 窗口自然恢复。
func (s *StatSeg) IfStats() ([]IfCounter, error) {
	if s == nil || s.data == nil {
		return nil, fmt.Errorf("vppapi: stats 段已关闭")
	}
	for tries := 0; ; tries++ {
		rows, err := s.tryIfStats()
		if err == nil {
			return rows, nil
		}
		if tries >= 25 {
			return nil, err
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (s *StatSeg) tryIfStats() ([]IfCounter, error) {
	d := s.data
	if len(d) < 40 {
		return nil, fmt.Errorf("vppapi: stats 段太短（%d B）", len(d))
	}
	version := binary.LittleEndian.Uint64(d[0:8])
	if version != 2 {
		return nil, fmt.Errorf("vppapi: stats 段版本 %d 不支持（只支持 v2）", version)
	}
	base := binary.LittleEndian.Uint64(d[8:16])
	if int64(binary.LittleEndian.Uint64(d[24:32])) != 0 {
		return nil, fmt.Errorf("vppapi: stats 目录重建中")
	}
	dv, err := adjust(d, base, binary.LittleEndian.Uint64(d[32:40]))
	if err != nil {
		return nil, fmt.Errorf("vppapi: 目录指针出界: %w", err)
	}
	if dv < 8 || dv+statSegDirStride > len(d) {
		return nil, fmt.Errorf("vppapi: 目录向量出界")
	}
	dirLen := int(binary.LittleEndian.Uint32(d[dv-8 : dv-4]))
	if dirLen <= 0 || dirLen > len(d)/statSegDirStride {
		return nil, fmt.Errorf("vppapi: 目录长度可疑（%d）", dirLen)
	}

	var namesVec, rxVec, txVec *uint64
	for i := 0; i < dirLen; i++ {
		e := dv + i*statSegDirStride
		typ := int32(binary.LittleEndian.Uint32(d[e : e+4]))
		union := binary.LittleEndian.Uint64(d[e+8 : e+16])
		nameEnd := e + 16 + statSegNameLen
		name := cstring(d[e+16 : nameEnd])
		switch name {
		case "/if/names":
			if typ == 4 {
				namesVec = &union
			}
		case "/if/rx":
			if typ == 3 {
				rxVec = &union
			}
		case "/if/tx":
			if typ == 3 {
				txVec = &union
			}
		}
		if namesVec != nil && rxVec != nil && txVec != nil {
			break
		}
	}
	if rxVec == nil || txVec == nil {
		return nil, fmt.Errorf("vppapi: stats 目录里没有 /if/rx 或 /if/tx")
	}

	names := map[uint32]string{}
	if namesVec != nil {
		vec, err := adjust(d, base, *namesVec)
		if err == nil {
			n := vecLen(d, vec)
			for i := uint32(0); i < n && i < statSegMaxIfaces; i++ {
				p := binary.LittleEndian.Uint64(d[vec+int(i)*8 : vec+int(i)*8+8])
				if p == 0 {
					continue
				}
				sv, err := adjust(d, base, p)
				if err != nil {
					continue
				}
				l := vecLen(d, sv)
				if l > 256 {
					l = 256
				}
				names[i] = cstring(d[sv : sv+int(l)])
			}
		}
		// 名字读不到不致命：数据键是 sw_if_index。
	}

	rx := sumCombined(d, base, *rxVec)
	tx := sumCombined(d, base, *txVec)

	seen := map[uint32]bool{}
	for idx := range rx {
		seen[idx] = true
	}
	for idx := range tx {
		seen[idx] = true
	}
	out := make([]IfCounter, 0, len(seen))
	for idx := range seen {
		c := IfCounter{SwIfIndex: idx, Name: names[idx]}
		if a, ok := rx[idx]; ok {
			c.RxPackets, c.RxBytes = a[0], a[1]
		}
		if a, ok := tx[idx]; ok {
			c.TxPackets, c.TxBytes = a[0], a[1]
		}
		out = append(out, c)
	}
	// 接口号升序：agent/测试都不想每次读同一个东西顺序不一样。
	sort.Slice(out, func(i, j int) bool { return out[i].SwIfIndex < out[j].SwIfIndex })
	return out, nil
}

// sumCombined 把一个 combined 计数向量按 sw_if_index 跨线程求和。
// 返回 map[sw_if_index]{packets, bytes}。
func sumCombined(d []byte, base, vecPtr uint64) map[uint32][2]uint64 {
	out := map[uint32][2]uint64{}
	vec, err := adjust(d, base, vecPtr)
	if err != nil || vec < 8 {
		return out
	}
	threads := int(vecLen(d, vec))
	if threads > statSegMaxThreads {
		threads = statSegMaxThreads
	}
	for th := 0; th < threads; th++ {
		p := binary.LittleEndian.Uint64(d[vec+th*8 : vec+th*8+8])
		if p == 0 {
			continue // 没跑起来的线程没有向量
		}
		wv, err := adjust(d, base, p)
		if err != nil || wv < 8 {
			continue
		}
		n := int(vecLen(d, wv))
		if max := (len(d) - wv) / 16; n > max {
			n = max // 防御：共享内存说多长都行，段就这么大
		}
		for j := 0; j < n; j++ {
			off := wv + j*16
			pk := binary.LittleEndian.Uint64(d[off : off+8])
			by := binary.LittleEndian.Uint64(d[off+8 : off+16])
			a := out[uint32(j)]
			a[0] += pk
			a[1] += by
			out[uint32(j)] = a
		}
	}
	return out
}

// adjust 把共享内存里存的 VPP 绝对指针换算成 mmap 内偏移，并做范围检查。
func adjust(d []byte, base, ptr uint64) (int, error) {
	if base == 0 || ptr < base {
		return 0, fmt.Errorf("指针 %d 在段基址 %d 之前", ptr, base)
	}
	off := int(ptr - base)
	if off < 0 || off >= len(d) {
		return 0, fmt.Errorf("偏移 %d 出段（%d B）", off, len(d))
	}
	return off, nil
}

// vecLen 读 VPP vec 的长度头（数据区前 8 字节里的 u32）。调用方保证 vec>=8。
func vecLen(d []byte, vec int) uint32 {
	return binary.LittleEndian.Uint32(d[vec-8 : vec-4])
}
