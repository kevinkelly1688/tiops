// @author: kevinkelly
// @license: Apache License 2.0
// @file: hub_export_test.go
// @time: 2026/10/28

package server

import "testing"

// IsGlobalAPIForTest 让 server_test 包校验端点分类表。
func IsGlobalAPIForTest(p string) bool { return isGlobalAPI(p) }

func TestNotifyLinkCarriesCluster(t *testing.T) {
	o := &ops{cfg: OpsConfig{PublicURL: "https://ops.example.com/"}}
	primary := &Server{hub: &Hub{primaryID: "prod"}, clusterID: "prod"}
	other := &Server{hub: &Hub{primaryID: "prod"}, clusterID: "stg"}
	single := &Server{}
	if got := primary.notifyLink(o, "#schedule"); got != "https://ops.example.com/#schedule" {
		t.Errorf("主集群链接保持不变: %s", got)
	}
	if got := single.notifyLink(o, "#schedule"); got != "https://ops.example.com/#schedule" {
		t.Errorf("单集群链接保持不变: %s", got)
	}
	if got := other.notifyLink(o, "#schedule"); got != "https://ops.example.com/?cluster=stg#schedule" {
		t.Errorf("非主集群链接应带 cluster: %s", got)
	}
	if got := other.notifyLink(&ops{}, "#x"); got != "" {
		t.Errorf("没有 PublicURL 不加链接: %q", got)
	}
}
