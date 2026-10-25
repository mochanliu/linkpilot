package frr

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// stateDoc 是"我往 zebra 写了什么"的落盘格式。
//
// 这份文件存在的唯一原因是 zebra 不在客户端断开时回收路由（实测）：进程
// 正常退出时我们逐条撤掉并删文件；被 SIGKILL 时文件还在，下次启动先按它
// 把上一轮的残留删干净，再开始收敛。没有它，崩溃一次就会留下一批再也没人
// 认领的前缀，而且会一直被 redistribute 出去。
type stateDoc struct {
	Routes []Route `json:"routes"`
}

// LoadState 读状态文件；文件不存在表示"上一轮干净退出"，不算错误。
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
	for _, r := range doc.Routes {
		if !r.Prefix.IsValid() || !r.Prefix.Addr().Is4() || r.Prefix != r.Prefix.Masked() || r.TableID == 0 {
			return nil, fmt.Errorf("状态文件 %s 里的条目无效: %q 表 %d", path, r.Prefix, r.TableID)
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
	b, err := json.MarshalIndent(stateDoc{Routes: routes}, "", "  ")
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
