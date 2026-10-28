package wire

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The interface inventory rides the heartbeat as an OPTIONAL field, and the
// difference between "not reported" and "the dataplane has no interfaces" is
// load-bearing: the first is what an agent without -vpp-api says, the second is
// what a box with an empty VPP says, and the controller must not render them
// the same way. That distinction lives entirely in nil-vs-empty, so it is worth
// locking down at the JSON boundary.
func TestHeartbeatIfacesNilVersusEmpty(t *testing.T) {
	b, err := json.Marshal(HeartbeatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "ifaces") {
		t.Fatalf("没上报时不该出现 ifaces 字段（nil = 未上报），得到 %s", b)
	}

	empty := []IfaceInfo{}
	b, err = json.Marshal(HeartbeatRequest{Ifaces: &empty})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"ifaces":[]`) {
		t.Fatalf("空清单必须序列化成空数组（= 数据面确实没有接口），得到 %s", b)
	}
}

func TestHeartbeatIfacesRoundTrip(t *testing.T) {
	list := []IfaceInfo{{
		Name:      "host-eth1",
		SWIfIndex: 1,
		MTU:       1500,
		AdminUp:   true,
		LinkUp:    true,
		Addrs:     []string{"10.10.1.2/24"},
	}}
	b, err := json.Marshal(HeartbeatRequest{Ifaces: &list})
	if err != nil {
		t.Fatal(err)
	}
	var back HeartbeatRequest
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Ifaces == nil {
		t.Fatal("反序列化后 Ifaces 变回 nil：未上报与空清单又混在一起了")
	}
	if !reflect.DeepEqual(*back.Ifaces, list) {
		t.Errorf("往返后 = %#v，期望 %#v", *back.Ifaces, list)
	}
}
