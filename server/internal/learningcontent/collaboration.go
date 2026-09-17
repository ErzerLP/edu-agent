package learningcontent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Selection 的范围是原块 text 的 UTF-8 字节偏移，不是渲染后 DOM 的偏移。
type Selection struct {
	SpaceID    string `json:"space_id"`
	GoalID     string `json:"goal_id"`
	SessionID  string `json:"session_id"`
	ArtifactID string `json:"artifact_id"`
	Version    int64  `json:"version"`
	BlockID    string `json:"block_id"`
	Start      int    `json:"start"`
	End        int    `json:"end"`
	Hash       string `json:"sha256"`
}

type EditRequest struct {
	Selection Selection `json:"selection"`
	Action    string    `json:"action"`
}

// 模型只能提供候选正文和已知引用，不能选择目标块、版本或修改评分结构。
type Candidate struct {
	Text         string   `json:"text"`
	ReferenceIDs []string `json:"reference_ids"`
}

type Origin struct {
	ArtifactID    string `json:"artifact_id"`
	Version       int64  `json:"version"`
	BlockID       string `json:"block_id"`
	SourceBlockID string `json:"source_block_id,omitempty"`
}

type Change struct {
	BaseVersion     int64      `json:"base_version"`
	RestoredVersion int64      `json:"restored_version,omitempty"`
	Action          string     `json:"action"`
	Reason          string     `json:"reason"`
	Selection       *Selection `json:"selection,omitempty"`
	ChangedBlocks   []string   `json:"changed_blocks"`
}

func TextHash(text string) string {
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:])
}

func FindBlock(blocks []Block, id string) *Block {
	for i := range blocks {
		if blocks[i].ID == id {
			return &blocks[i]
		}
		if found := FindBlock(blocks[i].Children, id); found != nil {
			return found
		}
	}
	return nil
}

func Select(r Revision, s Selection) (string, error) {
	if s.SpaceID != r.SpaceID || s.GoalID != r.GoalID || s.SessionID != r.SessionID || s.ArtifactID != r.ArtifactID {
		return "", ErrNotFound
	}
	if s.Version != r.Version || r.Version != r.CommittedVersion || r.Status != "committed" {
		return "", ErrConflict
	}
	b := FindBlock(r.Body.Blocks, s.BlockID)
	if b == nil || b.Kind == "citation" || b.Kind == "answer_input" || b.Kind == "group" || s.Start < 0 || s.End <= s.Start || s.End > len(b.Text) {
		return "", ErrInvalid
	}
	text := b.Text[s.Start:s.End]
	if !utf8.ValidString(text) || !utf8.ValidString(b.Text[:s.Start]) || TextHash(text) != s.Hash {
		return "", ErrConflict
	}
	return text, nil
}

func (r EditRequest) Validate() error {
	for _, id := range []string{r.Selection.SpaceID, r.Selection.GoalID, r.Selection.SessionID, r.Selection.ArtifactID, r.Selection.BlockID} {
		if uuid.Validate(id) != nil {
			return ErrInvalid
		}
	}
	if r.Selection.Version < 1 || len(r.Selection.Hash) != 64 {
		return ErrInvalid
	}
	switch r.Action {
	case "explain", "example", "expand", "critique", "rewrite":
		return nil
	}
	return ErrInvalid
}

func Patch(r Revision, request EditRequest, candidate Candidate, reason, operation, model string) (Body, error) {
	if request.Validate() != nil || uuid.Validate(operation) != nil || strings.TrimSpace(reason) == "" || len(reason) > 16000 || !utf8.ValidString(reason) {
		return Body{}, ErrInvalid
	}
	if _, err := Select(r, request.Selection); err != nil {
		return Body{}, err
	}
	if strings.TrimSpace(candidate.Text) == "" || len(candidate.Text) > 32000 || !utf8.ValidString(candidate.Text) || strings.ContainsRune(candidate.Text, 0) {
		return Body{}, ErrInvalid
	}
	for _, id := range candidate.ReferenceIDs {
		found := false
		for _, ref := range r.Body.References {
			if ref.NodeRevisionID == id {
				found = true
				break
			}
		}
		if !found {
			return Body{}, ErrInvalid
		}
	}
	// 深复制保证历史、原题及未选块不受候选生成过程影响。
	raw, _ := json.Marshal(r.Body)
	var body Body
	if err := json.Unmarshal(raw, &body); err != nil {
		return Body{}, err
	}
	b := FindBlock(body.Blocks, request.Selection.BlockID)
	changed := b.ID
	// 原始教学正文只附加替代解释；已派生的纯文字块允许精确替换选区。
	derived := false
	for _, origin := range body.Lineage {
		if origin.BlockID == b.ID {
			derived = true
		}
	}
	if request.Action == "rewrite" && derived && (b.Kind == "callout" || b.Kind == "markdown" || b.Kind == "code") {
		b.Text = b.Text[:request.Selection.Start] + candidate.Text + b.Text[request.Selection.End:]
		b.Fallback = b.Text
	} else {
		changed = uuid.NewSHA1(namespace, []byte(r.ArtifactID+":"+operation)).String()
		addition := Block{ID: changed, Kind: "callout", Text: candidate.Text, Fallback: candidate.Text}
		var insert func([]Block) []Block
		insert = func(blocks []Block) []Block {
			for i := range blocks {
				if blocks[i].ID == b.ID {
					return append(blocks[:i+1], append([]Block{addition}, blocks[i+1:]...)...)
				}
				blocks[i].Children = insert(blocks[i].Children)
			}
			return blocks
		}
		body.Blocks = insert(body.Blocks)
		body.Lineage = append(body.Lineage, Origin{ArtifactID: r.ArtifactID, Version: r.Version, BlockID: changed, SourceBlockID: request.Selection.BlockID})
	}
	body.Change = &Change{BaseVersion: r.Version, Action: request.Action, Reason: reason, Selection: &request.Selection, ChangedBlocks: []string{changed}}
	body.ModelID = model
	body.InputFingerprint = fingerprint(struct {
		Request EditRequest
		Reason  string
	}{request, reason})
	return body, nil
}
