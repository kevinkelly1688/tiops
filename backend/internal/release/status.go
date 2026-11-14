// @author: kevinkelly
// @license: Apache License 2.0
// @file: status.go
// @time: 2026/10/28

package release

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"time"

	"tidbops/internal/dataver"
)

// Status 是 admin status 的结果（--json 直接输出该结构）。
type Status struct {
	Installed     bool           `json:"installed"`
	Current       string         `json:"current,omitempty"`
	BuildTime     *time.Time     `json:"buildTime,omitempty"`
	Env           string         `json:"env,omitempty"`
	Cluster       string         `json:"cluster,omitempty"`
	Listen        string         `json:"listen,omitempty"`
	Releases      []string       `json:"releases"`
	Service       string         `json:"service"`    // active | inactive | failed | unknown …
	DataSchema    *int           `json:"dataSchema"` // 读不到时为 null
	DataVersion   string         `json:"dataVersion,omitempty"`
	DataNote      string         `json:"dataNote,omitempty"`
	BinarySchema  int            `json:"binarySchema"`
	History       []HistoryEntry `json:"history"` // 最近 5 条，新的在后
	Backups       []backupInfo   `json:"backups"` // 新的在前
	BackupsNote   string         `json:"backupsNote,omitempty"`
	HistoryNote   string         `json:"historyNote,omitempty"`
	GeneratedTime time.Time      `json:"generatedAt"`
}

// Status 收集状态（不需要 root；读不到的部分给出说明而不是失败）。
func (e *Engine) Status(ctx context.Context) (Status, error) {
	e.defaults()
	s := Status{Releases: []string{}, History: []HistoryEntry{}, Backups: []backupInfo{}, GeneratedTime: e.Sys.Now()}
	cur, err := e.currentVersion()
	switch {
	case err == nil:
		s.Installed, s.Current = true, cur
		if m, merr := e.releaseManifest(cur); merr == nil {
			bt := m.BuildTime
			s.BuildTime, s.BinarySchema = &bt, m.SchemaVersion
		}
		if p, perr := e.installedParams(); perr == nil {
			s.Env, s.Cluster, s.Listen = p.Env, p.Cluster, p.Listen
		}
	case errors.Is(err, fs.ErrNotExist):
	default:
		return s, err
	}
	if rels, err := e.listReleases(); err == nil && rels != nil {
		s.Releases = rels
	}
	if _, st := e.serviceActive(ctx); st != "" {
		s.Service = st
	} else {
		s.Service = "unknown"
	}

	switch in, err := dataver.Read(e.L.Data()); {
	case err == nil:
		sc := in.Schema
		s.DataSchema, s.DataVersion = &sc, in.Version
	case errors.Is(err, fs.ErrNotExist):
		if _, derr := os.Stat(e.L.Data()); derr == nil {
			zero := 0
			s.DataSchema = &zero
			s.DataNote = "没有 VERSION 文件（旧版本创建的数据，视为 schema 0）"
		} else if errors.Is(derr, fs.ErrPermission) {
			s.DataNote = "无权读取数据目录（需 root 或 tidbops 账号）"
		} else {
			s.DataNote = "数据目录不存在"
		}
	case errors.Is(err, fs.ErrPermission):
		s.DataNote = "无权读取数据目录（需 root 或 tidbops 账号）"
	default:
		s.DataNote = err.Error()
	}

	if hs, err := readHistory(e.L.History()); err == nil {
		if len(hs) > 5 {
			hs = hs[len(hs)-5:]
		}
		if hs != nil {
			s.History = hs
		}
	} else if errors.Is(err, fs.ErrPermission) {
		s.HistoryNote = "无权读取升级历史（需 root 或 tidbops 账号）"
	}
	if bs, err := e.listBackups(); err == nil {
		if bs != nil {
			s.Backups = bs
		}
	} else if errors.Is(err, fs.ErrPermission) {
		s.BackupsNote = "无权读取备份目录（需 root）"
	}
	return s, nil
}

// PrintStatus 以人读格式输出。
func (e *Engine) PrintStatus(s Status, asJSON bool) error {
	if asJSON {
		b, err := json.MarshalIndent(s, "", "  ")
		if err != nil {
			return err
		}
		e.printf("%s\n", b)
		return nil
	}
	if !s.Installed {
		e.printf("未安装（%s 不存在）。首次部署：sudo ./bin/tidbops-server admin install --cluster <集群名> --env prod\n", e.L.Current())
	} else {
		e.printf("当前版本：%s", s.Current)
		if s.BuildTime != nil {
			e.printf("（构建于 %s）", s.BuildTime.Format("2006-01-02 15:04:05"))
		}
		e.printf("\n环境 / 集群 / 监听：%s / %s / %s\n", orDash(s.Env), orDash(s.Cluster), orDash(s.Listen))
	}
	e.printf("已安装的 release：%v\n", s.Releases)
	e.printf("服务状态：%s\n", s.Service)
	if s.DataSchema != nil {
		e.printf("数据 schema：%d（程序 schema：%d）", *s.DataSchema, s.BinarySchema)
		if s.DataVersion != "" {
			e.printf("，数据最近由 %s 写入", s.DataVersion)
		}
		e.printf("\n")
	} else {
		e.printf("数据 schema：未知（程序 schema：%d）\n", s.BinarySchema)
	}
	if s.DataNote != "" {
		e.printf("  注：%s\n", s.DataNote)
	}
	e.printf("最近升级历史：\n")
	if s.HistoryNote != "" {
		e.printf("  %s\n", s.HistoryNote)
	} else if len(s.History) == 0 {
		e.printf("  （无）\n")
	}
	for _, h := range s.History {
		res := "成功"
		switch {
		case h.RolledBack:
			res = "失败，已自动回滚"
		case !h.OK:
			res = "失败"
		}
		e.printf("  %s  %-8s %s → %s  %s", h.Time.Local().Format("2006-01-02 15:04:05"), h.Action, orDash(h.From), h.To, res)
		if h.Operator != "" {
			e.printf("  操作人 %s", h.Operator)
		}
		if h.Error != "" {
			e.printf("  错误：%s", h.Error)
		}
		e.printf("\n")
	}
	e.printf("可用备份：\n")
	if s.BackupsNote != "" {
		e.printf("  %s\n", s.BackupsNote)
	} else if len(s.Backups) == 0 {
		e.printf("  （无）\n")
	}
	for _, b := range s.Backups {
		e.printf("  %s  %s\n", b.Path, humanBytes(b.Size))
	}
	return nil
}
