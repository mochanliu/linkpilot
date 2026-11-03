// Package stun -- RFC 5389 Binding Request/Success 的编解码 + 收发（零依赖）。
//
// 三块：报文格式（本文件）、应答器 reflector.go（控制器侧，内核 UDP）、
// 探测客户端 client.go（盒子侧，一个复用的内核 UDP socket + 对称 NAT 判定）。
//
// 2026-11-03 定案：地址发现走内核 socket，不在 VPP 数据面里做反射器或探针。
// 代价是"探针的源"和"隧道的源"必须自己证明是同一条路 —— 所以 Mapping 里
// Local（本地源）和 Addr（观测到的外层源）是两个都必须上报的字段，控制器拿
// Local 去对数据面接口清单，对不上就不许用这条观测建隧道。
//
// 移植自 develop 的 src/control/internal/dpdiscover（同一份报文代码，
// 删掉了依赖 dp.Client 的 Role/Both 循环）。
package stun

import (
	"encoding/binary"
	"fmt"
)

var magic = [4]byte{0x21, 0x12, 0xA4, 0x42}

const (
	attrMappedAddress    = 0x0001
	attrXorMappedAddress = 0x0020
)

// BindingRequest 构造一条零属性的 Binding Request，txid 必须是 12 字节。
func BindingRequest(txid []byte) []byte {
	m := make([]byte, 20)
	m[0], m[1] = 0x00, 0x01 // class=0 (request), method=1 (binding), length=0
	copy(m[4:8], magic[:])
	copy(m[8:20], txid)
	return m
}

// MappedAddress 从 Binding Success Response 里取 (ip, port)。
//
// XOR-MAPPED-ADDRESS 是规范答案；MAPPED-ADDRESS 只作为早期 reflector 的兜底。
// txid 不匹配、或不是 Binding Success Response，一律返回 ok=false —— 调用方
// 必须把它当作"未知"，而不是"用本地端口凑一个"。
func MappedAddress(data, txid []byte) (string, int, bool) {
	if len(data) < 20 || data[0] != 0x01 || data[1] != 0x01 {
		return "", 0, false
	}
	if !equal(data[8:20], txid) {
		return "", 0, false
	}
	magicCookie := binary.BigEndian.Uint32(magic[:])
	p := 20
	for p+4 <= len(data) {
		t := binary.BigEndian.Uint16(data[p:])
		l := int(binary.BigEndian.Uint16(data[p+2:]))
		if p+4+l > len(data) {
			break
		}
		v := data[p+4 : p+4+l]
		if l >= 8 && v[1] == 0x01 { // family 0x01 = IPv4
			switch t {
			case attrXorMappedAddress:
				port := int(binary.BigEndian.Uint16(v[2:4])) ^ int(magicCookie>>16)
				ip := binary.BigEndian.Uint32(v[4:8]) ^ magicCookie
				return ipv4str(ip), port, true
			case attrMappedAddress:
				port := int(binary.BigEndian.Uint16(v[2:4]))
				ip := binary.BigEndian.Uint32(v[4:8])
				return ipv4str(ip), port, true
			}
		}
		p += 4 + ((l + 3) &^ 3)
	}
	return "", 0, false
}

func equal(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func ipv4str(h uint32) string {
	return fmt.Sprintf("%d.%d.%d.%d",
		(h>>24)&0xFF, (h>>16)&0xFF, (h>>8)&0xFF, h&0xFF)
}
