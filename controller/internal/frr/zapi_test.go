package frr

import (
	"encoding/hex"
	"testing"
)

func TestEncodeHello(t *testing.T) {
	got := encodeHello(0)
	want := "0012fe060000000000131800000000000000"
	if enc := hex.EncodeToString(got); enc != want {
		t.Fatalf("HELLO 编码不对:\n got %s\nwant %s", enc, want)
	}
}
