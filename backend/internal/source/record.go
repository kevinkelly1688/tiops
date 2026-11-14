// @author: kevinkelly
// @license: Apache License 2.0
// @file: record.go
// @time: 2026/10/28

package source

import (
	"context"
	"fmt"
	"sort"
	"time"

	"tidbops/internal/fixtures"
)

// Recorder 把另一个 Source（通常是 Live）的响应脱敏后落盘，生成样例数据包。
type Recorder struct {
	Src  Source
	Dir  fixtures.Dir
	San  *fixtures.Sanitizer
	Man  *fixtures.Manifest
	Hook func(rec fixtures.ItemRecord) // 每采完一项回调，用于进度输出
}

// NewRecorder 创建 Recorder。
func NewRecorder(src Source, root, cluster, toolVersion string, opt fixtures.Options) *Recorder {
	return &Recorder{
		Src: src,
		Dir: fixtures.Dir{Root: root},
		San: fixtures.NewSanitizer(opt),
		Man: &fixtures.Manifest{
			Schema: fixtures.SchemaVersion, Tool: "tidbops-fixtures", ToolVersion: toolVersion,
			Cluster: cluster, CollectedAt: time.Now().UTC(), Options: opt,
		},
	}
}

// RecordAll 依次采集所有条目。单项失败不会中断：失败原因记入清单，
// 必需项失败由调用方根据 Manifest.Summary 决定退出码。
func (r *Recorder) RecordAll(ctx context.Context, items []Item) {
	for _, it := range items {
		rec := fixtures.ItemRecord{Name: it.Name, Kind: string(it.Kind), Purpose: it.Purpose, Request: it.Request(), Optional: it.Optional}
		data, err := r.Src.Fetch(ctx, it)
		if err != nil {
			rec.Error = err.Error()
		} else {
			clean := r.San.Apply(data, it.SQLCols)
			file, sum, werr := r.Dir.Write(it.Name, it.Ext, clean)
			if werr != nil {
				rec.Error = werr.Error()
			} else {
				rec.OK, rec.File, rec.SHA256, rec.Bytes = true, file, sum, len(clean)
			}
		}
		r.Man.Add(rec)
		if r.Hook != nil {
			r.Hook(rec)
		}
	}
}

// Finish 写入清单（含只读声明）。
func (r *Recorder) Finish(items []Item) error {
	r.Man.ReadOnly = Attest(items)
	if err := r.Dir.SaveManifest(r.Man); err != nil {
		return fmt.Errorf("写入清单失败: %w", err)
	}
	return nil
}

// Attest 根据采集目录生成只读声明：列出本次会用到的全部操作种类。
func Attest(items []Item) fixtures.Attestation {
	a := fixtures.Attestation{HTTPMethods: []string{"GET"}}
	seenC, seenS, seenF := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, it := range items {
		switch it.Kind {
		case KindCmd:
			s := joinArgs(it.Argv)
			if !seenC[s] {
				seenC[s] = true
				a.Commands = append(a.Commands, s)
			}
		case KindSQL:
			s := oneLine(it.Query)
			if !seenS[s] {
				seenS[s] = true
				a.SQL = append(a.SQL, s)
			}
		case KindFile:
			if !seenF[it.File] {
				seenF[it.File] = true
				a.Files = append(a.Files, it.File)
			}
		}
	}
	sort.Strings(a.Commands)
	sort.Strings(a.SQL)
	sort.Strings(a.Files)
	return a
}

func joinArgs(a []string) string {
	s := ""
	for i, x := range a {
		if i > 0 {
			s += " "
		}
		s += x
	}
	return s
}
