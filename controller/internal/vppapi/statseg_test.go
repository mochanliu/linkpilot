package vppapi

import (
	"encoding/binary"
	"testing"
)

// 合成一个 stats 段的 []byte 直接喂解析器——socket+fd 那半边要在真盒子上验
//（exp-296 活体验证），这里钉死的是布局算法：目录条目、指针换算、跨线程
// 求和、names 读取、以及各种出界防御。
type segBuilder struct {
	buf   []byte
	base  uint64
	alloc uint64 // 已分配到的偏移
}

func newSegBuilder() *segBuilder {
	// 段头 40 字节，base 取一个"像 VPP 堆"的地址，alloc 从段尾开始。
	b := &segBuilder{base: 0x00007f5e94400000}
	b.alloc = uint64(4096) // 头 + 一点余量
	b.buf = make([]byte, 1<<20)
	binary.LittleEndian.PutUint64(b.buf[0:8], 2)          // version
	binary.LittleEndian.PutUint64(b.buf[8:16], b.base)    // base
	binary.LittleEndian.PutUint64(b.buf[32:40], b.base+1) // dir_vector 占位，稍后回填
	return b
}

// vec 在段里放一个带 vec 头（长度 u64 在数据前 8 字节）的向量，返回数据区偏移。
func (b *segBuilder) vec(n int, elem func(i int) []byte) uint64 {
	elemLen := 0
	if n > 0 {
		elemLen = len(elem(0))
	}
	binary.LittleEndian.PutUint32(b.buf[b.alloc:b.alloc+4], uint32(n)) // vec 头
	start := b.alloc + 8
	for i := 0; i < n; i++ {
		copy(b.buf[start+uint64(i)*uint64(elemLen):], elem(i))
	}
	b.alloc = start + uint64(elemLen*n)
	return start
}

func TestIfStatsParseSyntheticSegment(t *testing.T) {
	b := newSegBuilder()

	// names：3 个接口。每个名字是独立的小向量（字节直到 NUL）。
	nameStrs := []string{"local0", "wg1009", "tap4096"}
	namePtrs := make([]uint64, len(nameStrs))
	for i, s := range nameStrs {
		start := b.alloc + 8
		copy(b.buf[start:], s)
		binary.LittleEndian.PutUint32(b.buf[b.alloc:b.alloc+4], uint32(len(s)))
		b.alloc = start + uint64(len(s)) + 1
		namePtrs[i] = start
	}
	namesVec := b.vec(len(nameStrs), func(i int) []byte {
		v := make([]byte, 8)
		binary.LittleEndian.PutUint64(v, b.base+namePtrs[i])
		return v
	})

	// rx：两个"线程"向量，各 3 个接口。接口 0：线程0 给 (10, 1000)，线程1 给
	// (3, 300) —— 求和必须 13/1300；接口 1 只有线程 0 有；接口 2 只有线程 1 有。
	rx0 := [][]uint64{{10, 1000}, {100, 10000}, {0, 0}}
	rx1 := [][]uint64{{3, 300}, {0, 0}, {7, 700}}
	mkThread := func(rows [][]uint64) uint64 {
		return b.vec(len(rows), func(i int) []byte {
			v := make([]byte, 16)
			binary.LittleEndian.PutUint64(v[0:8], rows[i][0])
			binary.LittleEndian.PutUint64(v[8:16], rows[i][1])
			return v
		})
	}
	t0, t1 := mkThread(rx0), mkThread(rx1)
	rxVec := b.vec(2, func(i int) []byte {
		v := make([]byte, 8)
		if i == 0 {
			binary.LittleEndian.PutUint64(v, b.base+t0)
		} else {
			binary.LittleEndian.PutUint64(v, b.base+t1)
		}
		return v
	})

	// tx：单线程，接口 1 有计数，其余 0。同样两级：线程向量 → 行向量。
	txRows := [][]uint64{{0, 0}, {90, 9000}, {0, 0}}
	txThread := b.vec(len(txRows), func(i int) []byte {
		v := make([]byte, 16)
		binary.LittleEndian.PutUint64(v[0:8], txRows[i][0])
		binary.LittleEndian.PutUint64(v[8:16], txRows[i][1])
		return v
	})
	txVec := b.vec(1, func(i int) []byte {
		v := make([]byte, 8)
		binary.LittleEndian.PutUint64(v, b.base+txThread)
		return v
	})

	// 目录：四条（多放一条无关条目验证按名匹配）。先给目录向量留出 8 字节
	// vec 头，别踩进上一个向量的数据尾。
	b.alloc += 8
	dirStart := b.alloc
	entry := func(typ int32, union uint64, name string) {
		e := b.alloc
		binary.LittleEndian.PutUint32(b.buf[e:e+4], uint32(typ))
		binary.LittleEndian.PutUint64(b.buf[e+8:e+16], union)
		copy(b.buf[e+16:e+16+statSegNameLen], name)
		b.alloc += statSegDirStride
	}
	entry(2, b.base+0xdeadbeef, "/if/drops") // 无关条目
	entry(4, b.base+namesVec, "/if/names")
	entry(3, b.base+rxVec, "/if/rx")
	entry(3, b.base+txVec, "/if/tx")
	binary.LittleEndian.PutUint32(b.buf[dirStart-8:dirStart-4], 4) // 目录向量长度
	binary.LittleEndian.PutUint64(b.buf[32:40], b.base+dirStart)   // 回填段头 dir_vector

	rows, err := (&StatSeg{data: b.buf}).IfStats()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("接口数 = %d，期望 3：%+v", len(rows), rows)
	}
	// 升序。
	for i := 1; i < len(rows); i++ {
		if rows[i].SwIfIndex <= rows[i-1].SwIfIndex {
			t.Fatalf("接口没按 sw_if_index 升序: %+v", rows)
		}
	}
	rxSum := rows[0]
	if rxSum.SwIfIndex != 0 || rxSum.Name != "local0" ||
		rxSum.RxPackets != 13 || rxSum.RxBytes != 1300 || rxSum.TxPackets != 0 {
		t.Fatalf("接口 0 = %+v，期望 跨线程求和 13/1300", rxSum)
	}
	if rows[1].SwIfIndex != 1 || rows[1].Name != "wg1009" ||
		rows[1].RxPackets != 100 || rows[1].RxBytes != 10000 ||
		rows[1].TxPackets != 90 || rows[1].TxBytes != 9000 {
		t.Fatalf("接口 1 = %+v", rows[1])
	}
	if rows[2].SwIfIndex != 2 || rows[2].Name != "tap4096" ||
		rows[2].RxPackets != 7 || rows[2].RxBytes != 700 {
		t.Fatalf("接口 2 = %+v", rows[2])
	}
}

func TestIfStatsRejectsWrongVersion(t *testing.T) {
	b := newSegBuilder()
	binary.LittleEndian.PutUint64(b.buf[0:8], 1) // v1 段
	if _, err := (&StatSeg{data: b.buf}).IfStats(); err == nil {
		t.Fatal("v1 段应报错")
	}
}

func TestIfStatsRejectsRebuildInProgress(t *testing.T) {
	b := newSegBuilder()
	binary.LittleEndian.PutUint64(b.buf[24:32], 7) // in_progress
	if _, err := (&StatSeg{data: b.buf}).IfStats(); err == nil {
		t.Fatal("目录重建中应报错")
	}
}

func TestIfStatsSurvivesZeroThreadPointer(t *testing.T) {
	// 没跑起来的线程没有向量（指针 0）：跳过而不是报错。
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
		return v // 线程 0 的指针是 0
	})
	b.alloc += 8 // 目录向量自己的 vec 头
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

	rows, err := (&StatSeg{data: b.buf}).IfStats()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].RxPackets != 1 || rows[1].RxPackets != 2 {
		t.Fatalf("rows = %+v", rows)
	}
}
