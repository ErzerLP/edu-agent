package modelusage

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type Query struct {
	Source, ActorDeviceID, CLISessionID     string
	From, To                                time.Time
	Timezone, Grain, Model, SpaceID, Cursor string
	Limit                                   int
}

func (q Query) Validate() error {
	if q.From.IsZero() || q.To.IsZero() || !q.From.Before(q.To) || q.Limit < 1 || q.Limit > 100 || len(q.Model) > 200 || len(q.Cursor) > 2048 {
		return ErrInvalid
	}
	if q.Grain != "day" && q.Grain != "week" && q.Grain != "month" {
		return ErrInvalid
	}
	if _, err := time.LoadLocation(q.Timezone); err != nil || q.Timezone == "" || q.Timezone == "Local" {
		return ErrInvalid
	}
	if q.SpaceID != "" {
		if id, err := uuid.Parse(q.SpaceID); err != nil || id.String() != q.SpaceID || id == uuid.Nil {
			return ErrInvalid
		}
	}
	if q.Source != "" && q.Source != "web" && q.Source != "cli" && q.Source != "server" {
		return ErrInvalid
	}
	if q.CLISessionID != "" && q.ActorDeviceID == "" {
		return ErrInvalid
	}
	for _, value := range []string{q.ActorDeviceID, q.CLISessionID} {
		if value != "" {
			id, err := uuid.Parse(value)
			if err != nil || id == uuid.Nil || id.String() != value {
				return ErrInvalid
			}
		}
	}
	return nil
}
func (s *Store) read(ctx context.Context, actor identity.Credential) (pgx.Tx, int64, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return nil, 0, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = tx.Rollback(context.Background())
		}
	}()
	var generation int64
	if err = tx.QueryRow(ctx, `SELECT privacy_lock_owner_gate('identity','read',NULL)`).Scan(&generation); err != nil {
		return nil, 0, err
	}
	if err = tx.QueryRow(ctx, `SELECT privacy_lock_owner_gate('learning','read',NULL)`).Scan(&generation); err != nil {
		return nil, 0, err
	}
	var active bool
	err = tx.QueryRow(ctx, `SELECT d.revoked_at IS NULL AND t.revoked_at IS NULL AND 'learning:read'=ANY(t.scopes) FROM devices d JOIN device_tokens t ON t.device_id=d.id WHERE d.id=$1 AND t.id=$2 FOR SHARE OF d,t`, actor.Device.ID, actor.TokenID).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !active {
		return nil, 0, identity.ErrForbidden
	}
	if err != nil {
		return nil, 0, err
	}
	// In the current single-learner ACL, learning:read grants all existing spaces,
	// including archived spaces. No device ownership filter is an ACL substitute.
	ok = true
	return tx, generation, nil
}

const selected = ` FROM learning_model_calls c JOIN learning_model_tasks t ON t.id=c.task_id AND t.privacy_generation=c.privacy_generation
 LEFT JOIN learning_cli_model_turns cli ON cli.task_id=t.id AND cli.privacy_generation=t.privacy_generation
 WHERE c.privacy_generation=$1 AND ($2='' OR t.space_id::text=$2)
 AND (t.space_id IS NULL OR EXISTS(SELECT 1 FROM learning_spaces sp WHERE sp.id=t.space_id))
 AND c.started_at >= $3 AND c.started_at < $4 AND ($5='' OR c.model=$5)
 AND ($6='' OR t.source=$6) AND ($7='' OR t.actor_device_id::text=$7) AND ($8='' OR cli.session_id::text=$8) `
const sums = `count(*),count(*) FILTER(WHERE c.dispatch_state='dispatching'),count(*) FILTER(WHERE c.dispatch_state='outcome_unknown'),
 COALESCE(sum(c.input_tokens),0)::bigint,count(*) FILTER(WHERE c.input_tokens IS NULL),
 COALESCE(sum(c.output_tokens),0)::bigint,count(*) FILTER(WHERE c.output_tokens IS NULL),
 COALESCE(sum(c.reasoning_tokens),0)::bigint,count(*) FILTER(WHERE c.reasoning_tokens IS NULL),
 COALESCE(sum(c.total_tokens),0)::bigint,count(*) FILTER(WHERE c.total_tokens IS NULL)`

func targets(t *Totals) []any {
	return []any{&t.Calls, &t.PendingCalls, &t.OutcomeUnknownCalls, &t.Input.KnownSum, &t.Input.UnknownCalls, &t.Output.KnownSum, &t.Output.UnknownCalls, &t.Reasoning.KnownSum, &t.Reasoning.UnknownCalls, &t.Total.KnownSum, &t.Total.UnknownCalls}
}
func args(g int64, q Query) []any {
	return []any{g, q.SpaceID, q.From, q.To, q.Model, q.Source, q.ActorDeviceID, q.CLISessionID}
}

func (s *Store) Summary(ctx context.Context, actor identity.Credential, q Query) (Summary, error) {
	result := Summary{Coverage: "mentor", From: q.From, To: q.To, Timezone: q.Timezone, Grain: q.Grain, Buckets: []Bucket{}, Models: []Model{}}
	if err := q.Validate(); err != nil {
		return result, err
	}
	tx, g, err := s.read(ctx, actor)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(context.Background())
	result.Generation = g
	if err = tx.QueryRow(ctx, `SELECT started_at,full_started_at FROM model_usage_coverage`).Scan(&result.CoverageSince, &result.FullCoverageSince); err != nil {
		return result, err
	}
	if result.FullCoverageSince != nil {
		result.Coverage = "server"
	}
	if err = tx.QueryRow(ctx, `SELECT `+sums+selected, args(g, q)...).Scan(targets(&result.Totals)...); err != nil {
		return result, err
	}
	rows, err := tx.Query(ctx, `SELECT date_trunc($9,c.started_at AT TIME ZONE $10) AT TIME ZONE $10 AS bucket,`+sums+selected+` GROUP BY bucket ORDER BY bucket`, append(args(g, q), q.Grain, q.Timezone)...)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var b Bucket
		if err = rows.Scan(append([]any{&b.Start}, targets(&b.Totals)...)...); err != nil {
			rows.Close()
			return result, err
		}
		result.Buckets = append(result.Buckets, b)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return result, err
	}
	modelsQuery := q
	modelsQuery.Model = ""
	rows, err = tx.Query(ctx, `SELECT DISTINCT c.provider,c.model,c.api_style,c.destination`+selected+` ORDER BY c.model,c.provider,c.api_style,c.destination`, args(g, modelsQuery)...)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var m Model
		if err = rows.Scan(&m.Provider, &m.Model, &m.APIStyle, &m.Destination); err != nil {
			return result, err
		}
		result.Models = append(result.Models, m)
	}
	return result, rows.Err()
}

type position struct {
	At          time.Time
	ID, Binding string
}

func binding(actor identity.Credential, g int64, q Query, task string) string {
	q.Cursor = ""
	q.Limit = 0
	raw, _ := json.Marshal(struct {
		Actor      string
		Generation int64
		Query      Query
		Task       string
	}{actor.TokenID, g, q, task})
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}
func readPosition(q Query, bind string) (position, error) {
	p := position{At: time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC), ID: "ffffffff-ffff-ffff-ffff-ffffffffffff", Binding: bind}
	if q.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(q.Cursor)
		if err != nil || json.Unmarshal(raw, &p) != nil || p.Binding != bind || p.At.IsZero() {
			return p, ErrInvalid
		}
		if _, err = uuid.Parse(p.ID); err != nil {
			return p, ErrInvalid
		}
	}
	return p, nil
}
func nextPosition(at time.Time, id, bind string) string {
	raw, _ := json.Marshal(position{At: at, ID: id, Binding: bind})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func (s *Store) Tasks(ctx context.Context, actor identity.Credential, q Query) (Page[Task], error) {
	p := Page[Task]{Items: []Task{}}
	if err := q.Validate(); err != nil {
		return p, err
	}
	tx, g, err := s.read(ctx, actor)
	if err != nil {
		return p, err
	}
	defer tx.Rollback(context.Background())
	p.Generation = g
	bind := binding(actor, g, q, "")
	after, err := readPosition(q, bind)
	if err != nil {
		return p, err
	}
	rows, err := tx.Query(ctx, `SELECT t.id::text,t.space_id::text,t.kind,t.source,min(c.started_at),
	 CASE WHEN EXISTS(SELECT 1 FROM learning_mentor_runs r WHERE r.id=t.origin_id AND r.device_id=$12 AND r.privacy_generation=$1 AND (r.state->>'body_available')::boolean AND r.expires_at>clock_timestamp()) THEN t.origin_id::text ELSE NULL END,
	 CASE WHEN t.kind='topic_index' AND EXISTS(SELECT 1 FROM learning_tutor_conversations c WHERE c.id=t.origin_id AND c.device_id=$12 AND c.privacy_generation=$1 AND NOT c.deleted) THEN t.origin_id::text ELSE NULL END,
	 t.actor_device_id::text,cli.session_id::text,cli.turn_id::text,`+sums+selected+`
	 GROUP BY t.id,cli.session_id,cli.turn_id HAVING (min(c.started_at),t.id)<($9,$10::uuid) ORDER BY min(c.started_at) DESC,t.id DESC LIMIT $11`, append(args(g, q), after.At, after.ID, q.Limit+1, actor.Device.ID)...)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var item Task
		if err = rows.Scan(append([]any{&item.ID, &item.SpaceID, &item.Kind, &item.Source, &item.StartedAt, &item.RunID, &item.ConversationID, &item.ActorDeviceID, &item.CLISessionID, &item.CLITurnID}, targets(&item.Totals)...)...); err != nil {
			return p, err
		}
		p.Items = append(p.Items, item)
	}
	if err = rows.Err(); err != nil {
		return p, err
	}
	if len(p.Items) > q.Limit {
		p.Items = p.Items[:q.Limit]
		last := p.Items[len(p.Items)-1]
		p.NextCursor = nextPosition(last.StartedAt, last.ID, bind)
	}
	return p, nil
}

func (s *Store) Calls(ctx context.Context, actor identity.Credential, q Query, task string) (Page[Call], error) {
	p := Page[Call]{Items: []Call{}}
	if err := q.Validate(); err != nil {
		return p, err
	}
	if _, err := uuid.Parse(task); err != nil {
		return p, ErrInvalid
	}
	tx, g, err := s.read(ctx, actor)
	if err != nil {
		return p, err
	}
	defer tx.Rollback(context.Background())
	p.Generation = g
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM learning_model_tasks t WHERE id=$1 AND privacy_generation=$2 AND ($3='' OR space_id::text=$3) AND ($4='' OR source=$4) AND ($5='' OR actor_device_id::text=$5) AND ($6='' OR EXISTS(SELECT 1 FROM learning_cli_model_turns cli WHERE cli.task_id=t.id AND cli.session_id::text=$6)))`, task, g, q.SpaceID, q.Source, q.ActorDeviceID, q.CLISessionID).Scan(&exists); err != nil {
		return p, err
	}
	if !exists {
		return p, ErrNotFound
	}
	bind := binding(actor, g, q, task)
	after, err := readPosition(q, bind)
	if err != nil {
		return p, err
	}
	rows, err := tx.Query(ctx, `SELECT c.id::text,c.stage,c.attempt_no,c.dispatch_state,c.started_at,c.finished_at,c.provider,c.model,c.api_style,c.destination,c.input_tokens,c.output_tokens,c.reasoning_tokens,c.provider_total_tokens,c.usage_inconsistent,c.total_tokens`+selected+`
	 AND c.task_id=$9 AND (c.started_at,c.id)<($10,$11::uuid) ORDER BY c.started_at DESC,c.id DESC LIMIT $12`, append(args(g, q), task, after.At, after.ID, q.Limit+1)...)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var c Call
		if err = rows.Scan(&c.ID, &c.Stage, &c.AttemptNo, &c.State, &c.StartedAt, &c.FinishedAt, &c.Provider, &c.Model.Model, &c.APIStyle, &c.Destination, &c.Input, &c.Output, &c.Reasoning, &c.ProviderTotal, &c.Inconsistent, &c.Total); err != nil {
			return p, err
		}
		p.Items = append(p.Items, c)
	}
	if err = rows.Err(); err != nil {
		return p, err
	}
	if len(p.Items) > q.Limit {
		p.Items = p.Items[:q.Limit]
		last := p.Items[len(p.Items)-1]
		p.NextCursor = nextPosition(last.StartedAt, last.ID, bind)
	}
	return p, nil
}
