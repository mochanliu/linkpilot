package agent

import (
	"context"
	"fmt"
	"sync"

	"sdwan/controller/internal/vppapi"
	"sdwan/controller/internal/wire"
)

// InventorySource 是"读一份接口清单"的只读接缝：*vppapi.Client 满足它，单测用
// 假实现，不必起一个 VPP。
//
// Close 也在这里，因为连接的生命周期归调用方：一次 api.sock 注册在 VPP 的
// `show api clients` 里是一行，反复 Dial 却不关会把那张表堆满。
type InventorySource interface {
	Inventory(ctx context.Context) ([]vppapi.SwInterface, error)
	Close() error
}

// InventoryReporter 按心跳节奏读一次设备侧接口清单。
//
// 两条规则，都来自"这是旁路信息"这个定位：
//
//  1. 读不到就报 nil，绝不拖垮心跳。VPP 重启的几十秒里 agent 仍然必须让控制器
//     看见它还活着；把一个可选字段的失败升级成心跳失败，会把"接口清单缺一行"
//     变成"设备离线"。
//  2. 连接复用、断了才重连。VPP 关连接后下一次 Report 会重新注册 —— 而不是每条
//     心跳都注册一次（那会在 VPP 里留下一串死条目，也让故障更难读）。
//
// nil 与空列表的区别是契约的一部分（见 wire.HeartbeatRequest.Ifaces）：没启用
// -vpp-api、或这次读失败 → nil；数据面确实一张口都没有 → 空列表。
type InventoryReporter struct {
	socket  string
	appName string
	logf    func(string, ...any)

	// dial 可替换（单测）；默认就是 vppapi.Dial。
	dial func(ctx context.Context, socket, appName string) (InventorySource, error)

	mu   sync.Mutex
	src  InventorySource
	last string // 上一次已经打过的错误，同一句不重复刷日志
}

// NewInventoryReporter 造一个上报器。socket 为空 = 这个功能没打开（Report 恒返回
// nil），这正好对上"agent 不带 -vpp-api 时不碰数据面"。
func NewInventoryReporter(socket, appName string, logf func(string, ...any)) *InventoryReporter {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &InventoryReporter{
		socket:  socket,
		appName: appName,
		logf:    logf,
		dial: func(ctx context.Context, socket, appName string) (InventorySource, error) {
			return vppapi.Dial(ctx, socket, appName)
		},
	}
}

// Report 读一次清单。它永远不返回错误：失败已经是一种"上报内容"（nil），
// 由控制器把"这台设备的接口清单没上来"显示成未上报，而不是当成零接口。
func (r *InventoryReporter) Report(ctx context.Context) *[]wire.IfaceInfo {
	if r == nil || r.socket == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.src == nil {
		src, err := r.dial(ctx, r.socket, r.appName)
		if err != nil {
			r.warnf("接口清单：连不上 VPP api.sock，这次心跳不带清单: %v", err)
			return nil
		}
		r.src = src
	}
	ifs, err := r.src.Inventory(ctx)
	if err != nil {
		// 超时/连接断开都在这里：丢掉连接，下一次心跳重新注册。
		r.src.Close()
		r.src = nil
		r.warnf("接口清单：读取失败，这次心跳不带清单: %v", err)
		return nil
	}
	r.last = ""
	return ifaceInfos(ifs)
}

// Close 释放那条连接（进程退出时调用）。
func (r *InventoryReporter) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.src == nil {
		return nil
	}
	err := r.src.Close()
	r.src = nil
	return err
}

func (r *InventoryReporter) warnf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if msg == r.last {
		return
	}
	r.last = msg
	r.logf("%s", msg)
}

// ifaceInfos 把数据面的接口表翻成心跳里的那份清单。
//
// 报**全部**接口，不做"只留物理口"的筛选：VPP 26.06 没有 hw_interface_dump，
// 而 sw_interface_details.type 在 local0/host-eth1/wg0 上实测都是 0，按它筛等于
// 自己发明一个分类。让控制器拿反射器看到的真实外层源去比对，才是"测出来的"。
func ifaceInfos(ifs []vppapi.SwInterface) *[]wire.IfaceInfo {
	out := make([]wire.IfaceInfo, 0, len(ifs))
	for _, i := range ifs {
		info := wire.IfaceInfo{
			Name:      i.Name,
			SWIfIndex: i.SWIfIndex,
			MTU:       i.L3MTU,
			AdminUp:   i.AdminUp,
			LinkUp:    i.LinkUp,
		}
		for _, p := range i.IPv4 {
			info.Addrs = append(info.Addrs, fmt.Sprintf("%s/%d", p.IP, p.Len))
		}
		out = append(out, info)
	}
	return &out
}
