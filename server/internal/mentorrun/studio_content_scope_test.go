package mentorrun

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Losing an older deletion, a patched-out section, or a neighboring unit must
// fail at the actual model boundary, including after confirmation and reload.
func TestPostgreSQLStudioContentRequestsKeepConfirmedScope(t *testing.T) {
	f, ctx, _, create := conversationStudioFixture(t)
	var requests []struct {
		System string
		Data   map[string]json.RawMessage
	}
	f.modelOverride = func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		var data map[string]json.RawMessage
		if err := json.Unmarshal([]byte(request.Messages[1].Content), &data); err != nil {
			t.Error(err)
			return
		}
		if data["directory"] != nil {
			sections := []any{}
			for i, topic := range []string{"连接建立", "滑动窗口", "拥塞重传"} {
				sections = append(sections, map[string]any{"id": []string{"s1", "s2", "s3"}[i], "topic": topic, "objective": "解释" + topic, "chapters": []int{1}, "topic_ids": create.ConversationScope.TopicIDs})
			}
			raw, _ := json.Marshal(map[string]any{"title": "TCP 笔记", "coverage": "partial", "sections": sections, "uncovered": []string{}, "explanation": "按所选话题组织。"})
			studioReply(w, string(raw), "stop")
			return
		}
		requests = append(requests, struct {
			System string
			Data   map[string]json.RawMessage
		}{request.Messages[0].Content, data})
		if data["allowed_claims"] != nil {
			studioReply(w, `{"overview":["s1-1:1"],"transitions":[],"uncovered":["需补充窗口示例。"]}`, "stop")
			return
		}
		studioReply(w, `{"title":"局部主题","coverage":"partial","claims":[{"text":"资料中的实际内容。","citations":[{"source":1,"fragment":1}]}]}`, "stop")
	}
	j, err := f.service.CreateStudioJob(ctx, f.actor, create)
	if err != nil {
		t.Fatal(err)
	}
	f.work(t)
	j, _ = contentSnapshot(t, f, ctx, j.ID)
	for _, edit := range []string{"删除拥塞重传，不要在窗口部分重新展开。", "先讲连接", "简短一些", "保留学习顺序"} {
		j, err = f.service.ChangeStudioPlan(ctx, f.actor, j.ID, "message", StudioPlanCommand{OperationID: uuid.NewString(), ExpectedVersion: j.Version, PlanRevision: j.ActivePlanRevision, Text: edit})
		if err != nil {
			t.Fatal(err)
		}
		f.work(t)
		j, _ = contentSnapshot(t, f, ctx, j.ID)
	}
	j, err = f.service.ChangeStudioPlan(ctx, f.actor, j.ID, "patch", StudioPlanCommand{OperationID: uuid.NewString(), ExpectedVersion: j.Version, PlanRevision: j.ActivePlanRevision, DeleteIDs: []string{"s3"}})
	if err != nil {
		t.Fatal(err)
	}
	j, err = f.service.ChangeStudioPlan(ctx, f.actor, j.ID, "confirm", StudioPlanCommand{OperationID: uuid.NewString(), ExpectedVersion: j.Version, PlanRevision: j.ActivePlanRevision})
	if err != nil || j.TotalUnits != 2 {
		t.Fatalf("confirmation: units=%d err=%v", j.TotalUnits, err)
	}
	for i := 0; i < 3; i++ {
		f.work(t)
	}
	j, _ = contentSnapshot(t, f, ctx, j.ID)
	if j.Status != "completed" || len(requests) != 3 {
		t.Fatalf("generation: status=%s requests=%d", j.Status, len(requests))
	}
	for i, request := range requests[:2] {
		var outline []struct{ ID, Topic, Objective string }
		_ = json.Unmarshal(request.Data["outline"], &outline)
		if len(outline) != 2 || outline[0].ID != "s1" || outline[0].Topic != "连接建立" || outline[1].Objective != "解释滑动窗口" {
			t.Errorf("unit %d missing compact confirmed outline", i+1)
		}
		var current struct {
			ID, SectionID string
			Slices        []json.RawMessage
		}
		// Use the wire names rather than depending on the production context type.
		var boundary map[string]json.RawMessage
		_ = json.Unmarshal(request.Data["current_unit"], &boundary)
		_ = json.Unmarshal(boundary["id"], &current.ID)
		_ = json.Unmarshal(boundary["section_id"], &current.SectionID)
		_ = json.Unmarshal(boundary["slices"], &current.Slices)
		if current.ID != []string{"s1-1", "s2-1"}[i] || current.SectionID != []string{"s1", "s2"}[i] || len(current.Slices) != 1 {
			t.Errorf("unit %d missing current boundary", i+1)
		}
		neighborKey := []string{"next_unit", "previous_unit"}[i]
		if !strings.Contains(string(request.Data[neighborKey]), []string{"解释滑动窗口", "解释连接建立"}[i]) {
			t.Errorf("unit %d missing adjacent responsibility", i+1)
		}
		if !strings.Contains(string(request.Data["applied_edits"]), "删除拥塞重传") || !strings.Contains(string(request.Data["excluded_content"]), "拥塞重传") {
			t.Errorf("unit %d lost deletion history", i+1)
		}
		var topics []struct{ ID string }
		_ = json.Unmarshal(request.Data["conversation_topics"], &topics)
		if len(topics) != 1 || topics[0].ID != create.ConversationScope.TopicIDs[0] {
			t.Errorf("unit %d selected topic scope changed", i+1)
		}
		for _, rule := range []string{"只写本段", "其他主题", "覆盖缺口", "标题"} {
			if !strings.Contains(request.System, rule) {
				t.Errorf("unit %d missing scope rule %s", i+1, rule)
			}
		}
	}
	if !strings.Contains(string(requests[2].Data["unit_coverage"]), "s2-1") || !strings.Contains(string(requests[2].Data["unit_coverage"]), "资料中的实际内容") {
		t.Error("summary did not receive later units' actual coverage")
	}
	artifact, err := f.service.studio.StudioGet(ctx, f.actor, j.Result.ArtifactID, j.Result.Version)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(artifact.Body.Summary.Uncovered, " "), "需补充窗口示例。") {
		t.Error("summary discarded the consolidated coverage explanation")
	}
	sections, err := f.service.studio.StudioSections(ctx, f.actor, artifact.ArtifactID, artifact.Version, 0, 2)
	if err != nil || len(sections.Items) != 2 {
		t.Fatalf("published sections: %v", err)
	}
	if sections.Items[0].Body.Title == sections.Items[1].Body.Title {
		t.Error("generated unit titles are indistinguishable")
	}
}
