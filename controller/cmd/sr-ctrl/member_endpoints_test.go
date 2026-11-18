package main

import (
	"reflect"
	"testing"
)

// 设备与 RR 拿到的地址表里，自己那条必须是本机此刻宣告的地址，而不是成员表里
// 入群时填下的记录。.100 迁移之后成员表里仍写着旧 Mac 的 192.168.1.4:8444——
// 照旧表发出去，设备与 RR 的候选表里就多一条必然失败的地址。
func TestMemberEndpointsUsesTheLocalAdvertiseForSelf(t *testing.T) {
	s := &server{
		nodeID: "me",
		adv:    "192.168.1.100:8444",
		state: &State{Cluster: &ClusterConfig{Members: map[string]*ClusterMember{
			"me":    {ID: "me", Endpoint: "192.168.1.4:8444", Status: memberActive, JoinedAt: 1},
			"other": {ID: "other", Endpoint: "192.168.1.5:8444", Status: memberActive, JoinedAt: 2},
			"gone":  {ID: "gone", Endpoint: "192.168.1.6:8444", Status: memberDecommissioned, JoinedAt: 3},
		}}},
	}
	if got, want := s.memberEndpointsLocked(), []string{"192.168.1.100:8444", "192.168.1.5:8444"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("地址表 = %v，应是 %v（自己用宣告地址，别的成员照旧，已下线的不出现）", got, want)
	}

	// 没有成员表（单机）时就是宣告地址本身。
	single := &server{state: &State{}, tlsAddrs: []string{"10.0.0.1:8444"}}
	if got, want := single.memberEndpointsLocked(), []string{"10.0.0.1:8444"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("单机地址表 = %v，应是 %v", got, want)
	}

	// 推不出宣告地址时不能把列表弄空：退回成员表里的值，宁可给一个可疑地址，
	// 也不要让设备与 RR 拿到一张空表（空表会让它们锁死在种子/旧地址上）。
	s.adv = ""
	if got, want := s.memberEndpointsLocked(), []string{"192.168.1.4:8444", "192.168.1.5:8444"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("无宣告地址时 = %v，应是成员表原值 %v", got, want)
	}
}
