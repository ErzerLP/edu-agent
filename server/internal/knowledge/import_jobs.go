package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/google/uuid"
)

const MaxImportJobBytes = 128 << 20

type ImportJobItem struct {
	Path   string `json:"path"`
	Digest string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}
type ImportJobBatch struct {
	RequestHash     string         `json:"request_hash,omitempty"`
	Item            ImportJobItem  `json:"item"`
	OperationID     string         `json:"operation_id"`
	Status          string         `json:"status"`
	Error           string         `json:"error,omitempty"`
	Preview         *ImportPreview `json:"preview,omitempty"`
	Result          *ImportResult  `json:"result,omitempty"`
	PlannedRevision string         `json:"planned_revision,omitempty"`
	PlannedManifest string         `json:"planned_manifest,omitempty"`
}
type ImportJob struct {
	BatchCount     int              `json:"batch_count"`
	ID             string           `json:"id"`
	Actor          string           `json:"actor_device_id"`
	Space          string           `json:"space_id"`
	Collection     string           `json:"collection_id"`
	Generation     int64            `json:"generation"`
	Version        int64            `json:"version"`
	PlanVersion    int64            `json:"plan_version"`
	Approved       bool             `json:"approved"`
	Status         string           `json:"status"`
	Created        time.Time        `json:"created_at"`
	Expires        time.Time        `json:"expires_at"`
	Batches        []ImportJobBatch `json:"batches"`
	CleanupPending bool             `json:"cleanup_pending"`
}
type ImportJobPage struct {
	Items      []ImportJob `json:"items"`
	NextCursor string      `json:"next_cursor,omitempty"`
}
type ImportJobCommand struct {
	ContentBase64       string               `json:"content_base64,omitempty"`
	ID                  string               `json:"id"`
	Action              string               `json:"action"`
	Items               []ImportJobItem      `json:"items,omitempty"`
	Batch               int                  `json:"batch,omitempty"`
	Document            *ImportDocument      `json:"document,omitempty"`
	PlanVersion         int64                `json:"plan_version,omitempty"`
	DocumentResolutions []DocumentResolution `json:"document_resolutions,omitempty"`
	NodeResolutions     []NodeResolution     `json:"node_resolutions,omitempty"`
}
type ImportJobStore interface {
	CreateImportJob(context.Context, ImportJob) (ImportJob, error)
	LoadImportJob(context.Context, string, string) (ImportJob, error)
	ListImportJobs(context.Context, string, string) (ImportJobPage, error)
	SaveImportJob(context.Context, *ImportJob) error
	LockImportJob(context.Context, string) (func(), error)
	PutImportJobPayload(context.Context, ImportJob, int, []byte) error
	ImportJobPayload(context.Context, ImportJob, int) ([]byte, error)
	CleanImportJob(context.Context, *ImportJob) error
}

func jobError(code string) error   { return &Error{Code: code} }
func jobDigest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func (s *Service) jobStore() (ImportJobStore, error) {
	store, ok := s.store.(ImportJobStore)
	if !ok {
		return nil, jobError("import_jobs_unavailable")
	}
	return store, nil
}
func (s *Service) ImportJobs(ctx context.Context, actor, cursor string) (ImportJobPage, error) {
	store, err := s.jobStore()
	if err != nil {
		return ImportJobPage{}, err
	}
	if cursor != "" && !validUUID(cursor) {
		return ImportJobPage{}, jobError(CodeInvalidRequest)
	}
	return store.ListImportJobs(ctx, actor, cursor)
}
func (s *Service) RunImportJob(ctx context.Context, actor string, c ImportJobCommand) (ImportJob, error) {
	store, err := s.jobStore()
	if err != nil {
		return ImportJob{}, err
	}
	if !validUUID(c.ID) {
		return ImportJob{}, jobError(CodeInvalidRequest)
	}
	unlock, err := store.LockImportJob(ctx, c.ID)
	if err != nil {
		return ImportJob{}, err
	}
	defer unlock()
	if c.Action == "create" {
		if len(c.Items) == 0 || len(c.Items) > MaxImportDocuments {
			return ImportJob{}, jobError(CodeInvalidRequest)
		}
		generation, err := s.importGeneration(ctx)
		if err != nil {
			return ImportJob{}, err
		}
		j := ImportJob{ID: c.ID, Actor: actor, Space: learningspace.Scope(ctx), Collection: CollectionID(ctx), Generation: generation, Version: 1, Status: "uploading", Created: s.now().UTC(), Expires: s.now().UTC().Add(24 * time.Hour)}
		seen, total := map[string]bool{}, 0
		for i, item := range c.Items {
			p, err := NormalizePath(item.Path)
			hash, hashErr := hex.DecodeString(item.Digest)
			if err != nil || p != item.Path || seen[foldedPath(p)] || item.Bytes < 0 || item.Bytes > 4<<20 || hashErr != nil || len(hash) != 32 || hex.EncodeToString(hash) != item.Digest {
				return ImportJob{}, jobError(CodeInvalidRequest)
			}
			seen[foldedPath(p)] = true
			total += item.Bytes
			j.Batches = append(j.Batches, ImportJobBatch{Item: item, OperationID: uuid.NewSHA1(uuid.MustParse(j.ID), []byte("batch/"+strconv.Itoa(i))).String(), Status: "pending"})
		}
		if total > MaxImportJobBytes {
			return ImportJob{}, jobError("import_job_quota")
		}
		return store.CreateImportJob(ctx, j)
	}
	j, err := store.LoadImportJob(ctx, c.ID, actor)
	if err != nil {
		return j, err
	}
	// 对账总是先于取消、过期与重新规划，未知结果绝不能被覆盖成失败。
	if err = s.reconcileImportJob(ctx, store, &j); err != nil {
		return j, err
	}
	if !s.now().Before(j.Expires) && j.Status != "completed" && j.Status != "cancelled" {
		j.Status = "expired"
		j.Approved = false
		j.CleanupPending = true
		if err = store.SaveImportJob(ctx, &j); err != nil {
			return j, err
		}
		if err = store.CleanImportJob(ctx, &j); err != nil {
			return j, err
		}
	}
	if c.Action == "get" {
		if j.CleanupPending {
			err = store.CleanImportJob(ctx, &j)
			return j, err
		}
		return j, nil
	}
	if c.Action == "cancel" || c.Action == "clean" {
		if j.Status != "completed" && j.Status != "expired" {
			j.Status = "cancelled"
		}
		j.Approved = false
		j.CleanupPending = true
		if err = store.SaveImportJob(ctx, &j); err != nil {
			return j, err
		}
		err = store.CleanImportJob(ctx, &j)
		return j, err
	}
	if j.Status == "completed" || j.Status == "cancelled" || j.Status == "expired" {
		return j, jobError("import_job_closed")
	}
	switch c.Action {
	case "upload":
		if c.Batch < 0 || c.Batch >= len(j.Batches) {
			return j, jobError(CodeInvalidRequest)
		}
		b := &j.Batches[c.Batch]
		if c.ContentBase64 != "" {
			if c.Document != nil || len(c.ContentBase64) > base64.StdEncoding.EncodedLen(4<<20) {
				return j, jobError(CodeInvalidRequest)
			}
			raw, e := base64.StdEncoding.Strict().DecodeString(c.ContentBase64)
			if e != nil {
				return j, jobError(CodeInvalidRequest)
			}
			c.Document = &ImportDocument{Path: b.Item.Path, Markdown: string(raw)}
		}
		if c.Document == nil {
			return j, jobError(CodeInvalidRequest)
		}
		if c.Document.Path != b.Item.Path || len(c.Document.Markdown) != b.Item.Bytes || jobDigest([]byte(c.Document.Markdown)) != b.Item.Digest {
			return j, jobError("import_job_source_changed")
		}
		if b.Status != "pending" && b.Status != "missing" {
			return j, nil
		}
		if _, err = s.prepareDocuments([]ImportDocument{*c.Document}); err != nil {
			return j, err
		}
		request := ImportCommand{OperationID: b.OperationID, Source: "import-job-v1", ActorDeviceID: actor, ExpectedParentProvided: true, Documents: []ImportDocument{*c.Document}}
		raw, _ := json.Marshal(request)
		if err = store.PutImportJobPayload(ctx, j, c.Batch, raw); err != nil {
			return j, err
		}
		b.Status = "uploaded"
		b.Error = ""
		j.Approved = false
		j.Status = "uploading"
	case "preview", "resolve":
		j.Approved = false
		j.Status = "review"
		j.PlanVersion++
		if err = store.SaveImportJob(ctx, &j); err != nil {
			return j, err
		}
		if c.Action == "resolve" {
			if c.Batch < 0 || c.Batch >= len(j.Batches) || j.Batches[c.Batch].Preview == nil || j.Batches[c.Batch].Preview.Review == nil {
				return j, jobError(CodeInvalidRequest)
			}
			raw, e := store.ImportJobPayload(ctx, j, c.Batch)
			if e != nil {
				return j, e
			}
			var request ImportCommand
			if json.Unmarshal(raw, &request) != nil {
				return j, jobError("import_job_staging_missing")
			}
			r := j.Batches[c.Batch].Preview.Review
			request.IdentityReviewBasisHash = r.BasisHash
			request.IdentityReviewOperationID = r.OperationID
			request.IdentityReviewReceipt = r.Receipt
			request.DocumentResolutions = append(request.DocumentResolutions, c.DocumentResolutions...)
			request.NodeResolutions = append(request.NodeResolutions, c.NodeResolutions...)
			// 决定使用新的规划操作，旧审阅操作仅作为服务端依据。
			request.OperationID = uuid.NewString()
			j.Batches[c.Batch].OperationID = request.OperationID
			raw, _ = json.Marshal(request)
			if err = store.PutImportJobPayload(ctx, j, c.Batch, raw); err != nil {
				return j, err
			}
		}
		err = s.previewImportJob(ctx, store, &j)
		if err != nil {
			return j, err
		}
	case "confirm":
		if j.Approved && c.PlanVersion == j.PlanVersion {
			return j, nil
		}
		if j.Status != "ready" || c.PlanVersion != j.PlanVersion {
			return j, jobError(CodeImportPreviewStale)
		}
		j.Approved = true
		j.Status = "committing"
	case "continue":
		if !j.Approved {
			return j, jobError("import_job_confirmation_required")
		}
		return s.continueImportJob(ctx, store, j)
	default:
		return j, jobError(CodeInvalidRequest)
	}
	err = store.SaveImportJob(ctx, &j)
	return j, err
}

func (s *Service) reconcileImportJob(ctx context.Context, store ImportJobStore, j *ImportJob) error {
	for i := range j.Batches {
		b := &j.Batches[i]
		if b.Status != "unknown" {
			continue
		}
		record, exists, lookupErr := s.store.LookupImportOperation(ctx, b.OperationID)
		if lookupErr != nil {
			return lookupErr
		}
		if !exists {
			continue
		}
		if b.RequestHash == "" || record.RequestHash != b.RequestHash {
			b.Status = "failed"
			b.Error = CodeIdempotencyConflict
			j.Status = "failed"
			j.Approved = false
			if err := store.SaveImportJob(ctx, j); err != nil {
				return err
			}
			continue
		}
		r, err := s.ImportOperation(ctx, b.OperationID, j.Actor)
		var domain *Error
		if errors.As(err, &domain) && domain.Code == CodeNotFound {
			continue
		}
		if err != nil {
			return err
		}
		r.Revision.Documents = nil
		r.Revision.Lineages = nil
		b.Result = &r
		b.Status = "completed"
		b.Error = ""
		if err = store.SaveImportJob(ctx, j); err != nil {
			return err
		}
	}
	complete := len(j.Batches) > 0
	for _, b := range j.Batches {
		if b.Status != "completed" {
			complete = false
		}
	}
	if complete && j.Status != "completed" {
		j.Status = "completed"
		j.Approved = false
		j.CleanupPending = true
		if err := store.SaveImportJob(ctx, j); err != nil {
			return err
		}
		return store.CleanImportJob(ctx, j)
	}
	return nil
}

// 虚拟版本只用于规划；正式写入继续使用既有事务端口。
type jobPlanningStore struct {
	CatalogStore
	ScopeStore
	head      *KnowledgeRevision
	revisions map[string]KnowledgeRevision
}

func (p *jobPlanningStore) Head(context.Context) (*KnowledgeRevision, error) { return p.head, nil }
func (p *jobPlanningStore) Revision(ctx context.Context, id string) (KnowledgeRevision, error) {
	if r, ok := p.revisions[id]; ok {
		return r, nil
	}
	return p.CatalogStore.Revision(ctx, id)
}
func (p *jobPlanningStore) ImportGeneration(ctx context.Context) (int64, error) {
	return p.CatalogStore.(importGenerationReader).ImportGeneration(ctx)
}
func (p *jobPlanningStore) DocumentIdentityExists(ctx context.Context, id string) (bool, error) {
	if p.head != nil {
		for _, d := range p.head.Documents {
			if d.Revision.DocumentID == id {
				return true, nil
			}
		}
	}
	return p.CatalogStore.DocumentIdentityExists(ctx, id)
}
func (p *jobPlanningStore) NodeIdentityOwner(ctx context.Context, id string) (string, bool, error) {
	if p.head != nil {
		for _, d := range p.head.Documents {
			for _, n := range d.Revision.Nodes {
				if n.NodeID == id {
					return d.Revision.DocumentID, true, nil
				}
			}
		}
	}
	return p.CatalogStore.NodeIdentityOwner(ctx, id)
}

func (s *Service) restoreJobReview(c *ImportCommand) {
	if c.IdentityReviewBasisHash != "" && c.IdentityReviewOperationID != "" {
		c.IdentityReviewReceipt = identityReviewReceipt(c.IdentityReviewBasisHash, c.IdentityReviewOperationID)
		s.rememberIssuedReview(&IdentityReview{BasisHash: c.IdentityReviewBasisHash, OperationID: c.IdentityReviewOperationID, Receipt: c.IdentityReviewReceipt})
	}
}
func (s *Service) previewImportJob(ctx context.Context, store ImportJobStore, j *ImportJob) error {
	for _, b := range j.Batches {
		if b.Status == "pending" || b.Status == "missing" || b.Status == "unknown" {
			return jobError("import_job_not_ready")
		}
	}
	head, err := s.store.Head(ctx)
	if err != nil {
		return err
	}
	virtual := &jobPlanningStore{CatalogStore: s.store, head: head, revisions: map[string]KnowledgeRevision{}}
	virtual.ScopeStore, _ = s.store.(ScopeStore)
	planner, _ := NewService(virtual, s.canonicalizer, ServiceOptions{Now: s.now, EvidenceImpactReader: s.evidenceImpactReader})
	j.Approved = false
	j.Status = "ready"
	for i := range j.Batches {
		b := &j.Batches[i]
		if b.Status == "completed" {
			continue
		}
		raw, e := store.ImportJobPayload(ctx, *j, i)
		if e != nil {
			b.Status = "missing"
			b.Error = "import_job_staging_missing"
			j.Status = "failed"
			break
		}
		var c ImportCommand
		if json.Unmarshal(raw, &c) != nil {
			return jobError("import_job_staging_missing")
		}
		b.OperationID = c.OperationID
		if !sameOptionalID(c.ExpectedParentRevisionID, revisionID(virtual.head)) {
			c.DocumentResolutions = nil
			c.NodeResolutions = nil
			c.IdentityReviewBasisHash = ""
			c.IdentityReviewOperationID = ""
			c.IdentityReviewReceipt = ""
		}
		c.ExpectedParentRevisionID = revisionID(virtual.head)
		c.ActorDeviceID = j.Actor
		c.ExpectedParentProvided = true
		planner.restoreJobReview(&c)
		preview, e := planner.PreviewImport(ctx, c)
		if e != nil {
			b.Status = "failed"
			b.Error = CodeInvalidRequest
			j.Status = "failed"
			break
		}
		if preview.Summary.DocumentIDs == nil {
			preview.Summary.DocumentIDs = []string{}
		}
		b.Preview = &preview
		b.Error = ""
		b.Status = "review"
		raw, _ = json.Marshal(c)
		if err = store.PutImportJobPayload(ctx, *j, i, raw); err != nil {
			return err
		}
		if preview.Status != "ready" {
			j.Status = "review"
			break
		}
		commit, _, e := planner.planImport(ctx, c)
		if e != nil {
			return e
		}
		b.Status = "ready"
		b.PlannedRevision = commit.Revision.ID
		b.PlannedManifest = commit.Revision.ManifestHash
		virtual.head = &commit.Revision
		virtual.revisions[commit.Revision.ID] = commit.Revision
	}
	return nil
}
func jobRequestHash(j ImportJob, c ImportCommand) string {
	c.IdentityReviewReceipt = ""
	raw, _ := json.Marshal(struct {
		ID         string
		Generation int64
		Request    ImportCommand
	}{j.ID, j.Generation, c})
	return jobDigest(raw)
}
func (s *Service) continueImportJob(ctx context.Context, store ImportJobStore, j ImportJob) (ImportJob, error) {
	for i := range j.Batches {
		b := &j.Batches[i]
		if b.Status == "completed" {
			continue
		}
		if b.Status != "ready" && b.Status != "unknown" {
			return j, jobError("import_job_not_ready")
		}
		raw, err := store.ImportJobPayload(ctx, j, i)
		if err != nil {
			b.Status = "missing"
			b.Error = "import_job_staging_missing"
			j.Status = "failed"
			j.Approved = false
			return j, store.SaveImportJob(ctx, &j)
		}
		var c ImportCommand
		if json.Unmarshal(raw, &c) != nil {
			return j, jobError("import_job_staging_missing")
		}
		s.restoreJobReview(&c)
		c.ActorDeviceID = j.Actor
		commit, base, err := s.planImport(ctx, c)
		if err != nil || commit.Revision.ID != b.PlannedRevision || commit.Revision.ManifestHash != b.PlannedManifest {
			var domain *Error
			if err != nil && !errors.As(err, &domain) {
				return j, err
			}
			b.Status = "stale"
			b.Error = CodeImportPreviewStale
			j.Approved = false
			j.Status = "review"
			return j, store.SaveImportJob(ctx, &j)
		}
		commit.ExpectedGeneration = &j.Generation
		commit.RequestHash = jobRequestHash(j, c)
		b.RequestHash = commit.RequestHash
		summary := importSummary(ctx, c, *commit, base)
		commit.Summary = &summary
		b.Status = "unknown"
		j.Status = "committing"
		if err = store.SaveImportJob(ctx, &j); err != nil {
			return j, err
		}
		result, err := s.store.CommitImport(ctx, *commit)
		if err != nil {
			var domain *Error
			if errors.As(err, &domain) {
				b.Status = "failed"
				b.Error = domain.Code
				j.Status = "failed"
				j.Approved = false
				if domain.Code == CodeRevisionConflict || domain.Code == CodeImportPreviewStale {
					b.Status = "stale"
					j.Status = "review"
				}
				return j, store.SaveImportJob(ctx, &j)
			}
			return j, nil
		} // 事务结果未知，下一次先查原回执，不分配新操作。
		result.Revision.Documents = nil
		result.Revision.Lineages = nil
		b.Result = &result
		b.Status = "completed"
		b.Preview = nil
		j.Status = "partial"
		break
	}
	complete := true
	for _, b := range j.Batches {
		if b.Status != "completed" {
			complete = false
		}
	}
	if complete {
		j.Status = "completed"
		j.Approved = false
		j.CleanupPending = true
	}
	if err := store.SaveImportJob(ctx, &j); err != nil {
		return j, err
	}
	if complete {
		if err := store.CleanImportJob(ctx, &j); err != nil {
			return j, err
		}
	}
	return j, nil
}
