package postgresstore

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ReferenceSourcesTx 只读取获准的冻结正文，沿用模型引用核验合同，不产生新的来源身份。
func (s *Store) ReferenceSourcesTx(ctx context.Context, tx pgx.Tx, id, goal string) ([]research.Source, error) {
	sources := []research.Source{}
	k, err := s.ContextTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	var owned bool
	if err = tx.QueryRow(ctx, `SELECT goal_id=$2 FROM knowledge_policies WHERE id=$1`, k.Policy.ID, goal).Scan(&owned); err != nil {
		return nil, err
	}
	if !owned || k.References == nil {
		return nil, scopeMissing()
	}
	r, err := loadScopedRevision(ctx, tx, k.ScopeSnapshotID)
	if err != nil {
		return nil, err
	}
	preferred := func(d knowledge.SnapshotDocument) bool {
		for _, e := range k.References.Entries {
			if e.Role == "prefer" && e.CollectionID == d.CollectionID && e.RevisionID == d.KnowledgeRevisionID && (e.DocumentID == "" || e.DocumentID == d.Revision.DocumentID) {
				return true
			}
		}
		return false
	}
	sort.SliceStable(r.Documents, func(i, j int) bool { return preferred(r.Documents[i]) && !preferred(r.Documents[j]) })
	remaining := research.MaxText
	for _, d := range r.Documents {
		text := d.Revision.CanonicalMarkdown
		origin := 0
		if d.SelectedRange != nil {
			origin = d.SelectedRange.Start
			text = text[d.SelectedRange.Start:d.SelectedRange.End]
		}
		coverage := "complete_text"
		limit := min(4000, remaining)
		if len(text) > limit {
			for limit > 0 && !utf8.RuneStart(text[limit]) {
				limit--
			}
			text, coverage = text[:limit], "partial_text"
		}
		if text == "" {
			continue
		}
		fragment := research.Fragment{ID: uuid.NewSHA1(uuid.MustParse(d.Revision.ID), []byte(fmt.Sprintf("reference:%d:%d", origin, len(text)))).String(), Start: 0, End: len(text), Text: text}
		sources = append(sources, research.Source{SpaceID: learningspace.Scope(ctx), GoalID: goal, Purpose: "goal_reference", ID: d.Revision.ID, RevisionID: d.KnowledgeRevisionID, Locator: d.Path, Title: d.Path, Kind: "reference", Status: "adopted", Fingerprint: fmt.Sprintf("%x", sha256.Sum256([]byte(text))), Parser: knowledge.ParserVersion, Coverage: coverage, StorageAllowed: true, Text: text, Fragments: []research.Fragment{fragment}, KnowledgeRevisionID: d.KnowledgeRevisionID, CollectionID: d.CollectionID})
		remaining -= len(text)
		if len(sources) == research.MaxSources || remaining == 0 {
			break
		}
	}
	return sources, nil
}

// 已确认范围只追加概念版本；不会发布新政策，也不会变更旧课堂的上下文。
func (s *Store) PublishReferenceTeachingTx(ctx context.Context, tx pgx.Tx, run, goal, goalRevision, id string, concept knowledge.ConceptRevision) (knowledge.KnowledgeContextRevision, error) {
	k, err := s.ContextTx(ctx, tx, id)
	if err != nil {
		return k, err
	}
	if k.Policy.GoalRevisionID != goalRevision {
		return k, &knowledge.Error{Code: knowledge.CodeRevisionConflict}
	}
	sources, err := s.ReferenceSourcesTx(ctx, tx, id, goal)
	if err != nil {
		return k, err
	}
	if err = (research.Synthesis{Points: []research.Point{{Text: concept.Name, Citations: concept.Support}}}).Validate(sources); err != nil {
		return k, err
	}
	k.PreviousRevisionID, k.ID = k.ID, uuid.NewString()
	return s.appendContextConceptTx(ctx, tx, run, goal, k, concept)
}
