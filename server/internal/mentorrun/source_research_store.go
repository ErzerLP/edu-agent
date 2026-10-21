package mentorrun

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

func (s *Service) CreateSourceResearch(ctx context.Context, actor identity.Credential, space string, c SourceResearchCreate) (SourceResearchJob, error) {
	result := SourceResearchJob{}
	if !validID(space) || !validID(c.OperationID) || !validText(c.Request, 16000) || utf8.RuneCountInString(c.Request) > 4000 {
		return result, ErrInvalid
	}
	if !s.CanSave() {
		return result, ErrStorage
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(context.Background())
	g, err := gates(ctx, tx, true)
	if err != nil {
		return result, err
	}
	if err = actorGate(ctx, tx, actor.Device.ID, actor.TokenID, true); err != nil {
		return result, err
	}
	if err = sourceActor(ctx, tx, actor.TokenID); err != nil {
		return result, err
	}
	if _, err = goalGate(ctx, tx, space, uuid.Nil.String(), 1); err != nil {
		return result, err
	}
	hash := requestHash("source_research_create", space, "", c)
	old, found, err := operation(ctx, tx, actor.Device.ID, c.OperationID, hash)
	if err != nil {
		return result, err
	}
	if found {
		var id string
		if err = tx.QueryRow(ctx, `SELECT source_research_job_id::text FROM learning_mentor_runs WHERE id=$1`, old.RunID).Scan(&id); err != nil {
			return result, err
		}
		return s.sourceResearchJobTx(ctx, tx, actor, space, id, g, false)
	}
	view, configuration, searchConfig, err := s.sourceResearchConfigurationSnapshot()
	if err != nil {
		return result, err
	}
	if err = s.quota(ctx, tx, 512<<10); err != nil {
		return result, err
	}
	jobID, roundID, runID, sessionID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	expires := time.Now().UTC().Add(7 * 24 * time.Hour)
	requestRaw, err := s.sealWeb("source_research_jobs", jobID, g, c.Request)
	if err != nil {
		return result, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO learning_source_research_jobs(id,space_id,device_id,privacy_generation,current_round_id,ciphertext,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, jobID, space, actor.Device.ID, g, roundID, requestRaw, expires); err != nil {
		return result, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO learning_mentor_sessions(id,device_id,space_id,goal_id,privacy_generation,kind,history) VALUES($1,$2,$3,$4,$5,'source_research',TRUE)`, sessionID, actor.Device.ID, space, uuid.Nil.String(), g); err != nil {
		return result, err
	}
	key := make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		return result, err
	}
	scale := view.SourceResearch
	item := row{Meta: Meta{RunID: runID, SessionID: sessionID, SpaceID: space, GoalID: uuid.Nil.String(), GoalVersion: 1, Generation: g, Kind: "source_research", SourceResearchJobID: jobID, SourceResearchRoundID: roundID, Status: "queued", Stage: "planning", Saved: true, BodyAvailable: true, ExpiresAt: expires, Configuration: configuration}, device: actor.Device.ID, token: actor.TokenID, process: s.process}
	item.body.SourceResearch = &SourceResearchState{RoundNumber: 1, Request: strings.TrimSpace(c.Request), SearchConfiguration: searchConfig, Destination: sourceResearchModelDestination(view), Limits: scale, Directions: []SourceResearchDirection{}, ReadURLs: map[string]bool{}, Screened: map[string]bool{}, Gaps: []string{}}
	item.body.Web = &WebState{ResourceKey: key, Input: c.Request, Epoch: 1, Enabled: true, Routed: true, Supplied: map[string][]string{}, Progress: WebProgress{Stage: "planning"}}
	raw, _ := json.Marshal(item.Meta)
	if _, err = tx.Exec(ctx, `INSERT INTO learning_mentor_runs(id,session_id,device_id,token_id,space_id,goal_id,goal_version,privacy_generation,state,process_id,expires_at,source_research_job_id) VALUES($1,$2,$3,$4,$5,$6,1,$7,$8,$9,$10,$11)`, runID, sessionID, item.device, item.token, space, item.GoalID, g, raw, s.process, expires, jobID); err != nil {
		return result, err
	}
	limits, _ := json.Marshal(scale)
	if _, err = tx.Exec(ctx, `INSERT INTO learning_source_research_rounds(id,job_id,privacy_generation,number,run_id,status,stage,limits) VALUES($1,$2,$3,1,$4,'queued','planning',$5)`, roundID, jobID, g, runID, limits); err != nil {
		return result, err
	}
	if err = s.save(ctx, tx, &item, "accepted"); err != nil {
		return result, err
	}
	if _, err = receipt(ctx, tx, item, c.OperationID, hash); err != nil {
		return result, err
	}
	if _, err = tx.Exec(ctx, `UPDATE learning_mentor_sessions SET current_run_id=$2 WHERE id=$1`, sessionID, runID); err != nil {
		return result, err
	}
	result, err = s.sourceResearchJobTx(ctx, tx, actor, space, jobID, g, false)
	if err != nil {
		return result, err
	}
	return result, tx.Commit(ctx)
}

func (s *Service) sourceResearchRowTx(ctx context.Context, tx pgx.Tx, actor identity.Credential, space, id string, g int64, write bool) (row, int64, int64, int64, string, error) {
	var run, selected, current string
	var version, selection, seq int64
	err := tx.QueryRow(ctx, `SELECT r.run_id::text,j.version,j.selection_version,j.event_seq,COALESCE(j.selected_report_revision_id::text,'') FROM learning_source_research_jobs j JOIN learning_source_research_rounds r ON r.id=j.current_round_id WHERE j.id=$1 AND j.space_id=$2 AND j.device_id=$3 AND j.privacy_generation=$4 AND NOT j.deleted AND j.expires_at>clock_timestamp()`, id, space, actor.Device.ID, g).Scan(&run, &version, &selection, &seq, &selected)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	if err != nil {
		return row{}, 0, 0, 0, "", err
	}
	if _, err = goalGate(ctx, tx, space, uuid.Nil.String(), 1); err != nil {
		return row{}, 0, 0, 0, "", err
	}
	runLock := "FOR SHARE"
	if write {
		runLock = "FOR UPDATE"
	}
	item, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM learning_mentor_runs WHERE id=$1 `+runLock, run))
	if err == nil {
		err = tx.QueryRow(ctx, `SELECT version,selection_version,event_seq,COALESCE(selected_report_revision_id::text,''),current_round_id::text FROM learning_source_research_jobs WHERE id=$1 AND NOT deleted AND expires_at>clock_timestamp() `+runLock, id).Scan(&version, &selection, &seq, &selected, &current)
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrNotFound
		}
	}
	if err == nil && current != item.SourceResearchRoundID {
		err = ErrConflict
	}
	if err == nil {
		err = s.decode(&item)
	}
	return item, version, selection, seq, selected, err
}
func (s *Service) sourceResearchJobTx(ctx context.Context, tx pgx.Tx, actor identity.Credential, space, id string, g int64, write bool) (SourceResearchJob, error) {
	item, v, selection, seq, selected, err := s.sourceResearchRowTx(ctx, tx, actor, space, id, g, write)
	if err != nil {
		return SourceResearchJob{}, err
	}
	state := item.body.SourceResearch
	if state == nil || !item.BodyAvailable {
		return SourceResearchJob{}, ErrNotFound
	}
	result := SourceResearchJob{RoundNumber: state.RoundNumber, ID: id, RoundID: item.SourceResearchRoundID, RunID: item.RunID, Version: v, SelectionVersion: selection, EventSeq: seq, Status: sourceResearchStatus(item), Stage: item.body.Web.Progress.Stage, Request: state.Request, Limits: state.Limits, Directions: state.Directions, Progress: item.body.Web.Progress, CandidateCount: state.CandidateCount, ResultSnapshotID: state.ResultSnapshotID, ReportRevisionID: state.ReportRevisionID, SelectedReportRevisionID: selected, ExpiresAt: item.ExpiresAt, AvailableActions: []string{"delete"}}
	if err = tx.QueryRow(ctx, `SELECT fetch_http_requests FROM learning_source_research_rounds WHERE id=$1`, item.SourceResearchRoundID).Scan(&result.Progress.FetchHTTPRequests); err != nil {
		return result, err
	}
	if sourceResearchActive(result.Status) {
		result.AvailableActions = append(result.AvailableActions, "stop")
	} else {
		if state.ResultSnapshotID != "" {
			result.AvailableActions = append(result.AvailableActions, "import")
		}
		result.AvailableActions = append(result.AvailableActions, "more")
		if slices.Contains([]string{"stopped", "failed", "outcome_unknown", "paused_tool_steps"}, result.Status) {
			result.AvailableActions = append(result.AvailableActions, "resume")
		}
		v, modelConfig, searchConfig, configErr := s.sourceResearchConfigurationSnapshot()
		if configErr == nil && (sourceResearchModelDestination(v) != state.Destination || searchConfig != state.SearchConfiguration) {
			result.DestinationChange = &SourceResearchDestinationChange{Model: sourceResearchModelDestination(v), Search: v.Search.Connection, Confirmation: modelConfig + ":" + searchConfig}
		}
	}
	_ = tx.QueryRow(ctx, `SELECT id::text FROM learning_model_tasks WHERE kind='source_research' AND origin_id=$1 AND privacy_generation=$2`, id, g).Scan(&result.UsageTaskID)
	_ = tx.QueryRow(ctx, `SELECT id::text FROM learning_source_research_previews WHERE job_id=$1 ORDER BY expires_at DESC,id DESC LIMIT 1`, id).Scan(&result.ImportPreviewID)
	if state.ResultSnapshotID != "" {
		snapshot, e := s.sourceResearchResultTx(ctx, tx, item, state.ResultSnapshotID)
		if e != nil {
			return result, e
		}
		result.CandidateCount = snapshot.NewCandidateCount
	}
	return result, nil
}

// Settings are mutable independently of the database transaction. Revision
// equality freezes a coherent model/search/scale tuple without holding a lock
// during network work or silently accepting a different destination.
func (s *Service) sourceResearchConfigurationSnapshot() (settings.View, string, string, error) {
	view := s.settings.View()
	_, _, model, err := s.settings.MentorClient()
	if err != nil {
		return view, "", "", ErrModel
	}
	_, search, err := s.search(nil)
	if err != nil {
		return view, "", "", ErrModel
	}
	if s.settings.View().Revision != view.Revision {
		return view, "", "", ErrConflict
	}
	return view, model, search, nil
}
func (s *Service) sourceResearchTx(ctx context.Context, actor identity.Credential, space, id string, write bool) (pgx.Tx, int64, error) {
	if !validID(space) || !validID(id) {
		return nil, 0, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, 0, err
	}
	g, err := gates(ctx, tx, write)
	if err == nil {
		err = actorGate(ctx, tx, actor.Device.ID, actor.TokenID, write)
	}
	if err != nil {
		tx.Rollback(context.Background())
		return nil, 0, err
	}
	return tx, g, nil
}
func (s *Service) SourceResearchJob(ctx context.Context, actor identity.Credential, space, id string) (SourceResearchJob, error) {
	tx, g, err := s.sourceResearchTx(ctx, actor, space, id, false)
	if err != nil {
		return SourceResearchJob{}, err
	}
	defer tx.Rollback(context.Background())
	return s.sourceResearchJobTx(ctx, tx, actor, space, id, g, false)
}

// Called from the existing run commit, keeping events/results and state atomic.
func (s *Service) saveSourceResearchTx(ctx context.Context, tx pgx.Tx, item *row, kind string) error {
	if item.SourceResearchJobID == "" {
		return nil
	}
	state := item.body.SourceResearch
	if !item.BodyAvailable {
		return s.scrubSourceResearchTx(ctx, tx, item.SourceResearchJobID)
	}
	if state == nil {
		return ErrStorage
	}
	status := sourceResearchStatus(*item)
	if !sourceResearchActive(status) && state.ResultSnapshotID == "" {
		if err := s.freezeSourceResearchTx(ctx, tx, item); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE learning_source_research_rounds SET status=$2,stage=$3 WHERE id=$1`, item.SourceResearchRoundID, status, item.body.Web.Progress.Stage); err != nil {
		return err
	}
	event := SourceResearchEvent{Type: kind, Status: status, Stage: item.body.Web.Progress.Stage, Progress: item.body.Web.Progress, CandidateCount: state.CandidateCount, CreatedAt: time.Now().UTC()}
	// Events carry just the current observation, not every prior query/failure.
	event.Progress.Queries = nil
	event.Progress.Failures = nil
	if err := tx.QueryRow(ctx, `UPDATE learning_source_research_jobs SET event_seq=event_seq+1 WHERE id=$1 RETURNING event_seq`, item.SourceResearchJobID).Scan(&event.Seq); err != nil {
		return err
	}
	id := uuid.NewString()
	raw, err := s.sealWeb("source_research_events", id, item.Generation, event)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO learning_source_research_events(id,job_id,privacy_generation,seq,ciphertext) VALUES($1,$2,$3,$4,$5)`, id, item.SourceResearchJobID, item.Generation, event.Seq, raw); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `DELETE FROM learning_source_research_events WHERE job_id=$1 AND seq<=$2`, item.SourceResearchJobID, event.Seq-512)
	return err
}
func (s *Service) sourceResearchGateTx(ctx context.Context, tx pgx.Tx, item row, epoch int64) error {
	var allowed bool
	err := tx.QueryRow(ctx, `SELECT NOT j.deleted AND j.expires_at>clock_timestamp() AND r.execution_epoch=$3 AND r.id=$4 AND r.status='running' FROM learning_source_research_jobs j JOIN learning_source_research_rounds r ON r.id=j.current_round_id WHERE j.id=$1 AND j.privacy_generation=$2 FOR SHARE OF j,r`, item.SourceResearchJobID, item.Generation, epoch, item.SourceResearchRoundID).Scan(&allowed)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !allowed {
		return ErrLease
	}
	return err
}
func (s *Service) sourceResearchConfiguration(item row) bool {
	if item.SourceResearchJobID == "" {
		return false
	}
	v := s.settings.View()
	stored := item.body.SourceResearch
	return stored != nil && v.EffectiveMentor.Enabled && v.EffectiveMentor.Configured && sourceResearchModelDestination(v) == stored.Destination
}
