package learningcontent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type Preference struct {
	Favorite      bool   `json:"favorite"`
	PinnedVersion *int64 `json:"pinned_version"`
}
type LibraryQuery struct {
	GoalID, Kind, NodeID, SourceStatus, Cursor string
	Favorite                                   bool
	After, Before                              *time.Time
	Limit                                      int
}
type LibraryItem struct {
	KnowledgePoints []KnowledgePoint `json:"knowledge_points"`
	ArtifactID      string           `json:"artifact_id"`
	GoalID          string           `json:"goal_id"`
	SessionID       string           `json:"session_id"`
	Version         int64            `json:"version"`
	Title           string           `json:"title"`
	Kind            string           `json:"kind"`
	SourceStatus    string           `json:"source_status"`
	Nodes           []string         `json:"nodes"`
	UpdatedAt       time.Time        `json:"updated_at"`
	Preference
}

type KnowledgePoint struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type LibraryPage struct {
	Items      []LibraryItem `json:"items"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

func (s *Store) Preference(ctx context.Context, actor identity.Credential, id string, update *Preference) (Preference, error) {
	if uuid.Validate(id) != nil {
		return Preference{}, ErrInvalid
	}
	tx, g, err := s.begin(ctx, actor, update != nil)
	if err != nil {
		return Preference{}, err
	}
	defer tx.Rollback(context.Background())
	if _, err = s.read(ctx, tx, id, 0, g, false); err != nil {
		return Preference{}, err
	}
	if update != nil {
		if update.PinnedVersion != nil {
			if *update.PinnedVersion < 1 {
				return Preference{}, ErrInvalid
			}
			r, err := s.read(ctx, tx, id, *update.PinnedVersion, g, false)
			if err != nil {
				return Preference{}, err
			}
			if r.Status != "committed" {
				return Preference{}, ErrInvalid
			}
		}
		if _, err = tx.Exec(ctx, `INSERT INTO learning_content_preferences(device_id,artifact_id,favorite,pinned_version) VALUES($1,$2,$3,$4) ON CONFLICT(device_id,artifact_id) DO UPDATE SET favorite=excluded.favorite,pinned_version=excluded.pinned_version`, actor.Device.ID, id, update.Favorite, update.PinnedVersion); err != nil {
			return Preference{}, err
		}
		return *update, tx.Commit(ctx)
	}
	var result Preference
	err = tx.QueryRow(ctx, `SELECT favorite,pinned_version FROM learning_content_preferences WHERE device_id=$1 AND artifact_id=$2`, actor.Device.ID, id).Scan(&result.Favorite, &result.PinnedVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	if err != nil {
		return result, err
	}
	return result, tx.Commit(ctx)
}

func (s *Store) Library(ctx context.Context, actor identity.Credential, q LibraryQuery) (LibraryPage, error) {
	page := LibraryPage{Items: []LibraryItem{}}
	for _, id := range []string{q.GoalID, q.NodeID, q.Cursor} {
		if id != "" && uuid.Validate(id) != nil {
			return page, ErrInvalid
		}
	}
	if q.Limit < 1 || q.Limit > 50 || (q.Kind != "" && q.Kind != "reading" && q.Kind != "exercise") || (q.SourceStatus != "" && q.SourceStatus != "available" && q.SourceStatus != "missing" && q.SourceStatus != "restricted") {
		return page, ErrInvalid
	}
	tx, g, err := s.begin(ctx, actor, false)
	if err != nil {
		return page, err
	}
	defer tx.Rollback(context.Background())
	// 游标按稳定 artifact 身份推进，筛选期间的新修订不会使同一条目反复出现。
	rows, err := tx.Query(ctx, `SELECT a.id FROM learning_content_artifacts a JOIN learning_content_revisions r ON r.artifact_id=a.id AND r.version=a.committed_version LEFT JOIN learning_content_preferences p ON p.artifact_id=a.id AND p.device_id=$3 WHERE a.space_id=$1 AND a.privacy_generation=$2 AND ($4='' OR a.goal_id::text=$4) AND ($5='' OR a.id::text>$5) AND (NOT $6::boolean OR p.favorite=TRUE) AND ($7::timestamptz IS NULL OR r.created_at>=$7) AND ($8::timestamptz IS NULL OR r.created_at<=$8) ORDER BY a.id LIMIT 501`, learningspace.Scope(ctx), g, actor.Device.ID, q.GoalID, q.Cursor, q.Favorite, q.After, q.Before)
	if err != nil {
		return page, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return page, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return page, err
	}
	for i, id := range ids {
		if i == 500 {
			page.NextCursor = ids[i-1]
			break
		}
		r, err := s.read(ctx, tx, id, 0, g, false)
		status := "available"
		if errors.Is(err, ErrForbidden) || errors.Is(err, ErrNotFound) {
			status = "restricted"
		} else if err != nil {
			return page, err
		}
		kind := "exercise"
		if r.Body.Interaction.Kind == "none" {
			kind = "reading"
		}
		nodes := []string{}
		matches := q.NodeID == ""
		for _, ref := range r.Body.References {
			nodes = append(nodes, ref.NodeID)
			if ref.NodeID == q.NodeID {
				matches = true
			}
			if ref.Slice == "" && status != "restricted" {
				status = "missing"
			}
		}
		if len(r.Body.References) == 0 && status != "restricted" {
			status = "missing"
		}
		if !matches || q.Kind != "" && q.Kind != kind || q.SourceStatus != "" && q.SourceStatus != status {
			continue
		}
		if len(page.Items) == q.Limit {
			page.NextCursor = ids[i-1]
			break
		}
		title := "学习内容"
		if len(r.Body.Blocks) > 0 {
			title = strings.TrimSpace(strings.SplitN(r.Body.Blocks[0].Text, "\n", 2)[0])
			runes := []rune(title)
			if len(runes) > 72 {
				title = string(runes[:72]) + "…"
			}
		}
		if status == "restricted" {
			title = "来源已不可用的内容"
			nodes = []string{}
		}
		item := LibraryItem{ArtifactID: id, GoalID: r.GoalID, SessionID: r.SessionID, Version: r.Version, Title: title, Kind: kind, SourceStatus: status, Nodes: nodes, UpdatedAt: r.CreatedAt}
		item.KnowledgePoints = []KnowledgePoint{}
		if status != "restricted" && s.references != nil {
			for _, ref := range r.Body.References {
				citation, err := s.references.ContentCitationTx(ctx, tx, ref)
				if err != nil {
					return page, err
				}
				name := citation.Title
				if name == "" {
					name = citation.Locator
				}
				if name == "" {
					name = "原资料章节"
				}
				item.KnowledgePoints = append(item.KnowledgePoints, KnowledgePoint{ID: ref.NodeID, Name: name})
			}
		}
		err = tx.QueryRow(ctx, `SELECT favorite,pinned_version FROM learning_content_preferences WHERE device_id=$1 AND artifact_id=$2`, actor.Device.ID, id).Scan(&item.Favorite, &item.PinnedVersion)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return page, err
		}
		page.Items = append(page.Items, item)
	}
	return page, tx.Commit(ctx)
}

type Export struct {
	Filename  string `json:"filename"`
	MediaType string `json:"media_type"`
	Text      string `json:"text"`
}

func (s *Store) Export(ctx context.Context, actor identity.Credential, id string, version int64, format string) (Export, error) {
	if format != "markdown" && format != "json" {
		return Export{}, ErrInvalid
	}
	r, err := s.Get(ctx, actor, id, version)
	if err != nil {
		return Export{}, err
	}
	result := Export{Filename: fmt.Sprintf("content-%s-v%d", id, r.Version)}
	if format == "json" {
		raw, err := json.MarshalIndent(r, "", "  ")
		result.Filename += ".json"
		result.MediaType = "application/json"
		result.Text = string(raw)
		return result, err
	}
	var text strings.Builder
	fmt.Fprintf(&text, "# 学习内容 · 第 %d 版\n\n这是教学派生内容，不能作为独立外部权威。\n\n", r.Version)
	var walk func([]Block)
	walk = func(blocks []Block) {
		for _, b := range blocks {
			if b.Kind == "group" {
				walk(b.Children)
				continue
			}
			value := b.Text
			if value == "" {
				value = b.Fallback
			}
			fmt.Fprintf(&text, "%s\n\n", value)
		}
	}
	walk(r.Body.Blocks)
	text.WriteString("## 原始来源链\n\n")
	for _, ref := range r.Body.References {
		fmt.Fprintf(&text, "- 节点版本 %s；资料版本 %s；字节范围 %d–%d；SHA-256 %s\n", ref.NodeRevisionID, ref.KnowledgeRevisionID, ref.Range.Start, ref.Range.End, ref.SliceSHA256)
	}
	for _, origin := range r.Body.Lineage {
		fmt.Fprintf(&text, "- 派生自内容 %s 第 %d 版，原块 %s\n", origin.ArtifactID, origin.Version, origin.SourceBlockID)
	}
	result.Filename += ".md"
	result.MediaType = "text/markdown"
	result.Text = text.String()
	return result, nil
}

type Reuse struct {
	OperationID      string `json:"operation_id"`
	ExpectedVersion  int64  `json:"expected_version"`
	SourceArtifactID string `json:"source_artifact_id"`
	SourceVersion    int64  `json:"source_version"`
	SourceBlockID    string `json:"source_block_id"`
	Reason           string `json:"reason"`
}

func (s *Store) Reuse(ctx context.Context, actor identity.Credential, id string, c Reuse) (Revision, error) {
	if c.SourceVersion < 1 || uuid.Validate(c.SourceArtifactID) != nil || uuid.Validate(c.SourceBlockID) != nil || strings.TrimSpace(c.Reason) == "" || len(c.Reason) > 16000 {
		return Revision{}, ErrInvalid
	}
	tx, g, err := s.begin(ctx, actor, true)
	if err != nil {
		return Revision{}, err
	}
	defer tx.Rollback(context.Background())
	original, err := s.read(ctx, tx, c.SourceArtifactID, c.SourceVersion, g, false)
	if err != nil {
		return Revision{}, err
	}
	block := FindBlock(original.Body.Blocks, c.SourceBlockID)
	if original.Status != "committed" || block == nil || block.Text == "" {
		return Revision{}, ErrInvalid
	}
	r, err := s.changeTx(ctx, tx, actor, id, c.OperationID, c.ExpectedVersion, fingerprint(c), func(target Revision) (Body, error) {
		body := target.Body
		newID := uuid.NewSHA1(namespace, []byte(id+":"+c.OperationID)).String()
		body.Blocks = append(body.Blocks, Block{ID: newID, Kind: "callout", Text: block.Text, Fallback: block.Text})
		for _, ref := range original.Body.References {
			found := false
			for _, current := range body.References {
				if current.NodeRevisionID == ref.NodeRevisionID {
					found = true
				}
			}
			if !found {
				body.References = append(body.References, ref)
			}
		}
		body.Lineage = append(body.Lineage, original.Body.Lineage...)
		body.Lineage = append(body.Lineage, Origin{ArtifactID: original.ArtifactID, Version: original.Version, BlockID: newID, SourceBlockID: block.ID})
		body.Change = &Change{BaseVersion: target.Version, Action: "reuse", Reason: c.Reason, ChangedBlocks: []string{newID}}
		return body, nil
	})
	if err != nil {
		return Revision{}, err
	}
	return r, tx.Commit(ctx)
}
