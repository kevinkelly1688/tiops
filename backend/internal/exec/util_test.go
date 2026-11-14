// @author: kevinkelly
// @license: Apache License 2.0
// @file: util_test.go
// @time: 2026/10/28

package exec

import (
	"os"
	"runtime"
	"testing"
	"time"
)

type timeDuration = time.Duration

func assertPerm0600(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return // Windows 没有 POSIX 权限位
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("%s 权限 %v，期望 0600", path, fi.Mode().Perm())
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(es)
}
