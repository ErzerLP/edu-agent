package api_test

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
)

func TestLearningContentContractAndUnknownFallback(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	a := learning.Activity{ID: id, Revision: 1, SessionID: id, Prompt: "选择原资料中的偶数", Type: learning.ActivityObjective}
	body := learningcontent.Adapt(learningcontent.Source{Activity: a})
	body.Blocks = append(body.Blocks, learningcontent.Block{ID: uuid.NewString(), Kind: "future_chart", Fallback: "未知展示保留完整文字解释"})
	body.Interaction.Kind = "future_input"
	r := learningcontent.Revision{ProtocolVersion: 1, ArtifactID: learningcontent.ArtifactID(a), Version: 1, CommittedVersion: 1, SpaceID: id, GoalID: id, GoalRevisionID: id, SessionID: id, ActivityID: id, ActivityRevision: 1, Generation: 1, ActorDeviceID: id, Status: "committed", CreatedAt: time.Now().UTC(), Body: body}
	raw, _ := json.Marshal(r)
	var decoded any
	if err = json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if err = doc.Components.Schemas["ContentRevision"].Value.VisitJSON(decoded, openapi3.EnableJSONSchema2020()); err != nil {
		t.Fatalf("可回退正文与正式协议不一致：%v", err)
	}
	for _, path := range []string{"/v1/learning/content/capabilities", "/v1/tutoring/sessions/{sessionID}/content", "/v1/learning/content/{artifactID}", "/v1/learning/content/{artifactID}/revisions", "/v1/learning/content/{artifactID}/answers", "/v1/tutoring/sessions/{sessionID}/operations/{operationID}"} {
		item := doc.Paths.Find(path)
		if item == nil {
			t.Fatalf("缺少正式内容接口：%s", path)
		}
		for _, op := range item.Operations() {
			if op.Security == nil || op.Extensions["x-required-scope"] == nil {
				t.Fatalf("缺少身份或权限合同：%s", path)
			}
		}
	}
}

func TestWorkspaceProxyAllowlist(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/web/nginx.conf")
	if err != nil {
		t.Fatal(err)
	}
	var patterns []*regexp.Regexp
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "location ~ ") {
			patterns = append(patterns, regexp.MustCompile(strings.TrimSuffix(strings.TrimPrefix(line, "location ~ "), " {")))
		}
	}
	for path, want := range map[string]bool{
		"/app/spaces/a/learn/b": true, "/content/a": true, "/v1/tutoring/sessions": true,
		"/v1/tutoring/sessions/a": true, "/v1/tutoring/sessions/a/actions": true, "/v1/tutoring/sessions/a/content": true,
		"/v1/tutoring/sessions/a/operations/b": true, "/v1/tutoring/proposals": true, "/v1/learning/content/a/answers": true,
		"/v1/knowledge/revisions/head": true, "/v1/knowledge/retrievals": true,
		"/v1/learning/start/capabilities": true, "/v1/learning/goals/a/start": true,
		"/v1/tutoring/sessions/a/knowledge-context": true, "/v1/learning/start/internal": false,
		"/v1/knowledge/structure": true, "/v1/knowledge/structure/capabilities": true,
		"/v1/knowledge/structure/concepts/a": true, "/v1/knowledge/structure/proposals": true,
		"/v1/knowledge/structure/proposals/a": true, "/v1/knowledge/structure/proposals/a/decisions": true,
		"/v1/knowledge/maintenance/proposals": true, "/v1/knowledge/maintenance/proposals/a": true,
		"/v1/knowledge/maintenance/proposals/a/approve": true, "/v1/knowledge/maintenance/proposals/a/reject": true,
		"/v1/knowledge/maintenance/rollbacks": true, "/v1/knowledge/revisions/a/tree": true, "/v1/knowledge/revisions/a/export": true,
		"/v1/knowledge/structure/internal": false, "/v1/knowledge/structure/concepts/a/clear": false,
		"/v1/knowledge/structure/proposals/a/force": false, "/v1/knowledge/maintenance/internal": false,
		"/v1/knowledge/maintenance/proposals/a/force": false, "/v1/knowledge/revisions/a/delete": false,
		"/admin": false, "/internal/privacy": false, "/mcp": false, "/v1/knowledge/imports": false,
	} {
		allowed := false
		for _, p := range patterns {
			allowed = allowed || p.MatchString(path)
		}
		if allowed != want {
			t.Fatalf("部署白名单边界错误：%s，允许=%v", path, allowed)
		}
	}
}
