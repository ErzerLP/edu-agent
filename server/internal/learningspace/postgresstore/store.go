package postgresstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	space "github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const columns = "id::text,name,description,status,version,created_at,updated_at"

func scan(row pgx.Row) (space.Space, error) {
	var s space.Space
	err := row.Scan(&s.ID, &s.Name, &s.Description, &s.Status, &s.Version, &s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = &space.Error{Code: "learning_space_not_found"}
	}
	return s, err
}
func (s *Store) read(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err = privacy.LockOwnerRead(ctx, tx, privacy.OwnerLearning); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}
func (s *Store) Get(ctx context.Context, id string) (space.Space, error) {
	if !space.ValidID(id) {
		return space.Space{}, space.Invalid()
	}
	tx, err := s.read(ctx)
	if err != nil {
		return space.Space{}, err
	}
	defer tx.Rollback(context.Background())
	return scan(tx.QueryRow(ctx, "SELECT "+columns+" FROM learning_spaces WHERE id=$1", id))
}

type cursor struct{ ID, Search, Status string }

func (s *Store) List(ctx context.Context, q space.Query) (space.Page, error) {
	if q.Limit < 1 || q.Limit > 100 || utf8.RuneCountInString(q.Search) > 120 || (q.Status != "" && q.Status != "active" && q.Status != "archived") {
		return space.Page{}, space.Invalid()
	}
	after := "00000000-0000-0000-0000-000000000000"
	if q.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(q.Cursor)
		var c cursor
		if len(q.Cursor) > 2048 || err != nil || json.Unmarshal(raw, &c) != nil || !space.ValidID(c.ID) || c.Search != q.Search || c.Status != q.Status {
			return space.Page{}, space.Invalid()
		}
		after = c.ID
	}
	tx, err := s.read(ctx)
	if err != nil {
		return space.Page{}, err
	}
	defer tx.Rollback(context.Background())
	// strpos performs literal substring search (SQL wildcard characters are data).
	rows, err := tx.Query(ctx, "SELECT "+columns+" FROM learning_spaces WHERE id>$1 AND ($2='' OR status=$2) AND strpos(lower(name||' '||description),lower($3))>0 ORDER BY id LIMIT $4", after, q.Status, q.Search, q.Limit+1)
	if err != nil {
		return space.Page{}, err
	}
	defer rows.Close()
	page := space.Page{Items: []space.Space{}}
	for rows.Next() {
		item, err := scan(rows)
		if err != nil {
			return space.Page{}, err
		}
		page.Items = append(page.Items, item)
	}
	if err = rows.Err(); err != nil {
		return space.Page{}, err
	}
	if len(page.Items) > q.Limit {
		page.Items = page.Items[:q.Limit]
		raw, _ := json.Marshal(cursor{page.Items[q.Limit-1].ID, q.Search, q.Status})
		page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return page, nil
}
func (s *Store) Mutate(ctx context.Context, actor, id string, c space.Command) (space.Space, error) {
	create := id == ""
	if !space.ValidID(actor) || (!create && !space.ValidID(id)) {
		return space.Space{}, space.Invalid()
	}
	if err := c.Validate(create); err != nil {
		return space.Space{}, err
	}
	raw, _ := json.Marshal(struct {
		ID      string
		Command space.Command
	}{id, c})
	hash := sha256.Sum256(raw)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return space.Space{}, err
	}
	defer tx.Rollback(context.Background())
	var generation int64
	if err = tx.QueryRow(ctx, `SELECT privacy_lock_owner_gate('learning','write',NULL)`).Scan(&generation); err != nil {
		return space.Space{}, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "learning-space:"+actor+":"+c.OperationID); err != nil {
		return space.Space{}, err
	}
	var oldHash, result []byte
	err = tx.QueryRow(ctx, `SELECT request_hash,result FROM learning_space_operations WHERE device_id=$1 AND operation_id=$2`, actor, c.OperationID).Scan(&oldHash, &result)
	if err == nil {
		var redaction struct {
			Redacted bool `json:"redacted"`
		}
		if json.Unmarshal(result, &redaction) == nil && redaction.Redacted {
			return space.Space{}, &privacy.Error{Code: privacy.CodeContentRedacted}
		}
		if !bytes.Equal(oldHash, hash[:]) {
			return space.Space{}, &space.Error{Code: "idempotency_conflict"}
		}
		var item space.Space
		if err = json.Unmarshal(result, &item); err != nil {
			return item, err
		}
		return item, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return space.Space{}, err
	}
	var item space.Space
	if create {
		id = uuid.NewString()
		item, err = scan(tx.QueryRow(ctx, "INSERT INTO learning_spaces(id,name,description,status,version) VALUES($1,$2,$3,'active',1) RETURNING "+columns, id, strings.TrimSpace(c.Name), c.Description))
	} else {
		current, e := scan(tx.QueryRow(ctx, "SELECT "+columns+" FROM learning_spaces WHERE id=$1 FOR UPDATE", id))
		if e != nil {
			return item, e
		}
		if current.Version != c.ExpectedVersion {
			return item, &space.Error{Code: "version_conflict"}
		}
		item, err = scan(tx.QueryRow(ctx, "UPDATE learning_spaces SET name=$2,description=$3,status=$4,version=version+1,updated_at=clock_timestamp() WHERE id=$1 RETURNING "+columns, id, strings.TrimSpace(c.Name), c.Description, c.Status))
	}
	if err != nil {
		return item, err
	}
	result, _ = json.Marshal(item)
	if _, err = tx.Exec(ctx, `INSERT INTO learning_space_operations(device_id,operation_id,request_hash,result) VALUES($1,$2,$3,$4)`, actor, c.OperationID, hash[:], result); err != nil {
		return item, err
	}
	return item, tx.Commit(ctx)
}
