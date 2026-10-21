package mentorrun

import (
	"bytes"
	"encoding/json"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"time"
)

type SourceResearchCreate struct {
	OperationID string `json:"operation_id"`
	Request     string `json:"request"`
}
type SourceResearchDirection struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Purpose string `json:"purpose"`
	Query   string `json:"query"`
	State   string `json:"state"`
}

// The checkpoint contains only bounded workflow state, never page bodies/PDFs.
type SourceResearchState struct {
	RoundNumber            int                       `json:"round_number"`
	PreviousDirections     []SourceResearchDirection `json:"previous_directions,omitempty"`
	PreviousGaps           []string                  `json:"previous_gaps,omitempty"`
	PendingResource        string                    `json:"pending_resource,omitempty"`
	DirectionReported      bool                      `json:"direction_reported"`
	SectionsAdded          int                       `json:"sections_added"`
	DomainReads            map[string]int            `json:"domain_reads,omitempty"`
	BlockedDomains         map[string]bool           `json:"blocked_domains,omitempty"`
	ResourceDomains        map[string]string         `json:"resource_domains,omitempty"`
	DeferredQueues         map[string][]string       `json:"deferred_queues,omitempty"`
	DirectionReadStart     int                       `json:"direction_read_start"`
	DirectionAllowance     int                       `json:"direction_allowance"`
	DirectionCandidateGoal int                       `json:"direction_candidate_goal"`
	ReadableVersions       map[string]bool           `json:"readable_versions,omitempty"`
	PassReadable           int                       `json:"pass_readable"`
	UnreadLeads            bool                      `json:"unread_leads"`
	ReadAliases            map[string]bool           `json:"read_aliases,omitempty"`
	Request                string                    `json:"request"`
	SearchConfiguration    string                    `json:"search_configuration"`
	Destination            settings.Connection       `json:"destination"`
	Limits                 settings.SourceResearch   `json:"limits"`
	Directions             []SourceResearchDirection `json:"directions"`
	Direction              int                       `json:"direction"`
	Queue                  []string                  `json:"queue,omitempty"`
	SeenQueries            []string                  `json:"seen_queries,omitempty"`
	ReadURLs               map[string]bool           `json:"read_urls"`
	Screened               map[string]bool           `json:"screened"`
	ReportedDocuments      map[string]bool           `json:"reported_documents,omitempty"`
	DirectionDocuments     []string                  `json:"direction_documents,omitempty"`
	ReportRevisionID       string                    `json:"report_revision_id,omitempty"`
	ResultSnapshotID       string                    `json:"result_snapshot_id,omitempty"`
	CandidateCount         int                       `json:"candidate_count"`
	SectionCount           int                       `json:"section_count"`
	PassStart              int                       `json:"pass_start"`
	Gaps                   []string                  `json:"gaps"`
	PendingDocument        string                    `json:"pending_document,omitempty"`
	Searched               bool                      `json:"searched"`
	Finished               bool                      `json:"finished"`
}
type SourceResearchJob struct {
	RoundNumber              int                              `json:"round_number"`
	DestinationChange        *SourceResearchDestinationChange `json:"destination_change,omitempty"`
	ImportPreviewID          string                           `json:"import_preview_id"`
	ID                       string                           `json:"job_id"`
	RoundID                  string                           `json:"round_id"`
	RunID                    string                           `json:"run_id"`
	Version                  int64                            `json:"version"`
	SelectionVersion         int64                            `json:"selection_version"`
	EventSeq                 int64                            `json:"event_seq"`
	Status                   string                           `json:"status"`
	Stage                    string                           `json:"stage"`
	Request                  string                           `json:"request"`
	Limits                   settings.SourceResearch          `json:"limits"`
	Directions               []SourceResearchDirection        `json:"directions"`
	Progress                 WebProgress                      `json:"progress"`
	CandidateCount           int                              `json:"candidate_count"`
	ResultSnapshotID         string                           `json:"result_snapshot_id"`
	ReportRevisionID         string                           `json:"report_revision_id"`
	SelectedReportRevisionID string                           `json:"selected_report_revision_id"`
	AvailableActions         []string                         `json:"available_actions"`
	UsageTaskID              string                           `json:"usage_task_id"`
	ExpiresAt                time.Time                        `json:"expires_at"`
}
type SourceResearchDestinationChange struct {
	Model        settings.Connection `json:"model"`
	Search       settings.Connection `json:"search"`
	Confirmation string              `json:"confirmation"`
}

func sourceResearchModelDestination(v settings.View) settings.Connection {
	if v.MentorUsesTeaching {
		return v.Teaching.Connection
	}
	return v.EffectiveMentor.Connection
}

type SourceResearchCandidate struct {
	RoundID               string                    `json:"round_id"`
	Support               *research.WebCitation     `json:"support,omitempty"`
	ID                    string                    `json:"id"`
	DocumentID            string                    `json:"document_id"`
	RevisionID            string                    `json:"document_revision_id"`
	Title                 string                    `json:"title"`
	URL                   string                    `json:"url"`
	MediaType             string                    `json:"media_type"`
	Reason                string                    `json:"reason"`
	QualityNotes          string                    `json:"quality_notes"`
	Coverage              string                    `json:"coverage"`
	Selected              bool                      `json:"selected"`
	ExplicitlyChanged     bool                      `json:"explicitly_changed"`
	RequiresPartialAccept bool                      `json:"requires_partial_accept"`
	PhysicalPages         int                       `json:"physical_pages"`
	Source                *knowledge.SourceIdentity `json:"source,omitempty"`
}
type SourceResearchCandidatePage struct {
	Items      []SourceResearchCandidate `json:"items"`
	NextCursor string                    `json:"next_cursor,omitempty"`
}
type sourceResearchCandidateRecord struct {
	SourceResearchCandidate
	Preexisting bool `json:"preexisting"`
}
type SourceResearchSelectionItem struct {
	CandidateID string `json:"candidate_id"`
	Selected    bool   `json:"selected"`
}

func (c *SourceResearchSelectionItem) UnmarshalJSON(raw []byte) error {
	var value struct {
		CandidateID string `json:"candidate_id"`
		Selected    *bool  `json:"selected"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	if value.Selected == nil {
		return ErrInvalid
	}
	c.CandidateID = value.CandidateID
	c.Selected = *value.Selected
	return nil
}

type SourceResearchSelectionCommand struct {
	ReportChanged    bool                          `json:"-"`
	OperationID      string                        `json:"operation_id"`
	ExpectedVersion  int64                         `json:"expected_selection_version"`
	Items            []SourceResearchSelectionItem `json:"items"`
	ReportRevisionID *string                       `json:"report_revision_id"`
}

func (c *SourceResearchSelectionCommand) UnmarshalJSON(raw []byte) error {
	type plain SourceResearchSelectionCommand
	var value plain
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	*c = SourceResearchSelectionCommand(value)
	_, c.ReportChanged = fields["report_revision_id"]
	return nil
}

type sourceResearchSelection struct {
	Selected          bool `json:"selected"`
	ExplicitlyChanged bool `json:"explicitly_changed"`
}
type SourceResearchReportSection struct {
	ID        string           `json:"id"`
	Ordinal   int              `json:"ordinal"`
	Title     string           `json:"title"`
	Body      string           `json:"body"`
	Citations []SourceCitation `json:"citations"`
	Gaps      []string         `json:"gaps"`
}
type SourceResearchReport struct {
	RoundID         string                        `json:"round_id"`
	Version         int64                         `json:"version"`
	BasedOnRevision string                        `json:"based_on_revision"`
	ID              string                        `json:"revision_id"`
	Status          string                        `json:"status"`
	Sections        []SourceResearchReportSection `json:"sections"`
	Gaps            []string                      `json:"gaps"`
	NextOrdinal     int                           `json:"next_ordinal"`
	Model           string                        `json:"model"`
	CreatedAt       time.Time                     `json:"created_at"`
}
type sourceResearchResult struct {
	NewCandidateCount int      `json:"new_candidate_count"`
	CandidateIDs      []string `json:"candidate_ids"`
	ReportRevisionID  string   `json:"report_revision_id"`
	Status            string   `json:"status"`
}
type SourceResearchEvent struct {
	Seq            int64       `json:"seq"`
	Type           string      `json:"type"`
	Stage          string      `json:"stage"`
	Status         string      `json:"status"`
	Progress       WebProgress `json:"progress"`
	CandidateCount int         `json:"candidate_count"`
	CreatedAt      time.Time   `json:"created_at"`
}
type SourceResearchCommand struct {
	OperationID        string `json:"operation_id"`
	ExpectedVersion    int64  `json:"expected_version"`
	RoundID            string `json:"round_id"`
	Kind               string `json:"kind"`
	ConfirmDestination string `json:"confirm_destination,omitempty"`
}
type SourceResearchMore struct {
	OperationID        string `json:"operation_id"`
	ExpectedVersion    int64  `json:"expected_version"`
	AdditionalRequest  string `json:"additional_request,omitempty"`
	ConfirmDestination string `json:"confirm_destination,omitempty"`
}
type SourceResearchPreviewRequest struct {
	OperationID              string `json:"operation_id"`
	ResultSnapshotID         string `json:"result_snapshot_id"`
	ExpectedSelectionVersion int64  `json:"expected_selection_version"`
}
type SourceResearchPreviewItem struct {
	ID                    string                    `json:"item_id"`
	Kind                  string                    `json:"kind"`
	OperationID           string                    `json:"operation_id"`
	WebPreviewID          string                    `json:"web_preview_id,omitempty"`
	RevisionID            string                    `json:"document_revision_id,omitempty"`
	Title                 string                    `json:"title"`
	Coverage              string                    `json:"coverage"`
	RequiresPartialAccept bool                      `json:"requires_partial_accept"`
	AlreadyImported       bool                      `json:"already_imported"`
	Source                *knowledge.SourceIdentity `json:"source,omitempty"`
}
type SourceResearchPreview struct {
	ID               string                      `json:"preview_id"`
	Version          int64                       `json:"preview_version"`
	ExpiresAt        time.Time                   `json:"expires_at"`
	ResultSnapshotID string                      `json:"result_snapshot_id"`
	Items            []SourceResearchPreviewItem `json:"items"`
	ReportRevisionID string                      `json:"report_revision_id"`
}
type SourceResearchReportImport struct {
	OperationID      string `json:"operation_id"`
	PreviewID        string `json:"preview_id"`
	ExpectedVersion  int64  `json:"expected_preview_version"`
	ReportRevisionID string `json:"report_revision_id"`
	StorageConsent   bool   `json:"storage_consent"`
	AcceptPartial    bool   `json:"accept_partial"`
}

func webOwner(item row) (string, string) {
	if item.SourceResearchJobID != "" {
		return "source_research", item.SourceResearchJobID
	}
	return "mentor_run", item.RunID
}
func webCancelKey(item row) string {
	if item.SourceResearchJobID != "" {
		return item.SourceResearchJobID
	}
	return item.ConversationID
}
func webOwnerID(item row) string { _, id := webOwner(item); return id }
func sourceResearchStatus(item row) string {
	switch item.Status {
	case "succeeded":
		return "completed"
	case "cancelled":
		return "stopped"
	case "cancelling":
		return "stopping"
	case "partial", "failed":
		if item.ResultUnknown {
			return "outcome_unknown"
		}
		return "failed"
	default:
		return item.Status
	}
}
func sourceResearchActive(status string) bool {
	return status == "queued" || status == "running" || status == "stopping"
}
