package learningcontent

import (
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

func selectionFixture() (Revision, EditRequest) {
	r := Revision{SpaceID: uuid.NewString(), GoalID: uuid.NewString(), SessionID: uuid.NewString(), ArtifactID: uuid.NewString(), Version: 1, CommittedVersion: 1, Status: "committed", Body: Body{Blocks: []Block{{ID: uuid.NewString(), Kind: "question", Text: "说明中文🙂题目", Fallback: "说明中文🙂题目"}, {ID: uuid.NewString(), Kind: "callout", Text: "旁边正文", Fallback: "旁边正文"}}}}
	s := Selection{SpaceID: r.SpaceID, GoalID: r.GoalID, SessionID: r.SessionID, ArtifactID: r.ArtifactID, Version: 1, BlockID: r.Body.Blocks[0].ID, Start: 6, End: 12, Hash: TextHash("中文")}
	return r, EditRequest{Selection: s, Action: "example"}
}

func TestSelectionAndPatchPreserveUnselectedContent(t *testing.T) {
	r, request := selectionFixture()
	before := fingerprint(r)
	body, err := Patch(r, request, Candidate{Text: "新的例子"}, "换个例子", uuid.NewString(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if len(body.Blocks) != 3 || !reflect.DeepEqual(body.Blocks[0], r.Body.Blocks[0]) || !reflect.DeepEqual(body.Blocks[2], r.Body.Blocks[1]) || fingerprint(r) != before {
		t.Fatal("未选正文或原题被修改")
	}
	if body.Change.BaseVersion != 1 || body.Change.Reason != "换个例子" || len(body.Lineage) != 1 {
		t.Fatal("丢失生成依据")
	}
	r.Version = 2
	r.CommittedVersion = 2
	r.Body = body
	request.Selection.Version = 2
	request.Selection.BlockID = body.Blocks[1].ID
	request.Selection.Start = 0
	request.Selection.End = 6
	request.Selection.Hash = TextHash("新的")
	request.Action = "rewrite"
	body, err = Patch(r, request, Candidate{Text: "另一种"}, "修改这里", uuid.NewString(), "fixture")
	if err != nil || body.Blocks[1].Text != "另一种例子" || len(body.Blocks) != 3 {
		t.Fatalf("未精确替换：%+v %v", body, err)
	}
}

func TestSelectionRejectsStaleForgedAndInvalidRanges(t *testing.T) {
	for _, kind := range []string{"version", "hash", "space", "goal", "session", "split_utf8", "missing_block", "fake_citation", "empty"} {
		t.Run(kind, func(t *testing.T) {
			r, c := selectionFixture()
			candidate := Candidate{Text: "例子"}
			switch kind {
			case "version":
				c.Selection.Version++
			case "hash":
				c.Selection.Hash = TextHash("伪造")
			case "space":
				c.Selection.SpaceID = uuid.NewString()
			case "goal":
				c.Selection.GoalID = uuid.NewString()
			case "session":
				c.Selection.SessionID = uuid.NewString()
			case "split_utf8":
				c.Selection.Start++
			case "missing_block":
				c.Selection.BlockID = uuid.NewString()
			case "fake_citation":
				candidate.ReferenceIDs = []string{uuid.NewString()}
			case "empty":
				candidate.Text = ""
			}
			if _, err := Patch(r, c, candidate, "解释这里", uuid.NewString(), "fixture"); err == nil {
				t.Fatal("错误定位或非法候选被接受")
			}
		})
	}
	r, c := selectionFixture()
	r.Status = "draft"
	if _, err := Select(r, c.Selection); !errors.Is(err, ErrConflict) {
		t.Fatal("草稿进入正式编辑", err)
	}
}
