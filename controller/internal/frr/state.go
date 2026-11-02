package frr

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// stateMode 是新状态文件的格式标记。旧文件（ZAPI 的 TABLE 路由）没有这个
// 字段，读到就按"已退役"处理：删掉文件、不按里面的内容去撤 —— 那些条目
// 属于 zebra 的 1250 表，用内核 netlink 删它们既不对也删不到。
const stateMode = "krt"

// ErrLegacyState 表示状态文件来自已退役的 ZAPI/1250 导出。
var ErrLegacyState = errors.New("frr: 旧版状态文件（没有 mode，属于已退役的 ZAPI/1250 导出）")

// stateDoc 是"我往内核表里写了什么"的落盘格式。
//
// 这份文件存在的唯一原因是内核不会在进程退出时回收路由：进程正常退出时我们
// 逐条撤掉并删文件；被 SIGKILL 时文件还在，下次启动先按它把上一轮的残留删
// 干净，再开始收敛。没有它，崩溃一次就会留下一批再也没人认领的前缀，而且
// 会一直被 redistribute kernel 出去。
type stateDoc struct {
	Mode   string  `json:"mode"`
	Routes []Route `json:"routes"`
}

// LoadState 读状态文件；文件不存在表示"上一轮干净退出"，不算错误。
// 旧格式返回 ErrLegacyState（调用方删文件即可，不要读 Routes）。
func LoadState(path string) ([]Route, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc stateDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("解析状态文件 %s: %w", path, err)
	}
	switch doc.Mode {
	case "":
		return nil, ErrLegacyState
	case stateMode:
	default:
		return nil, fmt.Errorf("状态文件 %s 的 mode=%q 不是 %q", path, doc.Mode, stateMode)
	}
	for _, r := range doc.Routes {
		if !r.Prefix.IsValid() || !r.Prefix.Addr().Is4() || r.Prefix != r.Prefix.Masked() {
			return nil, fmt.Errorf("状态文件 %s 里的条目无效: %q", path, r.Prefix)
		}
		if r.Table == 0 || r.Table > 255 {
			return nil, fmt.Errorf("状态文件 %s 里的 %s 表号 %d 不在 1..255", path, r.Prefix, r.Table)
		}
		if r.OIF <= 0 {
			return nil, fmt.Errorf("状态文件 %s 里的 %s 出口 ifindex %d 非法", path, r.Prefix, r.OIF)
		}
	}
	return doc.Routes, nil
}

// SaveState 原子替换状态文件：先写同目录临时文件再 rename，崩溃时读到的
// 要么是旧的完整内容，要么是新的完整内容，不会是半份 JSON。
func SaveState(path string, routes []Route) error {
	if routes == nil {
		routes = []Route{}
	}
	b, err := json.MarshalIndent(stateDoc{Mode: stateMode, Routes: routes}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func RemoveState(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
