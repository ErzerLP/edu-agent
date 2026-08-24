package learning

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/tutoring"
)

type proposalTestStore struct {
	commits           int
	archives          int
	lastCommit        CommitRequest
	session           tutoring.Session
	proposal          ProposalArtifact
	activity          Activity
	attempt           Attempt
	assessment        AssessmentArtifact
	decision          AssessmentDecision
	goal              GoalRevision
	route             RouteRevision
	freeQuestion      tutoring.FreeQuestion
	freeAnswer        tutoring.FreeAnswer
	evidence          []AcceptedEvidence
	misconceptions    []MisconceptionHypothesis
	aggregateVer      int64
	timeline          TimelinePage
	lookupResult      OperationResult
	lookupErr         error
	lookupFound       bool
	authorityAsOf     int64
	advanceOnArchive  *tutoring.Session
	lastArchivedError Error
	loads             int
}

func (s *proposalTestStore) Commit(_ context.Context, request CommitRequest) (OperationResult, error) {
	s.commits++
	s.lastCommit = request
	return OperationResult{Status: "succeeded", AggregateType: request.Operation.AggregateType, AggregateID: request.Operation.AggregateID}, nil
}
func (s *proposalTestStore) LookupOperation(context.Context, OperationLookup) (OperationResult, error, bool) {
	return s.lookupResult, s.lookupErr, s.lookupFound
}
func (s *proposalTestStore) ArchiveRejection(_ context.Context, rejection OperationRejection) (OperationResult, error) {
	s.archives++
	if s.advanceOnArchive != nil {
		s.session = *s.advanceOnArchive
		for _, expected := range rejection.Expectations {
			if expected.Type == "session" && expected.ID == s.session.ID && expected.ExpectedVersion != s.session.AggregateVer {
				rejection.Error = Error{Code: CodeVersionConflict, AggregateType: "session", AggregateID: s.session.ID, ExpectedVersion: expected.ExpectedVersion, CurrentVersion: s.session.AggregateVer, AsOfEventSequence: s.authorityAsOf}
				break
			}
		}
	}
	s.lastArchivedError = rejection.Error
	s.lastCommit = CommitRequest{DeviceID: rejection.Lookup.DeviceID, RequestHash: rejection.Lookup.RequestHash, Operation: OperationEnvelope{OperationID: rejection.Lookup.OperationID, AggregateType: rejection.AggregateType, AggregateID: rejection.AggregateID}}
	return OperationResult{Status: "rejected", Archived: true, AggregateType: rejection.AggregateType, AggregateID: rejection.AggregateID}, &rejection.Error
}
func (*proposalTestStore) CurrentSession(context.Context) (SessionView, error) {
	return SessionView{}, nil
}
func (*proposalTestStore) Session(context.Context, string) (SessionView, error) {
	return SessionView{}, nil
}
func (s *proposalTestStore) Timeline(context.Context, TimelineQuery) (TimelinePage, error) {
	return s.timeline, nil
}
func (*proposalTestStore) Routes(context.Context, CursorPageRequest) (RoutesPage, error) {
	return RoutesPage{}, nil
}
func (*proposalTestStore) Node(context.Context, string) (NodeView, error) { return NodeView{}, nil }
func (*proposalTestStore) EvidenceList(context.Context, EvidenceQuery) (EvidencePage, error) {
	return EvidencePage{}, nil
}
func (*proposalTestStore) Reviews(context.Context, ReviewQuery) (ReviewsPage, error) {
	return ReviewsPage{}, nil
}
func (*proposalTestStore) ProjectionStatus(context.Context) (ProjectionStatus, error) {
	return ProjectionStatus{}, nil
}
func (*proposalTestStore) Rebuild(context.Context) (ProjectionStatus, error) {
	return ProjectionStatus{}, nil
}
func (s *proposalTestStore) LoadSessionAuthority(context.Context, string) (SessionAuthority, error) {
	s.loads++
	return SessionAuthority{Session: s.session, AsOfEventSequence: s.authorityAsOf}, nil
}
func (s *proposalTestStore) LoadAggregateVersion(context.Context, string, string) (int64, error) {
	return s.aggregateVer, nil
}
func (s *proposalTestStore) LoadGoalRevision(context.Context, string) (GoalRevision, error) {
	return s.goal, nil
}
func (s *proposalTestStore) LoadRouteRevision(context.Context, string) (RouteRevision, error) {
	return s.route, nil
}
func (s *proposalTestStore) LoadActivity(context.Context, string) (Activity, error) {
	return s.activity, nil
}
func (s *proposalTestStore) LoadAttempt(context.Context, string) (Attempt, error) {
	return s.attempt, nil
}
func (s *proposalTestStore) LoadAssessment(context.Context, string) (AssessmentArtifact, AssessmentDecision, error) {
	s.loads++
	return s.assessment, s.decision, nil
}
func (s *proposalTestStore) LoadAssessmentForAttempt(context.Context, string) (AssessmentArtifact, AssessmentDecision, error) {
	s.loads++
	return s.assessment, s.decision, nil
}
func (s *proposalTestStore) LoadProposal(context.Context, string) (ProposalArtifact, error) {
	return s.proposal, nil
}
func (s *proposalTestStore) LoadFreeQuestion(context.Context, string) (tutoring.FreeQuestion, error) {
	return s.freeQuestion, nil
}
func (s *proposalTestStore) LoadFreeAnswer(context.Context, string) (tutoring.FreeAnswer, error) {
	return s.freeAnswer, nil
}
func (s *proposalTestStore) LoadValidEvidence(context.Context, string) ([]AcceptedEvidence, error) {
	return append([]AcceptedEvidence(nil), s.evidence...), nil
}
func (s *proposalTestStore) LoadMisconceptions(context.Context, string) ([]MisconceptionHypothesis, error) {
	return append([]MisconceptionHypothesis(nil), s.misconceptions...), nil
}
func (*proposalTestStore) LatestFreeQuestion(context.Context, string) (string, error) { return "", nil }
func (s *proposalTestStore) LatestFreeQuestionForFrame(context.Context, string, string) (string, error) {
	return s.freeQuestion.ID, nil
}

type proposalTestRepository struct {
	claim          ProposalClaim
	completed      *ProposalArtifact
	failedCategory string
	failedAttempts []string
}

func (r *proposalTestRepository) ClaimProposal(context.Context, string, ProposalRequest, string, time.Time) (ProposalClaim, error) {
	return r.claim, nil
}
func (r *proposalTestRepository) CompleteProposal(_ context.Context, _ string, _ string, value ProposalArtifact, _ time.Time) error {
	r.completed = &value
	return nil
}
func (r *proposalTestRepository) FailProposal(_ context.Context, _ string, _ string, attempts []string, category string, _ time.Time) error {
	r.failedAttempts = append([]string(nil), attempts...)
	r.failedCategory = category
	return nil
}

type proposalModelResult struct {
	raw json.RawMessage
	err error
}
type proposalTestModel struct {
	results []proposalModelResult
	calls   int
}

func (m *proposalTestModel) Generate(context.Context, ProposalRequest) (json.RawMessage, error) {
	index := m.calls
	m.calls++
	if index >= len(m.results) {
		return nil, errors.New("unexpected model call")
	}
	return m.results[index].raw, m.results[index].err
}

type proposalModelError string

func (e proposalModelError) Error() string         { return string(e) }
func (e proposalModelError) ModelCategory() string { return string(e) }

type proposalResolver struct{}

func (proposalResolver) Resolve(_ context.Context, knowledgeRevisionID, nodeRevisionID string) (KnowledgeReference, error) {
	return KnowledgeReference{KnowledgeRevisionID: knowledgeRevisionID, NodeID: "node", NodeRevisionID: nodeRevisionID, DocumentRevisionID: "document", Range: SourceRange{Start: 0, End: 5}, Slice: "topic", SliceSHA256: SHA256([]byte("topic"))}, nil
}

func newProposalTestService(t *testing.T, store *proposalTestStore, repo *proposalTestRepository, model TutorModel) *Service {
	t.Helper()
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	ids := []string{"10000000-0000-4000-8000-000000000010", "10000000-0000-4000-8000-000000000011"}
	index := 0
	service, err := NewService(store, repo, proposalResolver{}, ServiceOptions{Now: func() time.Time { return now }, NewUUID: func() string { value := ids[index%len(ids)]; index++; return value }, Model: model, ModelID: "strict-fake", ModelParameters: map[string]any{"temperature": 0}, PromptRevision: "test-prompt"})
	if err != nil {
		t.Fatal(err)
	}
	return service
}
func routeProposalRequest() ProposalRequest {
	return ProposalRequest{RequestID: "10000000-0000-4000-8000-000000000001", Type: ProposalRoute, AggregateType: "session", AggregateID: "10000000-0000-4000-8000-000000000002", AggregateVersion: 7, KnowledgeRevisionID: "10000000-0000-4000-8000-000000000003", NodeRevisionIDs: []string{"node-revision"}, Input: json.RawMessage(`{"goal":"fractions"}`)}
}

func routeProposalStore() *proposalTestStore {
	request := routeProposalRequest()
	return &proposalTestStore{
		session: tutoring.Session{ID: request.AggregateID, State: tutoring.StateDiagnostic, AggregateVer: request.AggregateVersion, Context: tutoring.FocusContext{GoalRevisionID: "goal-revision"}},
		goal:    GoalRevision{ID: "goal-revision", GoalID: "goal", Revision: 1},
	}
}

func TestProposalRetriesTransientOnceAndFreezesArtifactWithoutAggregateMutation(t *testing.T) {
	store := routeProposalStore()
	repo := &proposalTestRepository{claim: ProposalClaim{State: "claimed", LeaseToken: "lease"}}
	model := &proposalTestModel{results: []proposalModelResult{{err: proposalModelError("timeout")}, {raw: json.RawMessage(`{"route":[{"node_revision_id":"node-revision","teaching_intent":"explain","completion_condition":"pass"}]}`)}}}
	service := newProposalTestService(t, store, repo, model)
	request := routeProposalRequest()
	artifact, err := service.Propose(context.Background(), "device", request)
	if err != nil {
		t.Fatal(err)
	}
	if model.calls != 2 || store.commits != 0 || repo.completed == nil {
		t.Fatalf("calls=%d commits=%d completed=%v", model.calls, store.commits, repo.completed != nil)
	}
	expectedHash, _ := HashJSON(artifact.FrozenRequest)
	if artifact.InputHash != expectedHash || artifact.FrozenRequest.GoalRevisionID != "goal-revision" || artifact.FrozenRequest.TutoringState != string(tutoring.StateDiagnostic) || artifact.ModelID != "strict-fake" || artifact.PromptRevision != "test-prompt" {
		t.Fatalf("artifact metadata not frozen: %#v", artifact)
	}
	if len(artifact.AttemptCategories) != 2 || artifact.AttemptCategories[0] != "timeout" || artifact.AttemptCategories[1] != "success" {
		t.Fatalf("attempt categories=%v", artifact.AttemptCategories)
	}
}

func TestProposalStrictRecursiveDecodeFailsPermanently(t *testing.T) {
	store := routeProposalStore()
	repo := &proposalTestRepository{claim: ProposalClaim{State: "claimed", LeaseToken: "lease"}}
	model := &proposalTestModel{results: []proposalModelResult{{raw: json.RawMessage(`{"route":[{"node_revision_id":"node-revision","teaching_intent":"explain","completion_condition":"pass","invented_state":"mastered"}]}`)}}}
	service := newProposalTestService(t, store, repo, model)
	_, err := service.Propose(context.Background(), "device", routeProposalRequest())
	if ErrorCode(err) != CodeProposalRejected {
		t.Fatalf("expected proposal_rejected, got %v", err)
	}
	if model.calls != 1 || store.commits != 0 || repo.completed != nil || repo.failedCategory != "schema_mismatch" || len(repo.failedAttempts) != 1 || repo.failedAttempts[0] != "schema_mismatch" {
		t.Fatalf("calls=%d commits=%d completed=%v failed=%q", model.calls, store.commits, repo.completed != nil, repo.failedCategory)
	}
}

func TestA101FreeAnswerProposalChecksActiveFrameBeforeQuestionLookup(t *testing.T) {
	sessionID := "10000000-0000-4000-8000-000000000060"
	session := tutoring.Session{
		ID: sessionID, State: tutoring.StateFreeQuestion, AggregateVer: 7,
		Context: tutoring.FocusContext{GoalRevisionID: "goal", RouteRevisionID: "route", RouteStepID: "step", KnowledgeRevisionID: "knowledge", FocusNodeRevisionID: "node"},
	}
	store := &proposalTestStore{session: session}
	service := newProposalTestService(t, store, &proposalTestRepository{}, nil)
	request := frozenSessionRequest(session, ProposalFreeAnswer, "knowledge", []string{"node"})
	if _, err := service.freezeProposalRequest(context.Background(), request); ErrorCode(err) != CodeStaleProposal {
		t.Fatalf("missing active frame error=%v code=%q", err, ErrorCode(err))
	}
	if store.commits != 0 {
		t.Fatalf("missing active frame committed %d batches", store.commits)
	}
}

func TestA101FocusFrameAndQuestionVersionMatrixFailsClosed(t *testing.T) {
	frame := &tutoring.FocusFrame{
		ID: "frame", SessionID: "session", SavedState: tutoring.StateRouteActive,
		Context:               tutoring.FocusContext{GoalRevisionID: "goal", RouteRevisionID: "route", RouteStepID: "step", KnowledgeRevisionID: "knowledge", FocusNodeRevisionID: "node"},
		SavedAggregateVersion: 4, CreatedEventSequence: 9,
	}
	base := tutoring.Session{ID: "session", State: tutoring.StateFreeQuestion, AggregateVer: 7, Context: frame.Context, ActiveFrame: frame}
	question := tutoring.FreeQuestion{ID: "question", SessionID: base.ID, FocusFrameID: frame.ID, SessionAggregateVer: base.AggregateVer, KnowledgeRevisionID: "knowledge"}
	if !validActiveFocusFrame(base) || !currentFreeQuestionMatchesSession(base, question) {
		t.Fatal("valid frame/question fixture was rejected")
	}
	for _, test := range []struct {
		name   string
		mutate func(*tutoring.Session, *tutoring.FreeQuestion)
	}{
		{name: "goal mismatch", mutate: func(session *tutoring.Session, _ *tutoring.FreeQuestion) { session.Context.GoalRevisionID = "other" }},
		{name: "route mismatch", mutate: func(session *tutoring.Session, _ *tutoring.FreeQuestion) { session.Context.RouteRevisionID = "other" }},
		{name: "step mismatch", mutate: func(session *tutoring.Session, _ *tutoring.FreeQuestion) { session.Context.RouteStepID = "other" }},
		{name: "knowledge mismatch", mutate: func(session *tutoring.Session, _ *tutoring.FreeQuestion) {
			session.Context.KnowledgeRevisionID = "other"
		}},
		{name: "node mismatch", mutate: func(session *tutoring.Session, _ *tutoring.FreeQuestion) {
			session.Context.FocusNodeRevisionID = "other"
		}},
		{name: "invalid saved state", mutate: func(session *tutoring.Session, _ *tutoring.FreeQuestion) {
			session.ActiveFrame.SavedState = tutoring.StateFeedback
		}},
		{name: "question before frame", mutate: func(_ *tutoring.Session, question *tutoring.FreeQuestion) { question.SessionAggregateVer = 4 }},
		{name: "question future version", mutate: func(_ *tutoring.Session, question *tutoring.FreeQuestion) { question.SessionAggregateVer = 8 }},
		{name: "question frame mismatch", mutate: func(_ *tutoring.Session, question *tutoring.FreeQuestion) { question.FocusFrameID = "other" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			frameCopy := *frame
			session := base
			session.ActiveFrame = &frameCopy
			questionCopy := question
			test.mutate(&session, &questionCopy)
			if validActiveFocusFrame(session) && currentFreeQuestionMatchesSession(session, questionCopy) {
				t.Fatalf("damaged frame/question accepted: session=%+v question=%+v", session, questionCopy)
			}
		})
	}
}

func TestApplyRevalidatesProposalAggregateVersion(t *testing.T) {
	sessionID := "10000000-0000-4000-8000-000000000002"
	store := &proposalTestStore{session: tutoring.Session{ID: sessionID, State: tutoring.StateDiagnostic, AggregateVer: 8, Context: tutoring.FocusContext{GoalRevisionID: "goal"}}, proposal: ProposalArtifact{ID: "proposal", Type: ProposalRoute, AggregateType: "session", AggregateID: sessionID, AggregateVersion: 7}}
	service := newProposalTestService(t, store, &proposalTestRepository{}, nil)
	command := ActionCommand{Operation: OperationEnvelope{OperationID: "10000000-0000-4000-8000-000000000009", PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: sessionID, ExpectedVersion: 8, Payload: json.RawMessage(`{}`)}, Action: tutoring.ActionApplyRoute, ProposalID: "proposal"}
	_, err := service.ApplyAction(context.Background(), "10000000-0000-4000-8000-000000000099", sessionID, command)
	if ErrorCode(err) != CodeStaleProposal {
		t.Fatalf("expected stale proposal, got %v", err)
	}
	if store.commits != 0 || store.archives != 1 {
		t.Fatalf("stale apply authority commits=%d archives=%d", store.commits, store.archives)
	}
}
