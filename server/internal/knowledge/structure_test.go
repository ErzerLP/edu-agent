package knowledge

import (
	"testing"

	"github.com/google/uuid"
)

func TestStructureRejectsUnsupportedClaimsAndUnboundedInput(t *testing.T) {
	command := func() StructureCommand {
		return StructureCommand{OperationID: uuid.NewString(), Generation: 1, Kind: "edit", Reason: "核对来源", ActorDeviceID: uuid.NewString(), Edits: []StructureEdit{{ConceptID: uuid.NewString(), Name: "同名不会去重", Content: ConceptContent{SourceStatus: "candidate"}}}}
	}
	for name, change := range map[string]func(*StructureCommand){
		"冲突没有具体主张": func(c *StructureCommand) { c.Edits[0].Content.SourceStatus = "conflict" },
		"主张伪造出处序号": func(c *StructureCommand) {
			c.Edits[0].Content.Claims = []ConceptClaim{{Text: "断言", Sources: []int{99}}}
		},
		"主张没有出处与缺口": func(c *StructureCommand) { c.Edits[0].Content.Claims = []ConceptClaim{{Text: "无出处断言"}} },
		"自我前置": func(c *StructureCommand) {
			c.Edits[0].Content.Relations = []ConceptRelation{{TargetID: c.Edits[0].ConceptID, Kind: "prerequisite"}}
		},
		"伪造学习状态":   func(c *StructureCommand) { c.Edits[0].Content.SourceStatus = "mastered" },
		"同一身份重复写入": func(c *StructureCommand) { c.Edits = append(c.Edits, c.Edits[0]) },
		"无界关系": func(c *StructureCommand) {
			for range 41 {
				c.Edits[0].Content.Relations = append(c.Edits[0].Content.Relations, ConceptRelation{TargetID: uuid.NewString(), Kind: "related"})
			}
		},
		"回滚同时注入编辑": func(c *StructureCommand) { c.Kind = "compensate"; c.Compensates = uuid.NewString() },
	} {
		t.Run(name, func(t *testing.T) {
			c := command()
			change(&c)
			if c.Validate() == nil {
				t.Fatal("接受非法知识结构")
			}
		})
	}
	c := command()
	c.Edits[0].Content.Claims = []ConceptClaim{{Text: "需要核对的主张", Conditions: "尚未建立适用范围", Gap: "缺少可引用的实验来源"}}
	if err := c.Validate(); err != nil {
		t.Fatal("明确标记缺口的候选被拒绝", err)
	}
}
