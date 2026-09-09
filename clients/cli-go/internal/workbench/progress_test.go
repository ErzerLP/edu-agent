package workbench

import "testing"

func TestProgressContinuationKeepsExplicitSpaceAndSession(t *testing.T) {
	m := New(t.Context(), nil, "old-space")
	m.sessions["old-space"] = "old-session"
	cmd := m.choose("select:resume:new-space/new-session")
	if cmd == nil || m.space != "new-space" || m.sessions["new-space"] != "new-session" || m.sessions["old-space"] != "old-session" || m.page != "session/new-session" {
		t.Fatalf("续学跳到了错误上下文：%+v", m)
	}
}
