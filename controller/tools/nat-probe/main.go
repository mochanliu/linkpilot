// probe-nat: lab probe for the nat44_ei binary API path. NOT a product binary.
//
// Drives Nat44Enable -> interface features -> pool address -> dumps -> disable
// N times so the API path's stability can be judged by measurement (the CLI
// path OOM-panicked VPP twice and wedged the lab VM; see nat.go's header).
package main

import (
	"context"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"time"

	"sdwan/controller/internal/vppapi"
)

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func main() {
	socket := flag.String("socket", "/run/vpp/api.sock", "vpp api.sock")
	inside := flag.String("inside", "host-enp0s8", "inside interface")
	outside := flag.String("outside", "host-enp0s9", "outside interface")
	repeat := flag.Int("repeat", 5, "round trips")
	users := flag.Uint("users", 4096, "max users per thread")
	userSessions := flag.Uint("user-sessions", 256, "max sessions per user")
	sessions := flag.Uint("sessions", 16384, "max sessions per thread")
	keep := flag.Bool("keep", false, "leave NAT enabled (for traffic tests)")
	pool := flag.String("pool", "", "dedicated pool address for keep mode (else outside interface address)")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	c, err := vppapi.Dial(ctx, *socket, "probe-nat")
	if err != nil {
		fmt.Println("dial:", err)
		os.Exit(1)
	}
	defer c.Close()

	ifs, err := c.DumpInterfaces(ctx)
	if err != nil {
		fmt.Println("interfaces:", err)
		os.Exit(1)
	}
	idx := map[string]uint32{}
	for _, it := range ifs {
		idx[it.Name] = it.SWIfIndex
	}
	inIdx, ok := idx[*inside]
	if !ok {
		fmt.Println("no interface", *inside)
		os.Exit(1)
	}
	outIdx, ok := idx[*outside]
	if !ok {
		fmt.Println("no interface", *outside)
		os.Exit(1)
	}

	p := vppapi.Nat44Params{
		InsideVRF: 0, OutsideVRF: 0,
		Users: uint32(*users), UserSessions: uint32(*userSessions), Sessions: uint32(*sessions),
	}

	for i := 1; i <= *repeat; i++ {
		fmt.Printf("--- round %d\n", i)
		if err := c.Nat44Enable(ctx, p, true); err != nil {
			// -170 = feature already enabled (a previous -keep round).
			if !contains(err.Error(), "-170") {
				fmt.Println("enable FAIL:", err)
				os.Exit(2)
			}
			fmt.Println("enable: already enabled, continuing")
		} else {
			fmt.Println("enable retval=0")
		}
		for _, r := range []struct {
			name  string
			idx   uint32
			flags byte
		}{{*inside, inIdx, vppapi.Nat44FlagIfInside}, {*outside, outIdx, vppapi.Nat44FlagIfOutside}} {
			if err := c.Nat44InterfaceFeature(ctx, r.idx, r.flags, true); err != nil {
				fmt.Printf("interface %s add FAIL: %v\n", r.name, err)
				os.Exit(2)
			}
			fmt.Printf("interface %s add retval=0 (flags %d)\n", r.name, r.flags)
		}
		rows, err := c.Nat44Interfaces(ctx)
		if err != nil {
			fmt.Println("interface dump FAIL:", err)
			os.Exit(2)
		}
		fmt.Printf("interface dump: %d rows\n", len(rows))
		for _, r := range rows {
			fmt.Printf("  sw_if_index=%d inside=%v outside=%v\n", r.SWIfIndex, r.Inside, r.Outside)
		}
		addr := netip.MustParseAddr("10.9.9.10")
		if err := c.Nat44AddressRange(ctx, addr, addr, 0, true); err != nil {
			fmt.Println("address range add FAIL:", err)
			os.Exit(2)
		}
		fmt.Println("address range add retval=0")
		paddrs, err := c.Nat44PoolAddresses(ctx)
		if err != nil {
			fmt.Println("address dump FAIL:", err)
			os.Exit(2)
		}
		fmt.Printf("address dump: %d rows\n", len(paddrs))
		for _, r := range paddrs {
			fmt.Printf("  ip=%s vrf=%d\n", r.Addr, r.VRF)
		}
		// tear down
		if *keep {
			// For traffic tests the pool address must be one VPP can route
			// replies to: drop the fake range and use the outside interface's
			// own address instead.
			_ = c.Nat44AddressRange(ctx, addr, addr, 0, false)
			if *pool != "" {
				pa := netip.MustParseAddr(*pool)
				if err := c.Nat44AddressRange(ctx, pa, pa, 0, true); err != nil {
					fmt.Println("pool add FAIL:", err)
					os.Exit(2)
				}
				fmt.Printf("keep: NAT enabled, pool = dedicated %s\n", *pool)
			} else {
				if err := c.Nat44InterfaceAddr(ctx, outIdx, true); err != nil {
					fmt.Println("interface addr FAIL:", err)
					os.Exit(2)
				}
				fmt.Printf("keep: NAT enabled, pool = %s address\n", *outside)
			}
			break
		}
		if err := c.Nat44AddressRange(ctx, addr, addr, 0, false); err != nil {
			fmt.Println("address range del FAIL:", err)
			os.Exit(2)
		}
		for _, r := range []struct {
			name  string
			idx   uint32
			flags byte
		}{{*outside, outIdx, vppapi.Nat44FlagIfOutside}, {*inside, inIdx, vppapi.Nat44FlagIfInside}} {
			if err := c.Nat44InterfaceFeature(ctx, r.idx, r.flags, false); err != nil {
				fmt.Printf("interface %s del FAIL: %v\n", r.name, err)
				os.Exit(2)
			}
		}
		if err := c.Nat44Enable(ctx, p, false); err != nil {
			fmt.Println("disable FAIL:", err)
			os.Exit(2)
		}
		fmt.Println("disable retval=0")
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Println("ALL ROUNDS OK")
}
