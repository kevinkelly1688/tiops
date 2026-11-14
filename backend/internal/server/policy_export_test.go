// @author: kevinkelly
// @license: Apache License 2.0
// @file: policy_export_test.go
// @time: 2026/10/28

package server

import "time"

// AgeProposalsForTest 让 server_test 包把所有 pending 提案的发起 / 过期时间整体前移 d（模拟时间流逝，不影响会话）。
func (s *Server) AgeProposalsForTest(d time.Duration) {
	ps := s.ops.props
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for _, p := range ps.items {
		if p.State == PropPending {
			p.At, p.Expires = p.At.Add(-d), p.Expires.Add(-d)
		}
	}
	_ = ps.saveLocked()
}
