package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/cilium/ebpf"
)

func ntohs(n uint16) uint16 {
	return (n<<8)&0xff00 | n>>8
}

func ntohl(n uint32) uint32 {
	return (n<<24)&0xff000000 | (n >> 24) | (n<<8)&0x00ff0000 | (n>>8)&0x0000ff00
}

func convert_port(s string) uint16 {
	t, _ := strconv.ParseInt(s, 0, 16)
	return ntohs(uint16(t))
}

func convert_flowid(s string) uint32 {
	t, _ := strconv.ParseInt(s, 0, 32)
	return ntohl(uint32(t))
}

func main() {
	if len(os.Args) < 3 {
		fmt.Println("Wrong command.")
		return
	}
	switch os.Args[1] {
	case "add":
		if len(os.Args) < 4 {
			fmt.Println("Wrong command.")
			return
		}
		fmt.Println(os.Args)
		key := convert_port(os.Args[2])
		var value uint32 = convert_flowid(os.Args[3])
		receive_map, _ := ebpf.LoadPinnedMap("/sys/fs/bpf/xdp/globals/receive_map", nil)
		defer receive_map.Close()
		receive_map.Put(key, &value)
	case "del":
		fmt.Println(os.Args)
		key := convert_port(os.Args[2])
		receive_map, _ := ebpf.LoadPinnedMap("/sys/fs/bpf/xdp/globals/receive_map", nil)
		defer receive_map.Close()
		var result []byte
		receive_map.LookupAndDelete(key, &result)
		fmt.Println(result)
	default:
		fmt.Println("Wrong command.")
	}
}
