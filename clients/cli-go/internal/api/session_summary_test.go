package api

import "testing"

func TestSessionSummaryAcceptsProgressContext(t *testing.T) {
	const body = `{"session_id":"10000000-0000-4000-8000-000000000001","learning_space_id":"00000000-0000-4000-8000-000000000001","goal_id":"10000000-0000-4000-8000-000000000002","goal_revision_id":"10000000-0000-4000-8000-000000000003","name":"学习目标","goal_status":"active","state":"RouteActive","position":"路线","last_event_seq":3,"resumable":true,"node_revision_id":"10000000-0000-4000-8000-000000000004","route_revision_id":"10000000-0000-4000-8000-000000000005"}`
	var summary SessionSummary
	if err := decodeStrict([]byte(body), &summary); err != nil {
		t.Fatalf("不能读取服务端教学上下文：%v", err)
	}
}
