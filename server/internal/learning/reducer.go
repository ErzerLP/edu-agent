package learning

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"
)

type MasteryState string

const (
	MasteryUnseen      MasteryState = "unseen"
	MasteryLearning    MasteryState = "learning"
	MasteryProvisional MasteryState = "provisional"
	MasteryRetained    MasteryState = "retained"
)

type ReviewSchedule struct {
	NodeRevisionID string          `json:"node_revision_id"`
	Step           int             `json:"step"`
	DueAt          time.Time       `json:"due_at"`
	Intervals      []time.Duration `json:"intervals"`
	PolicyVersion  string          `json:"policy_version"`
}

type MasteryProjection struct {
	NodeRevisionID     string               `json:"node_revision_id"`
	State              MasteryState         `json:"state"`
	BaselineState      MasteryState         `json:"baseline_state"`
	ValidEvidenceCount int                  `json:"valid_evidence_count"`
	Kinds              map[EvidenceKind]int `json:"kinds"`
	Outcomes           map[Outcome]int      `json:"outcomes"`
	Help               map[HelpLevel]int    `json:"help"`
	LastEvidenceAt     *time.Time           `json:"last_evidence_at,omitempty"`
	PendingAssessments int                  `json:"pending_assessments"`
	UncertaintyReasons []string             `json:"uncertainty_reasons"`
	ReducerVersion     string               `json:"reducer_version"`
}

type MisconceptionStatus string

const (
	MisconceptionProposed   MisconceptionStatus = "proposed"
	MisconceptionSupported  MisconceptionStatus = "supported"
	MisconceptionChallenged MisconceptionStatus = "challenged"
	MisconceptionResolved   MisconceptionStatus = "resolved"
)

type MisconceptionHypothesis struct {
	ID                 string              `json:"misconception_id"`
	Revision           int64               `json:"revision"`
	NodeRevisionID     string              `json:"node_revision_id"`
	RubricItemID       string              `json:"rubric_item_id"`
	CandidateHash      string              `json:"candidate_hash"`
	Candidate          string              `json:"candidate"`
	Status             MisconceptionStatus `json:"status"`
	SourceEvidenceIDs  []string            `json:"source_evidence_ids"`
	CounterEvidenceIDs []string            `json:"counter_evidence_ids"`
	CausedByEvidenceID string              `json:"caused_by_evidence_id"`
}

type NodeReduction struct {
	Mastery        MasteryProjection         `json:"mastery"`
	Review         *ReviewSchedule           `json:"review,omitempty"`
	Misconceptions []MisconceptionHypothesis `json:"misconceptions"`
}

var reviewIntervals = []time.Duration{24 * time.Hour, 3 * 24 * time.Hour, 7 * 24 * time.Hour, 14 * 24 * time.Hour, 30 * 24 * time.Hour}

func ReduceNode(nodeRevisionID string, all []AcceptedEvidence, invalidated map[string]bool, pending []PendingAssessment) NodeReduction {
	values := make([]AcceptedEvidence, 0, len(all))
	for _, evidence := range all {
		if evidence.NodeRevisionID == nodeRevisionID && !invalidated[evidence.ID] {
			values = append(values, evidence)
		}
	}
	SortEvidence(values)
	projection := MasteryProjection{NodeRevisionID: nodeRevisionID, BaselineState: MasteryUnseen, State: MasteryUnseen, Kinds: map[EvidenceKind]int{}, Outcomes: map[Outcome]int{}, Help: map[HelpLevel]int{}, ReducerVersion: MasteryReducerVersion}
	for _, evidence := range values {
		projection.ValidEvidenceCount++
		projection.Kinds[evidence.Kind]++
		projection.Outcomes[evidence.Outcome]++
		projection.Help[evidence.Help]++
		copy := evidence.ReceivedAt
		projection.LastEvidenceAt = &copy
	}
	if len(values) > 0 {
		projection.BaselineState = MasteryLearning
	}
	if retained(values) {
		projection.BaselineState = MasteryRetained
	}
	projection.State = projection.BaselineState
	if len(pending) > 0 {
		projection.State = MasteryProvisional
		projection.PendingAssessments = len(pending)
		for _, assessment := range pending {
			projection.UncertaintyReasons = append(projection.UncertaintyReasons, assessment.Reasons...)
		}
		sort.Strings(projection.UncertaintyReasons)
	}
	return NodeReduction{Mastery: projection, Review: reduceReview(nodeRevisionID, values), Misconceptions: reduceMisconceptions(nodeRevisionID, values)}
}

func retained(values []AcceptedEvidence) bool {
	segmentStart := 0
	for i := range values {
		if values[i].Outcome == OutcomeFail || values[i].Outcome == OutcomePartial {
			segmentStart = i + 1
		}
	}

	var earliest, alternate *AcceptedEvidence
	seenActivities := map[string]bool{}
	for i := segmentStart; i < len(values); i++ {
		evidence := &values[i]
		if !successfulActiveRecall(*evidence) {
			continue
		}
		candidate := earliest
		if candidate != nil && candidate.ActivityID == evidence.ActivityID {
			candidate = alternate
		}
		if candidate != nil && evidence.ReceivedAt.Sub(candidate.ReceivedAt) >= 24*time.Hour {
			return true
		}
		if seenActivities[evidence.ActivityID] {
			continue
		}
		seenActivities[evidence.ActivityID] = true
		if earliest == nil {
			earliest = evidence
		} else if alternate == nil {
			alternate = evidence
		}
	}
	return false
}

func successfulActiveRecall(evidence AcceptedEvidence) bool {
	return (evidence.Kind == EvidencePracticeRecall || evidence.Kind == EvidenceReviewRecall) && evidence.Outcome == OutcomePass && lowHelp(evidence.Help)
}

func lowHelp(value HelpLevel) bool { return value == HelpNone || value == HelpHint }

func reduceReview(node string, values []AcceptedEvidence) *ReviewSchedule {
	var result *ReviewSchedule
	for _, evidence := range values {
		if evidence.Kind != EvidencePracticeRecall && evidence.Kind != EvidenceReviewRecall {
			continue
		}
		reset := evidence.Outcome != OutcomePass || !lowHelp(evidence.Help)
		if result == nil || reset {
			result = &ReviewSchedule{NodeRevisionID: node, Step: 0, DueAt: evidence.ReceivedAt.Add(reviewIntervals[0]), Intervals: append([]time.Duration(nil), reviewIntervals...), PolicyVersion: ReviewPolicyVersion}
			continue
		}
		if !evidence.ReceivedAt.Before(result.DueAt) {
			if result.Step < len(reviewIntervals)-1 {
				result.Step++
			}
			result.DueAt = evidence.ReceivedAt.Add(reviewIntervals[result.Step])
		}
	}
	return result
}

func reduceMisconceptions(node string, values []AcceptedEvidence) []MisconceptionHypothesis {
	type collected struct {
		candidate string
		rubric    string
		failures  []AcceptedEvidence
		counters  []AcceptedEvidence
	}
	groups := map[string]*collected{}
	for _, evidence := range values {
		if evidence.Outcome == OutcomeFail || evidence.Outcome == OutcomePartial {
			for _, candidate := range evidence.Misconceptions {
				normalized := normalizeCandidate(candidate.Text)
				if normalized == "" {
					continue
				}
				key := candidate.RubricItemID + "\x00" + normalized
				if groups[key] == nil {
					groups[key] = &collected{candidate: normalized, rubric: candidate.RubricItemID}
				}
				groups[key].failures = append(groups[key].failures, evidence)
			}
		}
	}
	for _, group := range groups {
		lastFailure := group.failures[len(group.failures)-1].ReceivedAt
		for _, evidence := range values {
			if evidence.ReceivedAt.Before(lastFailure) || evidence.ReceivedAt.Equal(lastFailure) {
				continue
			}
			for _, outcome := range evidence.RubricOutcomes {
				if outcome.RubricItemID == group.rubric && outcome.Conclusion == ConclusionPass {
					group.counters = append(group.counters, evidence)
					break
				}
			}
		}
	}
	result := make([]MisconceptionHypothesis, 0, len(groups))
	for _, group := range groups {
		hash := sha256.Sum256([]byte(group.candidate))
		idHash := sha256.Sum256([]byte(node + "\n" + group.rubric + "\n" + hex.EncodeToString(hash[:])))
		status := MisconceptionProposed
		activities := map[string]bool{}
		for _, evidence := range group.failures {
			activities[evidence.ActivityID] = true
		}
		if len(activities) >= 2 {
			status = MisconceptionSupported
		}
		if len(group.counters) > 0 {
			status = MisconceptionChallenged
		}
		if retained(group.counters) {
			status = MisconceptionResolved
		}
		hypothesis := MisconceptionHypothesis{ID: formatUUID(idHash[:16]), Revision: int64(len(group.failures) + len(group.counters)), NodeRevisionID: node, RubricItemID: group.rubric, CandidateHash: hex.EncodeToString(hash[:]), Candidate: group.candidate, Status: status}
		for _, evidence := range group.failures {
			hypothesis.SourceEvidenceIDs = append(hypothesis.SourceEvidenceIDs, evidence.ID)
		}
		for _, evidence := range group.counters {
			hypothesis.CounterEvidenceIDs = append(hypothesis.CounterEvidenceIDs, evidence.ID)
		}
		if len(hypothesis.CounterEvidenceIDs) > 0 {
			hypothesis.CausedByEvidenceID = hypothesis.CounterEvidenceIDs[len(hypothesis.CounterEvidenceIDs)-1]
		} else {
			hypothesis.CausedByEvidenceID = hypothesis.SourceEvidenceIDs[len(hypothesis.SourceEvidenceIDs)-1]
		}
		result = append(result, hypothesis)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func normalizeCandidate(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
}

func formatUUID(value []byte) string {
	encoded := hex.EncodeToString(value)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32]
}
