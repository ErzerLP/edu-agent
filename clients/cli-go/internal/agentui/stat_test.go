package agentui

import (
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
)

func TestStatToolDisplayAndMetadataDetail(t *testing.T) {
	if toolDisplayName("stat") != "检查入口元数据" {
		t.Fatal("stat UI name missing")
	}
	output := renderToolGroup([]agentloop.Activity{{Event: agentloop.Event{ID: "stat-file", Tool: "stat", Summary: "已检查 data", Status: agentloop.EventSucceeded}, File: &agentloop.FileActivityDetail{Path: "data", EntryKind: "file"}}}, 100, true)
	for _, required := range []string{"检查入口元数据", "data", "对象类型：file"} {
		if !strings.Contains(output, required) {
			t.Fatalf("missing %s: %s", required, output)
		}
	}
	for _, unexpected := range []string{"未知工具", "返回字节", "文件修改授权", "归档目标"} {
		if strings.Contains(output, unexpected) {
			t.Fatalf("misleading metadata UI: %s", output)
		}
	}
}
