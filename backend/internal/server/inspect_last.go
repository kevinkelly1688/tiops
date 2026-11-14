// @author: kevinkelly
// @license: Apache License 2.0
// @file: inspect_last.go
// @time: 2026/10/28

package server

import (
	"encoding/json"
	"os"
	"path/filepath"

	"tidbops/internal/model"
)

// 最近一次巡检报告落盘在 <数据目录>/inspect-last.json（多集群时是各集群自己的目录），
// 平台重启 / 升级后「集群巡检」页面仍能看到并导出上一次的报告。只保留最近一次，不是历史列表。

const inspectLastFile = "inspect-last.json"

// loadLastInspect 读取上次保存的巡检报告；数据目录为空、文件不存在或损坏都返回 nil（页面显示「还没有巡检结果」）。
func loadLastInspect(dir string) *model.Report {
	if dir == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(dir, inspectLastFile))
	if err != nil {
		return nil
	}
	var rep model.Report
	if json.Unmarshal(b, &rep) != nil || rep.Time.IsZero() {
		return nil
	}
	return &rep
}

// saveLastInspect 原子写入（临时文件 + 改名，0600）。失败只写日志，不影响巡检结果的返回。
func (s *Server) saveLastInspect(o *ops, rep *model.Report) {
	if o.cfg.Dir == "" || rep == nil {
		return
	}
	b, err := json.Marshal(rep)
	if err == nil {
		p := filepath.Join(o.cfg.Dir, inspectLastFile)
		if err = os.WriteFile(p+".tmp", b, 0o600); err == nil {
			err = os.Rename(p+".tmp", p)
		}
	}
	if err != nil {
		s.cfg.Logf("保存最近一次巡检报告失败（不影响本次结果）：%v", err)
	}
}
