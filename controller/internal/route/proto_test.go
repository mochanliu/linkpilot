package route

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

func validRoute() Route {
	return Route{
		DeviceID: 7,
		VRFID:    3,
		Prefix:   "10.1.0.0/16",
		RTs:      []string{"65000:100", "65000:200"},
	}
}

func TestValidateAcceptsCanonicalRoute(t *testing.T) {
	if err := validRoute().Validate(); err != nil {
		t.Fatalf("valid route rejected: %v", err)
	}
}

func TestValidateRejectsNonCanonicalOrBadRoutes(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*Route)
		wants string
	}{
		{"zero device", func(r *Route) { r.DeviceID = 0 }, "device_id"},
		{"host bits set", func(r *Route) { r.Prefix = "10.1.2.3/16" }, "not canonical"},
		{"ipv6 prefix", func(r *Route) { r.Prefix = "2001:db8::/32" }, "not IPv4"},
		{"prefix with space", func(r *Route) { r.Prefix = "10.1.0.0/16 " }, "bad prefix"},
		{"default route ok", func(r *Route) { r.Prefix = "0.0.0.0/0" }, ""},
		{"no RTs", func(r *Route) { r.RTs = nil }, "at least one RT"},
		{"empty RT", func(r *Route) { r.RTs = []string{""} }, "bad RT"},
		{"RT without colon", func(r *Route) { r.RTs = []string{"65000"} }, "ASN:value"},
		{"RT with two colons", func(r *Route) { r.RTs = []string{"65000:1:2"} }, "exactly one"},
		{"RT leading zero", func(r *Route) { r.RTs = []string{"65000:0100"} }, "not canonical"},
		{"RT asn leading zero", func(r *Route) { r.RTs = []string{"065000:100"} }, "not canonical"},
		{"RT negative", func(r *Route) { r.RTs = []string{"-1:100"} }, "bad RT"},
		{"RT duplicate", func(r *Route) { r.RTs = []string{"65000:100", "65000:100"} }, "duplicate RT"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := validRoute()
			c.mut(&r)
			err := r.Validate()
			if c.wants == "" {
				if err != nil {
					t.Fatalf("unexpected rejection: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted, want error containing %q", c.wants)
			}
			if !strings.Contains(err.Error(), c.wants) {
				t.Fatalf("error %q does not mention %q", err, c.wants)
			}
		})
	}
}

func TestValidateRejectsTooManyRTs(t *testing.T) {
	r := validRoute()
	r.RTs = nil
	for i := 0; i <= MaxRTsPerRoute; i++ {
		r.RTs = append(r.RTs, "65000:"+itoa(100+i))
	}
	if err := r.Validate(); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("want RT-count error, got %v", err)
	}
}

func TestValidateTunnels(t *testing.T) {
	ok := [][]Tunnel{
		nil,
		{{Peer: 20, NextHop: "192.168.99.2"}},
		{
			{Peer: 20, NextHop: "192.168.77.1"},
			{Peer: 20, NextHop: "192.168.99.2"}, // parallel tunnels to one peer
			{Peer: 21, NextHop: "192.168.99.3"},
		},
	}
	for i, ts := range ok {
		if err := ValidateTunnels(ts); err != nil {
			t.Fatalf("case %d rejected: %v", i, err)
		}
	}

	bad := map[string][]Tunnel{
		"peer 0":                 {{Peer: 0, NextHop: "192.168.99.2"}},
		"ipv6":                   {{Peer: 20, NextHop: "2001:db8::1"}},
		"unspecified":            {{Peer: 20, NextHop: "0.0.0.0"}},
		"multicast":              {{Peer: 20, NextHop: "224.0.0.1"}},
		"non-canonical next hop": {{Peer: 20, NextHop: "192.168.099.2"}},
		"peers descending":       {{Peer: 21, NextHop: "192.168.99.3"}, {Peer: 20, NextHop: "192.168.99.2"}},
		"next hops descending":   {{Peer: 20, NextHop: "192.168.99.2"}, {Peer: 20, NextHop: "192.168.77.1"}},
		"duplicate tunnel":       {{Peer: 20, NextHop: "192.168.99.2"}, {Peer: 20, NextHop: "192.168.99.2"}},
	}
	for name, ts := range bad {
		if err := ValidateTunnels(ts); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestValidateTunnelsRejectsTooMany(t *testing.T) {
	ts := make([]Tunnel, 0, MaxTunnelsPerDevice+1)
	for i := 0; i <= MaxTunnelsPerDevice; i++ {
		// One peer, increasing next hops, keeps the table canonical.
		ts = append(ts, Tunnel{Peer: 1, NextHop: "10.0." + itoa(i/256) + "." + itoa(i%256)})
	}
	if err := ValidateTunnels(ts); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("want tunnel-count error, got %v", err)
	}
}

func TestSortedTunnels(t *testing.T) {
	in := []Tunnel{
		{Peer: 21, NextHop: "192.168.99.3"},
		{Peer: 20, NextHop: "192.168.99.2"},
		{Peer: 20, NextHop: "192.168.77.1"},
	}
	got := SortedTunnels(in)
	want := []Tunnel{
		{Peer: 20, NextHop: "192.168.77.1"},
		{Peer: 20, NextHop: "192.168.99.2"},
		{Peer: 21, NextHop: "192.168.99.3"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SortedTunnels = %+v, want %+v", got, want)
	}
	if in[0].Peer != 21 {
		t.Fatal("SortedTunnels mutated its input")
	}
	if err := ValidateTunnels(got); err != nil {
		t.Fatalf("sorted table rejected: %v", err)
	}
}

func TestRouteKeyValidate(t *testing.T) {
	k := validRoute().Key()
	if err := k.Validate(); err != nil {
		t.Fatalf("valid key rejected: %v", err)
	}
	bad := []RouteKey{
		{DeviceID: 0, VRFID: 1, Prefix: "10.0.0.0/8"},
		{DeviceID: 1, VRFID: 1, Prefix: "10.0.0.1/8"},
	}
	for _, k := range bad {
		if err := k.Validate(); err == nil {
			t.Fatalf("key %+v accepted", k)
		}
	}
}

func TestCanonicalRT(t *testing.T) {
	ok := map[string]string{
		"65000:100":             "65000:100",
		"065000:0010":           "65000:10",
		"0:0":                   "0:0",
		"4294967295:4294967295": "4294967295:4294967295",
	}
	for in, want := range ok {
		got, err := CanonicalRT(in)
		if err != nil {
			t.Fatalf("CanonicalRT(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("CanonicalRT(%q) = %q, want %q", in, got, want)
		}
	}
	bad := []string{"", ":", ":1", "1:", "a:1", "1:b", "1:2:3", "1:-2", "4294967296:1", " 1:2"}
	for _, in := range bad {
		if got, err := CanonicalRT(in); err == nil {
			t.Fatalf("CanonicalRT(%q) = %q, want error", in, got)
		}
	}
}

func TestEqualAndKey(t *testing.T) {
	a := validRoute()
	b := validRoute()
	if !a.Equal(b) {
		t.Fatal("identical routes differ")
	}
	b.RTs = []string{"65000:200", "65000:100"}
	if a.Equal(b) {
		t.Fatal("RT order should be part of equality (snapshot dedupe depends on it)")
	}
	b = validRoute()
	b.RTs = append(b.RTs, "65000:300")
	if a.Equal(b) {
		t.Fatal("extra RT not detected")
	}
	b = validRoute()
	b.VRFID = 4
	if a.Equal(b) || a.Key() == b.Key() {
		t.Fatal("a different VRF must be a different route")
	}
	b = validRoute()
	b.DeviceID = 8
	if a.Equal(b) || a.Key() == b.Key() {
		t.Fatal("a different origin must be a different route")
	}
	if a.Key() != validRoute().Key() {
		t.Fatal("identical routes produced different keys")
	}
}

func TestCodecRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	buf.Grow(64 << 10)
	want := []Envelope{
		{Type: MsgHello, Version: ProtocolVersion, DeviceID: 42},
		{Type: MsgHelloOK, Version: ProtocolVersion, RRID: "rr-a", HoldSec: 30},
		{Type: MsgAnnounce, Routes: []Route{validRoute()}},
		{Type: MsgWithdraw, Keys: []RouteKey{validRoute().Key()}},
		{Type: MsgEOR},
		{Type: MsgKeepalive},
		{Type: MsgError, Message: "version mismatch"},
	}
	for _, env := range want {
		if err := WriteEnvelope(&buf, env); err != nil {
			t.Fatalf("write %s: %v", env.Type, err)
		}
	}

	br := bufio.NewReaderSize(bytes.NewReader(buf.Bytes()), 256) // force the ErrBufferFull path
	for i, env := range want {
		got, err := ReadEnvelope(br)
		if err != nil {
			t.Fatalf("read #%d: %v", i, err)
		}
		if got.Type != env.Type || got.Version != env.Version || got.DeviceID != env.DeviceID ||
			got.RRID != env.RRID || got.HoldSec != env.HoldSec || got.Message != env.Message {
			t.Fatalf("frame #%d round-tripped as %+v, want %+v", i, got, env)
		}
		if len(got.Routes) != len(env.Routes) {
			t.Fatalf("frame #%d routes %d, want %d", i, len(got.Routes), len(env.Routes))
		}
		for j := range env.Routes {
			if !got.Routes[j].Equal(env.Routes[j]) {
				t.Fatalf("route %d round-tripped as %+v, want %+v", j, got.Routes[j], env.Routes[j])
			}
		}
		if len(got.Keys) != len(env.Keys) || (len(env.Keys) > 0 && got.Keys[0] != env.Keys[0]) {
			t.Fatalf("frame #%d keys round-tripped as %+v", i, got.Keys)
		}
	}
	if _, err := ReadEnvelope(br); !errors.Is(err, io.EOF) {
		t.Fatalf("trailing read = %v, want EOF", err)
	}
}

func TestReadRejectsBadFrames(t *testing.T) {
	cases := map[string]string{
		"no type":      `{"version":1}` + "\n",
		"unknown type": `{"type":"update"}` + "\n",
		"not json":     "{oops}\n",
		"empty line":   "\n",
		"too many routes": `{"type":"announce","routes":[` +
			strings.Repeat(`{},`, MaxRoutesPerMessage) + `{}]}` + "\n",
		// The tunnel-table frame is gone (an RR only reflects); an old box that
		// still sends it must be refused, not silently ignored.
		"retired tunnels type": `{"type":"tunnels","tunnels":[]}` + "\n",
	}
	for name, in := range cases {
		if _, err := ReadEnvelope(bufio.NewReader(strings.NewReader(in))); err == nil {
			t.Fatalf("%s: frame accepted", name)
		}
	}
}

func TestWriteRejectsOversizedFrame(t *testing.T) {
	// One route with a huge RT list can never be valid, but WriteEnvelope must
	// refuse to put it on the wire before the reader has to defend itself.
	big := Envelope{Type: MsgAnnounce, Routes: []Route{validRoute()}}
	big.Routes[0].RTs = []string{strings.Repeat("1", MaxLineBytes)}
	if err := WriteEnvelope(io.Discard, big); err == nil {
		t.Fatal("oversized frame written")
	}
}

func TestReadRejectsOversizedLine(t *testing.T) {
	in := strings.NewReader(strings.Repeat("a", MaxLineBytes+1) + "\n")
	if _, err := ReadEnvelope(bufio.NewReader(in)); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("want size error, got %v", err)
	}
}

func TestSortedRoutesDeterministic(t *testing.T) {
	in := []Route{
		{DeviceID: 2, VRFID: 1, Prefix: "10.0.0.0/8"},
		{DeviceID: 1, VRFID: 2, Prefix: "10.0.0.0/8"},
		{DeviceID: 1, VRFID: 1, Prefix: "10.2.0.0/16"},
		{DeviceID: 1, VRFID: 1, Prefix: "10.0.0.0/8"},
	}
	got := SortedRoutes(in)
	wantOrder := []string{
		"1/1/10.0.0.0/8",
		"1/1/10.2.0.0/16",
		"1/2/10.0.0.0/8",
		"2/1/10.0.0.0/8",
	}
	for i, r := range got {
		gotStr := itoa(int(r.DeviceID)) + "/" + itoa(int(r.VRFID)) + "/" + r.Prefix
		if gotStr != wantOrder[i] {
			t.Fatalf("position %d = %s, want %s", i, gotStr, wantOrder[i])
		}
	}
	// The input must not be reordered in place: callers still hold their copy.
	if in[0].DeviceID != 2 {
		t.Fatal("SortedRoutes mutated its input")
	}

	keys := SortedKeys([]RouteKey{got[2].Key(), got[0].Key()})
	if keys[0] != got[0].Key() || keys[1] != got[2].Key() {
		t.Fatalf("SortedKeys = %+v", keys)
	}
}

func TestHoldDuration(t *testing.T) {
	if d, err := HoldDuration(0); err != nil || d != DefaultHoldSec*time.Second {
		t.Fatalf("zero hold: %v %v", d, err)
	}
	if d, err := HoldDuration(3); err != nil || d != 3*time.Second {
		t.Fatalf("3s hold: %v %v", d, err)
	}
	for _, bad := range []int{-1, 1, 2, 3601} {
		if _, err := HoldDuration(bad); err == nil {
			t.Fatalf("hold %d accepted", bad)
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}
