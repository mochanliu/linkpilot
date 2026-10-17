package stun

import (
	"encoding/binary"
	"testing"
)

var magicCookie = binary.BigEndian.Uint32(magic[:])

// xorValue 按 RFC 5389 独立算出 XOR-MAPPED-ADDRESS 的 8 字节属性值，
// 与 MappedAddress 的解码路径互为对照。
func xorValue(ip [4]byte, port int) []byte {
	plain := binary.BigEndian.Uint32(ip[:])
	v := make([]byte, 8)
	v[1] = 0x01
	binary.BigEndian.PutUint16(v[2:4], uint16(port)^uint16(magicCookie>>16))
	binary.BigEndian.PutUint32(v[4:8], plain^magicCookie)
	return v
}

func response(txid []byte, attrs ...[]byte) []byte {
	body := make([]byte, 0, 32)
	for _, a := range attrs {
		hdr := make([]byte, 4)
		binary.BigEndian.PutUint16(hdr[0:2], attrXorMappedAddress)
		binary.BigEndian.PutUint16(hdr[2:4], uint16(len(a)))
		body = append(body, hdr...)
		body = append(body, a...)
		for len(body)%4 != 0 {
			body = append(body, 0)
		}
	}
	m := make([]byte, 20)
	m[0], m[1] = 0x01, 0x01
	binary.BigEndian.PutUint16(m[2:4], uint16(len(body)))
	copy(m[4:8], magic[:])
	copy(m[8:20], txid)
	return append(m, body...)
}

func testTxid() []byte {
	t := make([]byte, 12)
	for i := range t {
		t[i] = byte(0xA0 + i)
	}
	return t
}

func TestBindingRequestShape(t *testing.T) {
	tx := testTxid()
	m := BindingRequest(tx)
	if len(m) != 20 {
		t.Fatalf("len = %d, want 20", len(m))
	}
	if m[0] != 0x00 || m[1] != 0x01 {
		t.Errorf("type = %#02x %#02x, want 00 01 (Binding Request)", m[0], m[1])
	}
	if binary.BigEndian.Uint16(m[2:4]) != 0 {
		t.Errorf("length != 0，零属性请求必须声明长度 0")
	}
	for i := range magic {
		if m[4+i] != magic[i] {
			t.Fatalf("magic cookie 第 %d 字节不符", i)
		}
	}
	for i := range tx {
		if m[8+i] != tx[i] {
			t.Fatalf("txid 第 %d 字节不符", i)
		}
	}
}

func TestMappedAddressXor(t *testing.T) {
	tx := testTxid()
	got, port, ok := MappedAddress(response(tx, xorValue([4]byte{203, 0, 113, 9}, 41245)), tx)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if got != "203.0.113.9" || port != 41245 {
		t.Errorf("got %s:%d, want 203.0.113.9:41245", got, port)
	}
}

func TestMappedAddressPlainFallback(t *testing.T) {
	tx := testTxid()
	v := make([]byte, 8)
	v[1] = 0x01
	binary.BigEndian.PutUint16(v[2:4], 51820)
	copy(v[4:8], []byte{10, 10, 1, 2})
	m := response(tx, v)
	// response() 写的是 XOR 属性号，这里退回 MAPPED-ADDRESS
	binary.BigEndian.PutUint16(m[20:22], attrMappedAddress)
	got, port, ok := MappedAddress(m, tx)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if got != "10.10.1.2" || port != 51820 {
		t.Errorf("got %s:%d, want 10.10.1.2:51820", got, port)
	}
}

func TestMappedAddressRejects(t *testing.T) {
	tx := testTxid()
	good := response(tx, xorValue([4]byte{198, 51, 100, 4}, 3478))

	t.Run("别人的 txid", func(t *testing.T) {
		other := make([]byte, 12)
		copy(other, tx)
		other[0] ^= 0xFF
		if _, _, ok := MappedAddress(good, other); ok {
			t.Error("txid 不匹配却返回了 ok")
		}
	})

	t.Run("不是 Binding Success", func(t *testing.T) {
		m := make([]byte, len(good))
		copy(m, good)
		m[0], m[1] = 0x01, 0x11 // Binding Error Response
		if _, _, ok := MappedAddress(m, tx); ok {
			t.Error("错误响应却返回了 ok")
		}
	})

	t.Run("属性被截断", func(t *testing.T) {
		if _, _, ok := MappedAddress(good[:len(good)-4], tx); ok {
			t.Error("属性被截断却返回了 ok")
		}
	})

	t.Run("非 IPv4 族", func(t *testing.T) {
		v := xorValue([4]byte{203, 0, 113, 9}, 41245)
		v[1] = 0x02
		if _, _, ok := MappedAddress(response(tx, v), tx); ok {
			t.Error("IPv6 族不应按 IPv4 解析")
		}
	})
}

func TestUnknownAttributeIsSkipped(t *testing.T) {
	tx := testTxid()
	// 一条长度 9（非 4 的倍数，需补齐后跳过）的未知属性
	unknown := make([]byte, 4+9+3)
	binary.BigEndian.PutUint16(unknown[0:2], 0x8020)
	binary.BigEndian.PutUint16(unknown[2:4], 9)
	m := response(tx, unknown[:4+9], xorValue([4]byte{203, 0, 113, 9}, 41245))
	got, port, ok := MappedAddress(m, tx)
	if !ok || got != "203.0.113.9" || port != 41245 {
		t.Errorf("未知属性没被正确跳过：got %s:%d ok=%v", got, port, ok)
	}
}
