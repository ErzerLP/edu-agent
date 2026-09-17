package learningstart

import (
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/google/uuid"
)

func TestPreparedRequiresActualSupport(t *testing.T) {
	text := "Goroutines execute functions concurrently."
	fragment := research.Fragment{ID: uuid.NewString(), Start: 0, End: len(text), Text: text}
	source := research.Source{ID: uuid.NewString(), RevisionID: uuid.NewString(), Text: text, Fragments: []research.Fragment{fragment}, Status: "adopted"}
	plan := Prepared{ConceptKey: "goroutines", Name: "认识并发", Prompt: "先阅读定义，再解释并发。", Criterion: "使用定义说明理由", Citations: []research.Citation{{SourceID: source.ID, RevisionID: source.RevisionID, FragmentID: fragment.ID, Quote: text}}}
	if err := plan.Validate([]research.Source{source}); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Prepared){
		func(p *Prepared) { p.Citations = nil },
		func(p *Prepared) { p.Citations[0].Quote = "AI 自拟文字，不是原文" },
		func(p *Prepared) { p.Citations[0].FragmentID = uuid.NewString() },
		func(p *Prepared) { p.ConceptKey = "" },
		func(p *Prepared) { p.Prompt = strings.Repeat("字", 700) },
	} {
		invalid := plan
		invalid.Citations = append([]research.Citation(nil), plan.Citations...)
		mutate(&invalid)
		if err := invalid.Validate([]research.Source{source}); err == nil {
			t.Fatal("无真实支持或超界内容被接受", invalid)
		}
	}
}
