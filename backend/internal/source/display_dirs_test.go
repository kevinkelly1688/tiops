// @author: kevinkelly
// @license: Apache License 2.0
// @file: display_dirs_test.go
// @time: 2026/10/28

package source_test

import (
	"testing"

	"tidbops/internal/source"
)

const displayHead = "Cluster name:       c1\nCluster version:    v8.5.2\nDeploy user:        tidb\n"

func TestParseDisplayDirs(t *testing.T) {
	out := displayHead + `ID               Role  Host      Ports        OS/Arch       Status   Data Dir               Deploy Dir
--               ----  ----      -----        -------       ------   --------               ----------
10.0.1.11:4000   tidb  10.0.1.11 4000/10080   linux/x86_64  Up       -                      /tidb-deploy/tidb-4000
10.0.2.11:20160  tikv  10.0.2.11 20160/20180  linux/x86_64  Up       /tidb-data/tikv-20160  /tidb-deploy/tikv-20160
10.0.3.11:3930   tiflash 10.0.3.11 9000/8123/3930/20170/20292/8234 linux/x86_64 Up /data/tiflash-9000 /deploy/tiflash-9000
Total nodes: 3
`
	ci := source.ParseDisplay(out)
	if len(ci.Instances) != 3 {
		t.Fatalf("实例数: %+v", ci.Instances)
	}
	if in := ci.Instances[0]; in.DataDir != "" || in.DeployDir != "/tidb-deploy/tidb-4000" {
		t.Errorf("TiDB 没有数据目录（display 里是 -）: %+v", in)
	}
	if in := ci.Instances[1]; in.DataDir != "/tidb-data/tikv-20160" || in.DeployDir != "/tidb-deploy/tikv-20160" || in.Status != "Up" || in.Ports != "20160/20180" {
		t.Errorf("TiKV: %+v", in)
	}
	if in := ci.Instances[2]; in.DataDir != "/data/tiflash-9000" || in.DeployDir != "/deploy/tiflash-9000" {
		t.Errorf("TiFlash（Ports 列很长）: %+v", in)
	}
}

// 列数不足（没有目录列 / 只有一列 / 被截断）：目录留空，其余字段照旧，不 panic。
func TestParseDisplayDirsMissingColumns(t *testing.T) {
	out := displayHead + `ID               Role  Host      Ports        OS/Arch       Status
--               ----  ----      -----        -------       ------
10.0.2.11:20160  tikv  10.0.2.11 20160/20180  linux/x86_64  Up
10.0.2.12:20160  tikv  10.0.2.12 20160/20180  linux/x86_64  Up       /tidb-data/tikv-20160
10.0.2.13:20160  tikv  10.0.2.13 20160/20180  linux/x86_64  Up|L     /tidb-data/tikv-20160  /tidb-deploy/tikv-20160
10.0.2.14:20160  tikv  10.0.2.14 20160
`
	ci := source.ParseDisplay(out)
	// 最后一行不足 6 列，被忽略（与既有行为一致）
	if len(ci.Instances) != 3 {
		t.Fatalf("实例数: %+v", ci.Instances)
	}
	for i, want := range [][2]string{{"", ""}, {"", ""}, {"/tidb-data/tikv-20160", "/tidb-deploy/tikv-20160"}} {
		if in := ci.Instances[i]; in.DataDir != want[0] || in.DeployDir != want[1] {
			t.Errorf("第 %d 行目录 = %q / %q，want %v", i, in.DataDir, in.DeployDir, want)
		}
	}
	if ci.Instances[2].Status != "Up|L" {
		t.Errorf("状态列不受目录列影响: %+v", ci.Instances[2])
	}
}
