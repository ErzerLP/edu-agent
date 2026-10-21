package mentorrun

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/google/uuid"
)

// The current run and job locks serialize more with stop, resume, selection
// and publication. The job version also rejects independently issued tab clicks.
func (s *Service) MoreSourceResearch(ctx context.Context, actor identity.Credential, space, id string, c SourceResearchMore) (SourceResearchJob, error) {
	result := SourceResearchJob{}
	if !validID(c.OperationID) || c.ExpectedVersion < 1 || utf8.RuneCountInString(c.AdditionalRequest) > 4000 || c.AdditionalRequest != "" && !validText(c.AdditionalRequest, 16000) || len(c.ConfirmDestination) > 256 {
		return result, ErrInvalid
	}
	if !s.CanSave() {
		return result, ErrStorage
	}
	tx, g, err := s.sourceResearchTx(ctx, actor, space, id, true)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(context.Background())
	// Acquire this before observing the current round: concurrent replays of
	// the same operation must see its receipt after the first creates a round.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "source-research-more:"+actor.Device.ID+":"+c.OperationID); err != nil {
		return result, err
	}
	item, version, _, _, _, err := s.sourceResearchRowTx(ctx, tx, actor, space, id, g, true)
	if err != nil {
		return result, err
	}
	hash := requestHash("source_research_more", space, id, c)
	if found, e := s.webOperation(ctx, tx, actor.Device.ID, space, c.OperationID, g, hash, &result); e != nil || found {
		return result, e
	}
	if version != c.ExpectedVersion || sourceResearchActive(sourceResearchStatus(item)) {
		return result, ErrConflict
	}
	if err = sourceActor(ctx, tx, actor.TokenID); err != nil {
		return result, err
	}
	view, configuration, search, err := s.sourceResearchConfigurationSnapshot()
	if err != nil {
		return result, err
	}
	prior := item.body.SourceResearch
	if (sourceResearchModelDestination(view) != prior.Destination || search != prior.SearchConfiguration) && c.ConfirmDestination != configuration+":"+search {
		return result, ErrInactive
	}
	if err = s.quota(ctx, tx, 512<<10); err != nil {
		return result, err
	}
	roundID, runID := uuid.NewString(), uuid.NewString()
	request := prior.Request
	if extra := strings.TrimSpace(c.AdditionalRequest); extra != "" {
		request += "\n补充需求：" + extra
	}
	state := &SourceResearchState{RoundNumber: prior.RoundNumber + 1, Request: request, SearchConfiguration: search, Destination: sourceResearchModelDestination(view), Limits: view.SourceResearch, Directions: []SourceResearchDirection{}, PreviousDirections: prior.Directions, PreviousGaps: prior.Gaps, ReadURLs: map[string]bool{}, ReadAliases: map[string]bool{}, Screened: map[string]bool{}, Gaps: []string{}, ReportRevisionID: prior.ReportRevisionID, SectionCount: prior.SectionCount, ReportedDocuments: prior.ReportedDocuments}
	// Persisted aliases include cancelled/excluded pages. They do not consume
	// the new round's reading slots or carry its unfinished queue forward.
	for key := range prior.ReadAliases {
		state.ReadAliases[key] = true
	}
	for key := range prior.ReadURLs {
		state.ReadAliases[key] = true
	}
	web := &WebState{ResourceKey: item.body.Web.ResourceKey, Input: request, Epoch: 1, Enabled: true, Routed: true, Supplied: item.body.Web.Supplied, Progress: WebProgress{Stage: "planning"}}
	item = row{Meta: Meta{RunID: runID, SessionID: item.SessionID, SpaceID: space, GoalID: uuid.Nil.String(), GoalVersion: 1, Generation: g, Kind: "source_research", SourceResearchJobID: id, SourceResearchRoundID: roundID, Status: "queued", Stage: "planning", Saved: true, BodyAvailable: true, ExpiresAt: item.ExpiresAt, Configuration: configuration}, device: actor.Device.ID, token: actor.TokenID, process: s.process, body: Body{SourceResearch: state, Web: web}}
	raw, _ := json.Marshal(item.Meta)
	if _, err = tx.Exec(ctx, `INSERT INTO learning_mentor_runs(id,session_id,device_id,token_id,space_id,goal_id,goal_version,privacy_generation,state,process_id,expires_at,source_research_job_id) VALUES($1,$2,$3,$4,$5,$6,1,$7,$8,$9,$10,$11)`, runID, item.SessionID, item.device, item.token, space, item.GoalID, g, raw, s.process, item.ExpiresAt, id); err != nil {
		return result, err
	}
	limits, _ := json.Marshal(state.Limits)
	if _, err = tx.Exec(ctx, `INSERT INTO learning_source_research_rounds(id,job_id,privacy_generation,number,run_id,status,stage,limits) VALUES($1,$2,$3,$4,$5,'queued','planning',$6)`, roundID, id, g, state.RoundNumber, runID, limits); err != nil {
		return result, err
	}
	if err = s.forkSourceResearchReportTx(ctx, tx, &item); err != nil {
		return result, err
	}
	if _, err = tx.Exec(ctx, `UPDATE learning_source_research_jobs SET current_round_id=$2,version=version+1 WHERE id=$1`, id, roundID); err != nil {
		return result, err
	}
	if _, err = tx.Exec(ctx, `UPDATE learning_mentor_sessions SET current_run_id=$2 WHERE id=$1`, item.SessionID, runID); err != nil {
		return result, err
	}
	if err = s.touchSourceResearchTx(ctx, tx, &item); err != nil {
		return result, err
	}
	if err = s.save(ctx, tx, &item, "source_research_more"); err != nil {
		return result, err
	}
	result, err = s.sourceResearchJobTx(ctx, tx, actor, space, id, g, false)
	if err != nil {
		return result, err
	}
	if err = s.saveWebOperation(ctx, tx, actor.Device.ID, space, "", runID, c.OperationID, g, hash, result); err != nil {
		return result, err
	}
	return result, tx.Commit(ctx)
}

// Report headers are paginated separately from immutable section bodies.
func (s *Service) SourceResearchReports(ctx context.Context, actor identity.Credential, space, id string, before int64) ([]SourceResearchReport, int64, error) {
	if before < 0 {
		return nil, 0, ErrInvalid
	}
	tx, g, err := s.sourceResearchTx(ctx, actor, space, id, false)
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback(context.Background())
	if _, _, _, _, _, err = s.sourceResearchRowTx(ctx, tx, actor, space, id, g, false); err != nil {
		return nil, 0, err
	}
	rows, err := tx.Query(ctx, `SELECT id::text,round_id::text,version,status,ciphertext FROM learning_source_research_reports WHERE job_id=$1 AND ($2::bigint=0 OR version<$2) ORDER BY version DESC LIMIT 21`, id, before)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []SourceResearchReport{}
	for rows.Next() {
		var rid, round, status string
		var version int64
		var raw []byte
		var report SourceResearchReport
		if err = rows.Scan(&rid, &round, &version, &status, &raw); err != nil {
			return nil, 0, err
		}
		if err = s.openWeb("source_research_reports", rid, g, raw, &report); err != nil {
			return nil, 0, err
		}
		report.RoundID = round
		report.Version = version
		report.Status = status
		items = append(items, report)
	}
	var next int64
	if len(items) > 20 {
		items = items[:20]
		next = items[19].Version
	}
	return items, next, rows.Err()
}
