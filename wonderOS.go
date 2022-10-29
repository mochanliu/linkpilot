package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/cilium/ebpf"
)

// SR data structure
type srouhdr struct {
	MagicNumber uint8
	FlowIDLen   uint8
	ProtoID     uint8
	SRLen       uint8
	FlowID      uint32
}

type srousrh struct {
	SegType   uint8
	LastEntry uint8
	SegLeft   uint8
	SRHLen    uint8
}

type srlistv4 struct {
	ip   uint32
	port uint16
	arg  uint16
}

type sr struct {
	srou   srouhdr
	OriSrc srlistv4
	srh    srousrh
	seg0   srlistv4
	seg1   srlistv4
	seg2   srlistv4
	seg3   srlistv4
	seg4   srlistv4
}

func ntohs(n uint16) uint16 {
	return (n<<8)&0xff00 | n>>8
}

func ntohl(n uint32) uint32 {
	return (n<<24)&0xff000000 | (n >> 24) | (n<<8)&0x00ff0000 | (n>>8)&0x0000ff00
}

func convertIP(s string) uint32 {
	var ipadd uint32 = 0
	t := strings.Split(s, ".")
	var a, b, c, d int
	a, _ = strconv.Atoi(t[0])
	b, _ = strconv.Atoi(t[1])
	c, _ = strconv.Atoi(t[2])
	d, _ = strconv.Atoi(t[3])
	ipadd = ipadd + uint32(a) + uint32(b)*256 + uint32(c)*256*256 + uint32(d)*256*256*256
	return ipadd
}

func convert_port(s string) uint16 {
	t, _ := strconv.ParseInt(s, 0, 16)
	return ntohs(uint16(t))
}

func convert_flowid(s string) uint32 {
	t, _ := strconv.ParseInt(s, 0, 32)
	return ntohl(uint32(t))
}

// configuraion module
type leaf map[string]string

type list struct {
	leaves leaf
}

type container struct {
	containers map[string]container
	lists      map[string][]list
}

type module struct {
	name string
	c    map[string]container
}

func parse_module(c []byte) (mod module) {
	j := 0
	i := 0
	var k int
	start := false
	for ; i < len(c); i++ {
		switch string(c[i]) {
		case string(10):
			if !start {
				j += 1
			}
		case string(32):
			if !start {
				j += 1
			}
		case "{":
			start = true
			temp := strings.Split(string(c[j:i]), " ")
			if temp[0] == "module" {
				fmt.Println(temp)
				mod.name = temp[1]
				start = false
				j = i + 1
			} else if temp[0] == "container" {
				fmt.Println(temp)
				if mod.c == nil {
					mod.c = make(map[string]container)
				}
				mod.c[temp[1]], k = parse_container(c[i+1:])
				i += k
				j = i + 1
				start = false
			}
		case "}":
			start = true
			return mod
		default:
			start = true
		}
	}
	return mod
}

func parse_container(c []byte) (con container, i int) {
	j := 0
	i = 0
	var k int
	start := false
	for ; i < len(c); i++ {
		switch string(c[i]) {
		case string(10):
			if !start {
				j += 1
			}
		case string(32):
			if !start {
				j += 1
			}
		case "{":
			start = true
			temp := strings.Split(string(c[j:i]), " ")
			switch temp[0] {
			case "container":
				fmt.Println(temp)
				if con.containers == nil {
					con.containers = make(map[string]container)
				}
				con.containers[temp[1]], k = parse_container(c[i+1:])
				i += k
				j = i + 1
				start = false
			case "list":
				fmt.Println(temp)
				var l list
				l, k = parse_list(c[i+1:])
				if con.lists == nil {
					con.lists = make(map[string][]list)
				}
				con.lists[temp[1]] = append(con.lists[temp[1]], l)
				i += k
				j = i + 1
				start = false
			}
		case "}":
			start = true
			i++
			return con, i
		default:
			start = true
		}
	}
	return con, i
}

func parse_list(c []byte) (l list, i int) {
	j := 0
	i = 0
	//var k int
	start := false
	for ; i < len(c); i++ {
		switch string(c[i]) {
		case string(10):
			if !start {
				j += 1
			}
		case string(32):
			if !start {
				j += 1
			}
		case ";":
			start = true
			temp := strings.Split(string(c[j:i]), " ")
			if temp[0] == "leaf" {
				if l.leaves == nil {
					l.leaves = make(leaf)
				}
				l.leaves[temp[1]] = temp[2]
			}
			j = i + 1
			start = false
		case "}":
			start = true
			i++
			return l, i
		default:
			start = true
		}
	}
	return l, i
}

func config_tunnel_list(lists []list) {
	var srconfig sr
	var key uint16
	var t int
	for _, v := range lists {
		switch v.leaves["action"] {
		case "encapsulation":
			fmt.Println("Found a SR tunnel!Configuring...")
			t, _ = strconv.Atoi(v.leaves["MagicNumber"])
			srconfig.srou.MagicNumber = uint8(t)
			t, _ = strconv.Atoi(v.leaves["FlowIDLen"])
			srconfig.srou.FlowIDLen = uint8(t)
			t, _ = strconv.Atoi(v.leaves["ProtoID"])
			srconfig.srou.ProtoID = uint8(t)
			t, _ = strconv.Atoi(v.leaves["SRLen"])
			srconfig.srou.SRLen = uint8(t)
			srconfig.srou.FlowID = convert_flowid(v.leaves["FlowID"])
			t, _ = strconv.Atoi(v.leaves["SegType"])
			srconfig.srh.SegType = uint8(t)
			t, _ = strconv.Atoi(v.leaves["LastEntry"])
			srconfig.srh.LastEntry = uint8(t)
			t, _ = strconv.Atoi(v.leaves["SegLeft"])
			srconfig.srh.SegLeft = uint8(t)
			t, _ = strconv.Atoi(v.leaves["SRHLen"])
			srconfig.srh.SRHLen = uint8(t)
			srconfig.OriSrc.ip = convertIP(v.leaves["OriSrc-ip"])
			srconfig.OriSrc.port = convert_port(v.leaves["OriSrc-port"])
			srconfig.OriSrc.arg = convert_port(v.leaves["OriSrc-arg"])
			srconfig.seg0.ip = convertIP(v.leaves["seg0-ip"])
			srconfig.seg0.port = convert_port(v.leaves["seg0-port"])
			srconfig.seg0.arg = convert_port(v.leaves["seg0-arg"])
			srconfig.seg1.ip = convertIP(v.leaves["seg1-ip"])
			srconfig.seg1.port = convert_port(v.leaves["seg1-port"])
			srconfig.seg1.arg = convert_port(v.leaves["seg1-arg"])
			srconfig.seg2.ip = convertIP(v.leaves["seg2-ip"])
			srconfig.seg2.port = convert_port(v.leaves["seg2-port"])
			srconfig.seg2.arg = convert_port(v.leaves["seg2-arg"])
			srconfig.seg3.ip = convertIP(v.leaves["seg3-ip"])
			srconfig.seg3.port = convert_port(v.leaves["seg3-port"])
			srconfig.seg3.arg = convert_port(v.leaves["seg3-arg"])
			srconfig.seg4.ip = convertIP(v.leaves["seg4-ip"])
			srconfig.seg4.port = convert_port(v.leaves["seg4-port"])
			srconfig.seg4.arg = convert_port(v.leaves["seg4-arg"])
			key = convert_port(v.leaves["source-port"])
			send_map, _ := ebpf.LoadPinnedMap("/sys/fs/bpf/tc/globals/send_map", nil)
			defer send_map.Close()
			send_map.Put(key, &srconfig)
			var value uint32 = convert_flowid(v.leaves["FlowID"])
			receive_map, _ := ebpf.LoadPinnedMap("/sys/fs/bpf/xdp/globals/receive_map", nil)
			defer receive_map.Close()
			receive_map.Put(key, &value)
			cmd := "ip link add tunnel" + v.leaves["source-port"] + " type wireguard"
			_ = exec.Command("bash", "-c", cmd).Run()
			cmd = "ip link set tunnel" + v.leaves["source-port"] + " up"
			_ = exec.Command("bash", "-c", cmd).Run()
			cmd = "ip link set tunnel" + v.leaves["source-port"] + " mtu 1364"
			_ = exec.Command("bash", "-c", cmd).Run()
			cmd = "wg set tunnel" + v.leaves["source-port"] + " private-key " + v.leaves["private"] + " listen-port " + v.leaves["source-port"]
			_ = exec.Command("bash", "-c", cmd).Run()
			cmd = "wg set tunnel" + v.leaves["source-port"] + " peer " + v.leaves["public"] + " allowed-ips 0.0.0.0/0"
			if v.leaves["destination-ip"] != "0.0.0.0" {
				cmd = cmd + " endpoint " + v.leaves["destination-ip"] + ":" + v.leaves["destination-port"]
			}
			_ = exec.Command("bash", "-c", cmd).Run()
		case "xmit":
			fmt.Println("Found transmit tunnel!Configuring...")
			key = convert_port(v.leaves["source-port"])
			var value uint32 = convert_flowid(v.leaves["FlowID"])
			receive_map, _ := ebpf.LoadPinnedMap("/sys/fs/bpf/xdp/globals/receive_map", nil)
			defer receive_map.Close()
			receive_map.Put(key, &value)
		case "none":
			fmt.Println("Found general VPN tunnel!Configuring...")
			cmd := "ip link add tunnel" + v.leaves["source-port"] + " type wireguard"
			_ = exec.Command("bash", "-c", cmd).Run()
			cmd = "ip link set tunnel" + v.leaves["source-port"] + " up"
			_ = exec.Command("bash", "-c", cmd).Run()
			cmd = "ip link set tunnel" + v.leaves["source-port"] + " mtu 1364"
			_ = exec.Command("bash", "-c", cmd).Run()
			cmd = "wg set tunnel" + v.leaves["source-port"] + " private-key " + v.leaves["private"] + " listen-port " + v.leaves["source-port"]
			_ = exec.Command("bash", "-c", cmd).Run()
			cmd = "wg set tunnel" + v.leaves["source-port"] + " peer " + v.leaves["public"] + " allowed-ips 0.0.0.0/0"
			if v.leaves["destination-ip"] != "0.0.0.0" {
				cmd = cmd + " endpoint " + v.leaves["destination-ip"] + ":" + v.leaves["destination-port"]
			}
			_ = exec.Command("bash", "-c", cmd).Run()
		}
	}
}

func config_container(c container) {
	for k, v := range c.containers {
		switch k {
		case "underlay":
			fmt.Println("Found underlay configuration.Configuring...")
			config_container(v)
		}
	}
	for k, v := range c.lists {
		switch k {
		case "tunnel":
			fmt.Println("Found tunnel configuration.Configuring...")
			config_tunnel_list(v)
		}
	}
}

func load_config(config_module module) {
	fmt.Println("Finding modules...")
	for k, v := range config_module.c {
		switch k {
		case "SDWAN":
			fmt.Println("SDWAN module found!Configuring...")
			config_container(v)
		}
	}
	fmt.Println("Configuraion loaded!!!")
	fmt.Println("-----------------------------------------------")
}

func main() {
	f, _ := os.Open("SDWAN-router.yang")
	config, err := ioutil.ReadAll(f)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Reading SDWAN-router.yang...")
	var config_module module = parse_module(config)
	fmt.Println(config_module)
	fmt.Println("-------------------------------------------------")
	load_config(config_module)
}
