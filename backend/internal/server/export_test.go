// @author: kevinkelly
// @license: Apache License 2.0
// @file: export_test.go
// @time: 2026/10/28

package server

import "tidbops/internal/model"

// WatchOnceForTest 让 server_test 包手动驱动一轮告警 / 诊断监视（配合 OpsConfig.NotifyWatchEvery<0）。
func (s *Server) WatchOnceForTest(snap model.Snapshot) { s.watchOnce(s.ops, snap) }

// NotifyDrainForTest 等待通知队列发完。
func (s *Server) NotifyDrainForTest() bool { return s.ops.notifier.Drain(5e9) }
