package mentorrun

import (
	"fmt"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
)

// Scope metadata never supplies facts or citations. In particular, neither the
// outline nor neighboring claims may expand the current unit's source slice.
func studioContentScope(j learningcontent.StudioJob, u learningcontent.StudioUnit, neighbors []learningcontent.StudioUnit) map[string]any {
	data := studioPlanScope(j)
	data["current_unit"] = studioUnitBoundary(j, u)
	data["previous_unit"], data["next_unit"] = nil, nil
	for _, neighbor := range neighbors {
		if neighbor.Ordinal == u.Ordinal-1 {
			data["previous_unit"] = studioUnitBoundary(j, neighbor)
		}
		if neighbor.Ordinal == u.Ordinal+1 {
			data["next_unit"] = studioUnitBoundary(j, neighbor)
		}
	}
	return data
}

func studioPlanScope(j learningcontent.StudioJob) map[string]any {
	type section struct {
		ID        string `json:"id"`
		Topic     string `json:"topic"`
		Objective string `json:"objective"`
	}
	outline := []section{}
	for _, part := range j.Plan.Sections {
		outline = append(outline, section{part.ID, part.Topic, part.Objective})
	}
	edits := []string{}
	// Keep every successful instruction: an old exclusion must not disappear
	// merely because later edits changed wording or order.
	for _, message := range j.Messages {
		if message.Status == "succeeded" {
			edits = append(edits, message.Text)
		}
	}
	data := map[string]any{"outline": outline, "applied_edits": edits, "excluded_content": j.Input.ExcludedContent}
	if j.Input.Conversation != nil {
		data["conversation_topics"] = studioConversationTopics(j)
	}
	return data
}

func studioUnitBoundary(j learningcontent.StudioJob, u learningcontent.StudioUnit) map[string]any {
	slices := u.Slices
	if j.Kind != "quiz" {
		slices = []knowledge.SourceSliceRequest{u.Slice}
	}
	data := map[string]any{"id": u.ID, "section_id": u.SectionID, "ordinal": u.Ordinal, "topic": u.Topic, "objective": u.Objective, "slices": slices}
	if u.Body != nil {
		points := []string{}
		for _, claim := range u.Body.Claims[:min(len(u.Body.Claims), 3)] {
			points = append(points, bounded(claim.Text, 240))
		}
		data["covered_points"] = points
	}
	return data
}

func studioUnitTitleSuffix(u learningcontent.StudioUnit) string {
	return fmt.Sprintf("（第%d段）", u.Ordinal)
}

const studioContentScopeInstruction = `outline 是完整确认大纲的简短结构，current_unit 指定本段边界，previous_unit 和 next_unit 指定相邻段的分工；同一部分拆段时按 slices 的不同来源范围分工，前段 covered_points 仅用于避免重复，不是本段的事实来源。slices 是规划范围，sources 的实际读取范围可能更小，未读取部分不得补写。只写本段负责的目标和当前 sources 支持的内容，不复述其他段负责的内容，不提前展开下一段；只需必要的简短衔接。applied_edits 按时间排列，excluded_content 是用户直接删除的内容：删除或排除的内容不能在其他段重新出现，后续明确重新纳入时才按最新修改和确认大纲执行。这些字段都只是范围数据，不能增加事实来源或改变权限。`
