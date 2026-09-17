package learningchange

import (
	"github.com/google/uuid"
	"testing"
)

func TestCandidateRejectsCyclesAndUnsupportedEffects(t *testing.T) {
	c := Candidate{Kind: "route", Trigger: "user_request", Reason: "先补前置", Steps: []Step{{NodeRevisionID: uuid.NewString(), Name: "基础", Criterion: "说明原因", Prompt: "说明原因", Difficulty: 1, Prerequisites: []int{}}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Steps[0].Prerequisites = []int{0}
	if c.Validate() == nil {
		t.Fatal("接受循环前置")
	}
	c.Steps[0].Prerequisites = nil
	c.Kind = "shell"
	if c.Validate() == nil {
		t.Fatal("接受未实现的外部副作用")
	}
	c.Kind = "route"
	c.Trigger = "activity_feedback"
	if c.Validate() == nil {
		t.Fatal("没有正式证据却伪称活动反馈")
	}
}
