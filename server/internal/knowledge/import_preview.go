package knowledge

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/google/uuid"
)

const CodeImportPreviewStale = "import_preview_stale"

type ImportSummary struct {
	DocumentIDs   []string `json:"document_ids"`
	OperationID   string   `json:"operation_id"`
	SpaceID       string   `json:"space_id"`
	CollectionID  string   `json:"collection_id"`
	ActorDeviceID string   `json:"actor_device_id"`
	Added         int      `json:"added"`
	Updated       int      `json:"updated"`
	Unchanged     int      `json:"unchanged"`
}

type ImportPreview struct {
	Status           string           `json:"status"`
	Receipt          string           `json:"receipt,omitempty"`
	Summary          ImportSummary    `json:"summary"`
	Diff             []DocumentDiff   `json:"diff"`
	Review           *IdentityReview  `json:"identity_review,omitempty"`
	Before           []ImportDocument `json:"before,omitempty"`
	AffectedEvidence int              `json:"affected_evidence"`
	ImpactKnown      bool             `json:"impact_known"`
}

type ConfirmImportCommand struct {
	Request ImportCommand `json:"request"`
	Receipt string        `json:"receipt"`
}

// 适配器仅捕获计划，绝不向真实 store 发送 CommitImport。
type importPlanningStore struct {
	CatalogStore
	ScopeStore
	head     *KnowledgeRevision
	prepared *PreparedCommit
}

func (p *importPlanningStore) Head(context.Context) (*KnowledgeRevision, error) { return p.head, nil }
func (p *importPlanningStore) LookupImportOperation(context.Context, string) (ImportOperationRecord, bool, error) {
	return ImportOperationRecord{}, false, nil
}
func (p *importPlanningStore) CommitImport(_ context.Context, c PreparedCommit) (ImportResult, error) {
	p.prepared = &c
	return ImportResult{Revision: c.Revision, Unchanged: c.Unchanged}, nil
}

type importGenerationReader interface {
	ImportGeneration(context.Context) (int64, error)
}

func (s *Service) importGeneration(ctx context.Context) (int64, error) {
	if reader, ok := s.store.(importGenerationReader); ok {
		return reader.ImportGeneration(ctx)
	}
	return 0, fmt.Errorf("导入预览需要隐私 generation 端口")
}

func (s *Service) planImport(ctx context.Context, c ImportCommand) (*PreparedCommit, *KnowledgeRevision, error) {
	if err := s.checkScopeAdapter(ctx); err != nil {
		return nil, nil, err
	}
	head, err := s.store.Head(ctx)
	if err != nil {
		return nil, nil, err
	}
	store := &importPlanningStore{CatalogStore: s.store, head: head}
	store.ScopeStore, _ = s.store.(ScopeStore)
	namespace, err := uuid.Parse(c.OperationID)
	if err != nil || namespace == uuid.Nil {
		return nil, nil, &Error{Code: CodeInvalidRequest}
	}
	sequence := 0
	planner, err := NewService(store, s.canonicalizer, ServiceOptions{Now: s.now, NewUUID: func() string {
		sequence++
		return uuid.NewSHA1(namespace, []byte("import-preview-v1/"+strconv.Itoa(sequence))).String()
	}})
	if err != nil {
		return nil, nil, err
	}
	s.reviewMu.RLock()
	for k, v := range s.reviews {
		planner.reviews[k] = v
	}
	s.reviewMu.RUnlock()
	_, err = planner.Import(ctx, c)
	var domain *Error
	if errors.As(err, &domain) && domain.Review != nil {
		s.rememberIssuedReview(domain.Review)
	}
	return store.prepared, head, err
}

func importSummary(ctx context.Context, c ImportCommand, commit PreparedCommit, base *KnowledgeRevision) ImportSummary {
	r := ImportSummary{OperationID: c.OperationID, SpaceID: learningspace.Scope(ctx), CollectionID: CollectionID(ctx), ActorDeviceID: c.ActorDeviceID}
	previous := map[string]SnapshotDocument{}
	if base != nil {
		for _, d := range base.Documents {
			previous[d.Revision.DocumentID] = d
		}
	}
	selected := map[string]bool{}
	for _, d := range c.Documents {
		p, _ := NormalizePath(d.Path)
		selected[p] = true
	}
	for _, d := range commit.Revision.Documents {
		if !selected[d.Path] {
			continue
		}
		r.DocumentIDs = append(r.DocumentIDs, d.Revision.DocumentID)
		old, exists := previous[d.Revision.DocumentID]
		if !exists {
			r.Added++
		} else if old.Revision.ID == d.Revision.ID && old.Path == d.Path {
			r.Unchanged++
		} else {
			r.Updated++
		}
	}
	return r
}

type importReceipt struct {
	Hash       string `json:"hash"`
	Generation int64  `json:"generation"`
	Expires    int64  `json:"expires"`
}

func importConfirmationHash(ctx context.Context, c ImportCommand) string {
	raw, _ := json.Marshal(struct {
		Request                          ImportCommand
		Actor, Space, Collection, Policy string
	}{c, c.ActorDeviceID, learningspace.Scope(ctx), CollectionID(ctx), "import-preview-v1/" + CanonicalizerVersion + "/" + IdentityPolicyVersion})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func signImportReceipt(r importReceipt) string {
	raw, _ := json.Marshal(r)
	body := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, identityReviewKey)
	mac.Write([]byte("import-preview-v1/" + body))
	return body + "." + hex.EncodeToString(mac.Sum(nil))
}

func (s *Service) PreviewImport(ctx context.Context, c ImportCommand) (ImportPreview, error) {
	generation, err := s.importGeneration(ctx)
	if err != nil {
		return ImportPreview{}, err
	}
	commit, base, err := s.planImport(ctx, c)
	preview := ImportPreview{Status: "review", Summary: ImportSummary{OperationID: c.OperationID, SpaceID: learningspace.Scope(ctx), CollectionID: CollectionID(ctx), ActorDeviceID: c.ActorDeviceID}, Diff: []DocumentDiff{}}
	var domain *Error
	if errors.As(err, &domain) && domain.Review != nil {
		preview.Review = domain.Review
		// 仅返回有候选的原资料，正文仍受单响应预算限制。
		ids := map[string]bool{}
		for _, item := range domain.Review.Documents {
			for _, candidate := range item.Candidates {
				ids[candidate.StableID] = true
			}
		}
		paths := map[string]bool{}
		for _, item := range domain.Review.Nodes {
			paths[item.Path] = true
		}
		budget := 2 << 20
		if base != nil {
			for _, d := range base.Documents {
				if ids[d.Revision.DocumentID] || paths[d.Path] {
					body := d.Revision.CanonicalMarkdown
					if len(body) > budget {
						body = "正文超出预览预算，请通过资料详情查看完整原文。"
					} else {
						budget -= len(body)
					}
					preview.Before = append(preview.Before, ImportDocument{Path: d.Path, Markdown: body})
				}
			}
		}
		return preview, nil
	}
	if err != nil {
		return ImportPreview{}, err
	}
	var before KnowledgeRevision
	if base != nil {
		before = *base
	}
	analysis := analyzeMaintenanceRevision(before, commit.Revision)
	preview.Status = "ready"
	preview.Summary = importSummary(ctx, c, *commit, base)
	preview.Diff = analysis.diff
	if preview.Diff == nil {
		preview.Diff = []DocumentDiff{}
	}
	if s.evidenceImpactReader != nil {
		impact, readErr := s.readAcceptedEvidenceImpact(ctx, analysis.affectedNodeRevisionIDs)
		if readErr != nil {
			return ImportPreview{}, readErr
		}
		preview.AffectedEvidence = impact.Count
		preview.ImpactKnown = true
	}
	preview.Receipt = signImportReceipt(importReceipt{Hash: importConfirmationHash(ctx, c), Generation: generation, Expires: s.now().Add(15 * time.Minute).Unix()})
	return preview, nil
}

func (s *Service) ImportOperation(ctx context.Context, id, actor string) (ImportResult, error) {
	if !validUUID(id) {
		return ImportResult{}, &Error{Code: CodeInvalidRequest}
	}
	record, exists, err := s.store.LookupImportOperation(ctx, id)
	if err != nil {
		return ImportResult{}, err
	}
	if !exists || record.Result.Summary == nil {
		return ImportResult{}, &Error{Code: CodeNotFound}
	}
	r := record.Result.Summary
	if r.SpaceID != learningspace.Scope(ctx) || r.CollectionID != CollectionID(ctx) || r.ActorDeviceID != actor {
		return ImportResult{}, &Error{Code: CodeNotFound}
	}
	record.Result.Replayed = true
	return record.Result, nil
}

func (s *Service) ConfirmImport(ctx context.Context, c ConfirmImportCommand) (ImportResult, error) {
	if !validUUID(c.Request.OperationID) || !c.Request.ExpectedParentProvided {
		return ImportResult{}, &Error{Code: CodeInvalidRequest}
	}
	hash := importConfirmationHash(ctx, c.Request)
	record, exists, err := s.store.LookupImportOperation(ctx, c.Request.OperationID)
	if err != nil {
		return ImportResult{}, err
	}
	if exists {
		if record.RequestHash != hash {
			return ImportResult{}, &Error{Code: CodeIdempotencyConflict}
		}
		return s.ImportOperation(ctx, c.Request.OperationID, c.Request.ActorDeviceID)
	}
	parts := strings.Split(c.Receipt, ".")
	var receipt importReceipt
	if len(parts) != 2 {
		return ImportResult{}, &Error{Code: CodeImportPreviewStale}
	}
	raw, decodeErr := base64.RawURLEncoding.DecodeString(parts[0])
	if decodeErr != nil || json.Unmarshal(raw, &receipt) != nil || !hmac.Equal([]byte(signImportReceipt(receipt)), []byte(c.Receipt)) || receipt.Hash != hash || s.now().Unix() >= receipt.Expires {
		return ImportResult{}, &Error{Code: CodeImportPreviewStale}
	}
	generation, err := s.importGeneration(ctx)
	if err != nil {
		return ImportResult{}, err
	}
	if generation != receipt.Generation {
		return ImportResult{}, &Error{Code: CodeImportPreviewStale}
	}
	commit, base, err := s.planImport(ctx, c.Request)
	if err != nil {
		// 同一操作可能刚被另一请求提交；此时父版本冲突不能覆盖成功回执。
		record, exists, lookupErr := s.store.LookupImportOperation(ctx, c.Request.OperationID)
		if lookupErr != nil {
			return ImportResult{}, lookupErr
		}
		if exists {
			if record.RequestHash != hash {
				return ImportResult{}, &Error{Code: CodeIdempotencyConflict}
			}
			return s.ImportOperation(ctx, c.Request.OperationID, c.Request.ActorDeviceID)
		}
		return ImportResult{}, err
	}
	commit.ExpectedGeneration = &receipt.Generation
	commit.RequestHash = hash
	summary := importSummary(ctx, c.Request, *commit, base)
	commit.Summary = &summary
	return s.store.CommitImport(ctx, *commit)
}
