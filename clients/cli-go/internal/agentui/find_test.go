package agentui

import "testing"

func TestFindToolDisplayName(t *testing.T) {
	if name := toolDisplayName("find"); name != "查找文件或目录" {
		t.Fatalf("find display name=%q", name)
	}
}
