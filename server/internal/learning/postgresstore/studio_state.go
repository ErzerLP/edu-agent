package postgresstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	knowledgepg "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/jackc/pgx/v5"
)

// ReadStudioLearningStateTx reads formal records across the learning space.
// The caller holds the learning, tutoring and knowledge read gates and a live
// actor scope. All counts, ranking and records use one PostgreSQL snapshot.
// Selection considers the entire space before limiting the result to 24
// concepts; records are never limited to a conversation or an arbitrary page.
func ReadStudioLearningStateTx(ctx context.Context, tx pgx.Tx, directionRevision string) (learning.StudioLearningState, error) {
	state := learning.StudioLearningState{
		DirectionRevision: directionRevision, Coverage: "empty",
		RecentConcepts: []learning.StudioLearningStateItem{}, NeedsReview: []learning.StudioLearningStateItem{},
		HelpUsed: []learning.StudioLearningStateItem{}, MasteredEvidence: []learning.StudioLearningStateItem{},
		PendingAssessments: []learning.StudioLearningStateItem{}, SuggestedFocus: []string{},
	}
	rows, err := tx.Query(ctx, studioLearningStateSQL, learningspace.Scope(ctx), directionRevision)
	if err != nil {
		return state, fmt.Errorf("read Studio learning state: %w", err)
	}
	defer rows.Close()
	var order []string
	concepts := map[string]*studioStateConcept{}
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&state.AsOfEventSeq, &state.TotalConcepts, &raw); err != nil {
			return state, err
		}
		if len(raw) == 0 {
			continue
		}
		var r studioStateRecord
		if err = json.Unmarshal(raw, &r); err != nil {
			return state, err
		}
		c := concepts[r.NodeRevisionID]
		if c == nil {
			c = &studioStateConcept{recent: r.StudioLearningStateItem}
			concepts[r.NodeRevisionID] = c
			order = append(order, r.NodeRevisionID)
		}
		// SQL orders records newest first, so each category keeps its latest
		// reference. Only structured metadata reaches this adapter.
		if c.needs == nil && (r.Outcome == learning.OutcomeFail || r.Outcome == learning.OutcomePartial || (r.Help != "" && r.Help != learning.HelpNone)) {
			item := r.StudioLearningStateItem
			c.needs = &item
		}
		if c.help == nil && r.Help != "" && r.Help != learning.HelpNone {
			item := r.StudioLearningStateItem
			c.help = &item
		}
		if r.Status == "pending" || r.Status == "provisional" {
			if c.pending == nil {
				item := r.StudioLearningStateItem
				c.pending = &item
			}
			c.pendingCurrent = c.pendingCurrent || r.CurrentDirection
		}
		if r.EvidenceID != "" && r.CurrentDirection {
			c.evidence = append(c.evidence, learning.AcceptedEvidence{
				ID: r.EvidenceID, ActivityID: r.ActivityID, GoalRevisionID: r.GoalRevisionID,
				NodeRevisionID: r.NodeRevisionID, Kind: r.Kind, Outcome: r.Outcome, Help: r.Help,
				ReceivedAt: r.EvidenceAt, AcceptedEventSequence: r.AcceptedEventSeq,
			})
			if c.mastered == nil && r.Outcome == learning.OutcomePass && (r.Help == learning.HelpNone || r.Help == learning.HelpHint) {
				item := r.StudioLearningStateItem
				item.RecordID, item.RecordType = r.EvidenceID, "evidence"
				item.OccurredAt = r.EvidenceAt
				c.mastered = &item
			}
		}
	}
	if err = rows.Err(); err != nil {
		return state, err
	}
	rows.Close()
	labels, err := knowledgepg.ReadLearningNodeLabelsTx(ctx, tx, order)
	if err != nil {
		return state, err
	}
	for _, id := range order {
		c := concepts[id]
		label := labels[id]
		if label == "" {
			label = "知识点"
		}
		for _, item := range []*learning.StudioLearningStateItem{&c.recent, c.needs, c.help, c.pending, c.mastered} {
			if item != nil {
				item.Label = label
			}
		}
		state.RecentConcepts = append(state.RecentConcepts, c.recent)
		if c.needs != nil {
			state.NeedsReview = append(state.NeedsReview, *c.needs)
			if len(state.SuggestedFocus) < 4 {
				label := "巩固：" + c.needs.Label
				if c.needs.Help != "" && c.needs.Help != learning.HelpNone {
					label = "独立练习：" + c.needs.Label
				}
				state.SuggestedFocus = append(state.SuggestedFocus, label)
			}
		}
		if c.help != nil {
			state.HelpUsed = append(state.HelpUsed, *c.help)
		}
		if c.pending != nil {
			state.PendingAssessments = append(state.PendingAssessments, *c.pending)
		}
		var pending []learning.PendingAssessment
		if c.pendingCurrent {
			pending = []learning.PendingAssessment{{NodeRevisionID: id}}
		}
		// Use the formal reducer: one passing answer is not retained mastery,
		// and provisional assessments cannot become current mastery.
		if c.mastered != nil && learning.ReduceNode(id, c.evidence, nil, pending).Mastery.State == learning.MasteryRetained {
			c.mastered.Status = string(learning.MasteryRetained)
			state.MasteredEvidence = append(state.MasteredEvidence, *c.mastered)
		}
	}
	state.IncludedConcepts = len(state.RecentConcepts)
	if state.TotalConcepts > 0 {
		state.Coverage = "complete"
		if state.TotalConcepts > state.IncludedConcepts {
			state.Coverage = "partial"
		}
	}
	return state, nil
}

type studioStateRecord struct {
	learning.StudioLearningStateItem
	EvidenceID       string                `json:"evidence_id"`
	ActivityID       string                `json:"activity_id"`
	Kind             learning.EvidenceKind `json:"kind"`
	EvidenceAt       time.Time             `json:"evidence_at"`
	AcceptedEventSeq int64                 `json:"accepted_event_seq"`
}

type studioStateConcept struct {
	recent                         learning.StudioLearningStateItem
	needs, help, pending, mastered *learning.StudioLearningStateItem
	pendingCurrent                 bool
	evidence                       []learning.AcceptedEvidence
}

// This query deliberately names only metadata columns. It does not read
// attempt payloads, question prompts, assessment quotations or chat tables.
// All valid evidence for selected concepts is streamed internally so the
// reducer retains its full-history semantics; only the bounded result is sent
// to a model. Empty snapshots still return the formal event high-water mark.
var studioLearningStateSQL = strings.TrimSpace(`
WITH activities AS (
 SELECT a.id,a.target_node_revision_id,a.goal_revision_id,
   g.id=(SELECT latest.id FROM learning_goal_revisions latest WHERE latest.goal_id=g.goal_id ORDER BY latest.revision DESC LIMIT 1)
   AND ($2='' OR EXISTS (
     SELECT 1 FROM learning_direction_goals dg JOIN learning_direction_revisions dr
       ON dr.space_id=dg.space_id AND dr.privacy_generation=dg.privacy_generation AND dr.version=dg.direction_version
     WHERE dg.space_id=g.space_id AND dr.id::text=$2 AND dg.goal_revision_id=g.id
   )) AS current_direction
 FROM learning_activities a JOIN learning_goal_revisions g ON g.id=a.goal_revision_id
 WHERE g.space_id=$1::uuid AND g.source<>'privacy_erasure'
), attempts AS (
 SELECT a.target_node_revision_id AS node_revision_id,a.goal_revision_id,a.current_direction,
   t.id AS record_id,'attempt'::text AS record_type,t.received_at AS occurred_at,t.help_level AS help,
   CASE WHEN d.disposition='voided' THEN 'voided'
        WHEN i.evidence_id IS NOT NULL THEN 'invalidated'
        WHEN d.disposition='provisional' THEN 'provisional'
        WHEN d.id IS NULL THEN 'pending'
        WHEN e.id IS NOT NULL THEN 'accepted' ELSE 'assessed' END AS status,
   CASE WHEN i.evidence_id IS NULL THEN COALESCE(e.outcome,'') ELSE '' END AS outcome,
   CASE WHEN i.evidence_id IS NULL THEN e.id END AS evidence_id,
   a.id AS activity_id,COALESCE(e.evidence_kind,'') AS kind,COALESCE(e.accepted_event_seq,0) AS accepted_event_seq,e.received_at AS evidence_at
 FROM activities a JOIN learning_attempts t ON t.activity_id=a.id
 LEFT JOIN learning_assessments assessment ON assessment.attempt_id=t.id
 LEFT JOIN LATERAL (
   SELECT id,disposition FROM learning_assessment_decisions WHERE assessment_id=assessment.id ORDER BY version DESC LIMIT 1
 ) d ON true
 LEFT JOIN learning_evidence e ON e.decision_id=d.id AND d.disposition IN ('accepted','overridden') AND e.rubric_revision<>'[redacted]'
 LEFT JOIN learning_evidence_invalidations i ON i.evidence_id=e.id
), records AS (
 SELECT * FROM attempts
 UNION ALL
 SELECT a.target_node_revision_id,a.goal_revision_id,a.current_direction,
   h.id,'help',h.created_at,h.level,'help_used','',NULL::uuid,a.id,'',0::bigint,NULL::timestamptz
 FROM activities a JOIN learning_exercises x ON x.activity_id=a.id JOIN learning_exercise_help h ON h.exercise_id=x.id
), ranked AS (
 SELECT node_revision_id,
   min(CASE WHEN outcome IN ('fail','partial') OR help<>'none' THEN 0
            WHEN occurred_at>=transaction_timestamp()-interval '30 days' THEN 1 ELSE 2 END) AS priority,
   max(occurred_at) AS latest_at
 FROM records GROUP BY node_revision_id
), selected AS (
 SELECT * FROM ranked ORDER BY priority,latest_at DESC,node_revision_id LIMIT 24
), selected_records AS (
 SELECT r.*,s.priority,s.latest_at
 FROM selected s JOIN records r USING(node_revision_id)
)
SELECT clock.current_event_seq,(SELECT count(*) FROM ranked),
       CASE WHEN r.node_revision_id IS NULL THEN NULL ELSE to_jsonb(r) END
FROM learning_event_clock clock LEFT JOIN selected_records r ON true
WHERE clock.singleton_id=1
ORDER BY r.priority,r.latest_at DESC,r.node_revision_id,r.occurred_at DESC,r.record_id
`)
