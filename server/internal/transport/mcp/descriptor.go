package mcp

import (
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type DescriptorKind string

const (
	DescriptorResource         DescriptorKind = "resource"
	DescriptorResourceTemplate DescriptorKind = "resource_template"
	DescriptorTool             DescriptorKind = "tool"
)

type Descriptor struct {
	Kind            DescriptorKind
	Name            string
	URI             string
	URITemplate     string
	Description     string
	RequiredScope   string
	PrivacyOwners   []privacy.OwnerKind
	ReadOnly        bool
	InputLimit      int64
	OutputLimit     int64
	AuditName       string
	HTTPOperationID string
	InputSchema     any
	OutputSchema    any
}

const (
	defaultToolInputLimit  = int64(256 << 10)
	learningToolInputLimit = int64(1 << 20)
	defaultOutputLimit     = int64(4 << 20)
	exportOutputLimit      = int64(16 << 20)
)

var descriptorCatalog = []Descriptor{
	{Kind: DescriptorResource, Name: "knowledge.head", URI: "edu-agent://knowledge/head", Description: "Current canonical knowledge revision", RequiredScope: "knowledge:read", PrivacyOwners: []privacy.OwnerKind{privacy.OwnerKnowledge}, ReadOnly: true, OutputLimit: defaultOutputLimit, AuditName: "knowledge_head", HTTPOperationID: "getKnowledgeHead"},
	{Kind: DescriptorResourceTemplate, Name: "knowledge.revision_tree", URITemplate: "edu-agent://knowledge/revisions/{revision_id}/tree", Description: "Deterministic tree for one knowledge revision", RequiredScope: "knowledge:read", PrivacyOwners: []privacy.OwnerKind{privacy.OwnerKnowledge}, ReadOnly: true, OutputLimit: exportOutputLimit, AuditName: "knowledge_tree", HTTPOperationID: "getKnowledgeRevisionTree"},
	{Kind: DescriptorResourceTemplate, Name: "knowledge.revision_export", URITemplate: "edu-agent://knowledge/revisions/{revision_id}/export", Description: "Canonical Markdown export for one knowledge revision", RequiredScope: "knowledge:read", PrivacyOwners: []privacy.OwnerKind{privacy.OwnerKnowledge}, ReadOnly: true, OutputLimit: exportOutputLimit, AuditName: "knowledge_export", HTTPOperationID: "exportKnowledgeRevision"},
	{Kind: DescriptorResource, Name: "tutoring.current_session", URI: "edu-agent://tutoring/sessions/current", Description: "Current tutoring session and projection", RequiredScope: "learning:read", PrivacyOwners: learningOwners(), ReadOnly: true, OutputLimit: defaultOutputLimit, AuditName: "tutoring_current_session", HTTPOperationID: "getCurrentTutoringSession"},
	{Kind: DescriptorResourceTemplate, Name: "tutoring.session", URITemplate: "edu-agent://tutoring/sessions/{session_id}", Description: "One tutoring session and projection", RequiredScope: "learning:read", PrivacyOwners: learningOwners(), ReadOnly: true, OutputLimit: defaultOutputLimit, AuditName: "tutoring_session", HTTPOperationID: "getTutoringSession"},
	{Kind: DescriptorResourceTemplate, Name: "learning.node", URITemplate: "edu-agent://learning/nodes/{node_revision_id}", Description: "Learner projection for one knowledge node revision", RequiredScope: "learning:read", PrivacyOwners: learningOwners(), ReadOnly: true, OutputLimit: defaultOutputLimit, AuditName: "learning_node", HTTPOperationID: "getLearningNode"},
	{Kind: DescriptorResource, Name: "learning.projection_status", URI: "edu-agent://learning/projections/status", Description: "Learning projection status and event high water", RequiredScope: "learning:read", PrivacyOwners: learningOwners(), ReadOnly: true, OutputLimit: defaultOutputLimit, AuditName: "learning_projection_status", HTTPOperationID: "getLearningProjectionStatus"},
	{Kind: DescriptorResourceTemplate, Name: "memory.record", URITemplate: "edu-agent://memory/records/{memory_id}", Description: "One admitted memory record loaded from the composed memory exporter", RequiredScope: "memory:read", PrivacyOwners: []privacy.OwnerKind{privacy.OwnerMemory}, ReadOnly: true, OutputLimit: defaultOutputLimit, AuditName: "memory_record", HTTPOperationID: "getMemoryRecord"},
	{Kind: DescriptorResource, Name: "memory.export", URI: "edu-agent://memory/export", Description: "First page of the composed memory export", RequiredScope: "memory:read", PrivacyOwners: []privacy.OwnerKind{privacy.OwnerMemory}, ReadOnly: true, OutputLimit: exportOutputLimit, AuditName: "memory_export", HTTPOperationID: "exportMemoryRecords"},

	toolDescriptor("knowledge.retrieve", "Retrieve canonical knowledge with frozen revision provenance", "knowledge:read", []privacy.OwnerKind{privacy.OwnerKnowledge}, true, defaultToolInputLimit, exportOutputLimit, "knowledge_retrieve", "retrieveKnowledge", knowledgeRetrieveSchema(), nil),
	toolDescriptor("knowledge.maintenance.propose", "Submit a sourced knowledge maintenance proposal", "knowledge:write", []privacy.OwnerKind{privacy.OwnerKnowledge}, false, exportOutputLimit, exportOutputLimit, "knowledge_maintenance_propose", "createKnowledgeMaintenanceProposal", knowledgeMaintenanceProposalSchema(), knowledgeMaintenanceProposalOutputSchema()),
	toolDescriptor("knowledge.maintenance.list", "List knowledge maintenance proposals", "knowledge:read", []privacy.OwnerKind{privacy.OwnerKnowledge}, true, defaultToolInputLimit, exportOutputLimit, "knowledge_maintenance_list", "listKnowledgeMaintenanceProposals", knowledgeMaintenanceListSchema(), knowledgeMaintenancePageOutputSchema()),
	toolDescriptor("knowledge.maintenance.get", "Read one knowledge maintenance proposal", "knowledge:read", []privacy.OwnerKind{privacy.OwnerKnowledge}, true, defaultToolInputLimit, exportOutputLimit, "knowledge_maintenance_get", "getKnowledgeMaintenanceProposal", knowledgeMaintenanceGetSchema(), knowledgeMaintenanceProposalOutputSchema()),
	toolDescriptor("learning.evidence_carryover.list", "List evidence carryover proposals", "learning:read", evidenceCarryoverOwners(), true, defaultToolInputLimit, defaultOutputLimit, "learning_evidence_carryover_list", "listEvidenceCarryovers", evidenceCarryoverListSchema(), evidenceCarryoverPageOutputSchema()),
	toolDescriptor("learning.evidence_carryover.get", "Read one evidence carryover proposal", "learning:read", evidenceCarryoverOwners(), true, defaultToolInputLimit, defaultOutputLimit, "learning_evidence_carryover_get", "getEvidenceCarryover", evidenceCarryoverGetSchema(), evidenceCarryoverProposalOutputSchema()),
	toolDescriptor("learning.list_timeline", "List learning timeline projection entries", "learning:read", learningOwners(), true, defaultToolInputLimit, defaultOutputLimit, "learning_list_timeline", "listLearningTimeline", timelineSchema(), nil),
	toolDescriptor("learning.list_routes", "List current or historical learning routes", "learning:read", learningOwners(), true, defaultToolInputLimit, defaultOutputLimit, "learning_list_routes", "listLearningRoutes", routesSchema(), nil),
	toolDescriptor("learning.list_evidence", "List accepted learning evidence", "learning:read", learningOwners(), true, defaultToolInputLimit, defaultOutputLimit, "learning_list_evidence", "listLearningEvidence", evidenceSchema(), nil),
	toolDescriptor("learning.list_reviews", "List scheduled learning reviews", "learning:read", learningOwners(), true, defaultToolInputLimit, defaultOutputLimit, "learning_list_reviews", "listLearningReviews", reviewsSchema(), nil),
	toolDescriptor("memory.list_records", "List admitted memory record metadata", "memory:read", []privacy.OwnerKind{privacy.OwnerMemory}, true, defaultToolInputLimit, defaultOutputLimit, "memory_list_records", "listMemoryRecords", pageSchema(), nil),
	toolDescriptor("learning.create_goal", "Create a goal revision through the learning application service", "learning:write", learningOwners(), false, learningToolInputLimit, defaultOutputLimit, "learning_create_goal", "createLearningGoal", createGoalSchema(), nil),
	toolDescriptor("tutoring.create_session", "Create a tutoring session through the learning application service", "learning:write", learningOwners(), false, learningToolInputLimit, defaultOutputLimit, "tutoring_create_session", "createTutoringSession", createSessionSchema(), nil),
	toolDescriptor("tutoring.propose", "Request a tutoring proposal from the composed tutor model path", "learning:write", learningOwners(), false, learningToolInputLimit, exportOutputLimit, "tutoring_propose", "proposeTutoringArtifact", proposeSchema(), nil),
	toolDescriptor("tutoring.apply_action", "Apply one existing tutoring state-machine action", "learning:write", learningOwners(), false, learningToolInputLimit, defaultOutputLimit, "tutoring_apply_action", "applyTutoringAction", applyActionSchema(), nil),
}

func toolDescriptor(name, description, scope string, owners []privacy.OwnerKind, readOnly bool, inputLimit, outputLimit int64, auditName, operationID string, inputSchema, outputSchema any) Descriptor {
	return Descriptor{Kind: DescriptorTool, Name: name, Description: description, RequiredScope: scope, PrivacyOwners: owners, ReadOnly: readOnly, InputLimit: inputLimit, OutputLimit: outputLimit, AuditName: auditName, HTTPOperationID: operationID, InputSchema: inputSchema, OutputSchema: outputSchema}
}

func learningOwners() []privacy.OwnerKind {
	return []privacy.OwnerKind{privacy.OwnerLearning, privacy.OwnerTutoring}
}

func evidenceCarryoverOwners() []privacy.OwnerKind {
	return []privacy.OwnerKind{privacy.OwnerKnowledge, privacy.OwnerLearning}
}

func Catalog() []Descriptor {
	result := make([]Descriptor, len(descriptorCatalog))
	copy(result, descriptorCatalog)
	for index := range result {
		result[index].PrivacyOwners = append([]privacy.OwnerKind(nil), result[index].PrivacyOwners...)
	}
	return result
}

func toolDefinition(descriptor Descriptor) *sdkmcp.Tool {
	openWorld := false
	destructive := false
	return &sdkmcp.Tool{
		Name: descriptor.Name, Description: descriptor.Description, InputSchema: descriptor.InputSchema,
		OutputSchema: descriptor.OutputSchema,
		Annotations: &sdkmcp.ToolAnnotations{
			ReadOnlyHint: descriptor.ReadOnly, IdempotentHint: true,
			OpenWorldHint: &openWorld, DestructiveHint: &destructive,
		},
	}
}

func resourceDefinition(descriptor Descriptor) *sdkmcp.Resource {
	return &sdkmcp.Resource{Name: descriptor.Name, URI: descriptor.URI, MIMEType: "application/json", Description: descriptor.Description}
}

func resourceTemplateDefinition(descriptor Descriptor) *sdkmcp.ResourceTemplate {
	return &sdkmcp.ResourceTemplate{Name: descriptor.Name, URITemplate: descriptor.URITemplate, MIMEType: "application/json", Description: descriptor.Description}
}

func objectSchema(properties map[string]any, required ...string) map[string]any {
	result := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) != 0 {
		result["required"] = required
	}
	return result
}

func stringProperty() map[string]any { return map[string]any{"type": "string"} }
func uuidProperty() map[string]any   { return map[string]any{"type": "string", "format": "uuid"} }
func integerProperty(minimum, maximum int) map[string]any {
	return map[string]any{"type": "integer", "minimum": minimum, "maximum": maximum}
}

func pageProperties() map[string]any {
	return map[string]any{"cursor": stringProperty(), "limit": integerProperty(1, 200)}
}

func operationProperties() map[string]any {
	return map[string]any{
		"operation_id": uuidProperty(), "payload_schema_version": map[string]any{"type": "integer", "const": 1},
		"aggregate_type": stringProperty(), "aggregate_id": uuidProperty(),
		"expected_version": map[string]any{"type": "integer", "minimum": 0},
		"occurred_at":      map[string]any{"type": "string", "format": "date-time"},
	}
}

func mergeProperties(left, right map[string]any) map[string]any {
	result := make(map[string]any, len(left)+len(right))
	for key, value := range left {
		result[key] = value
	}
	for key, value := range right {
		result[key] = value
	}
	return result
}

func knowledgeMaintenanceProposalSchema() any {
	source := objectSchema(map[string]any{
		"kind": stringProperty(), "locator": stringProperty(), "title": stringProperty(),
		"excerpt": stringProperty(), "sha256": map[string]any{"type": "string", "pattern": "^[0-9a-f]{64}$"},
	}, "kind", "locator", "sha256")
	document := objectSchema(map[string]any{"path": stringProperty(), "markdown": stringProperty()}, "path", "markdown")
	documentResolution := objectSchema(map[string]any{
		"locator": stringProperty(), "action": map[string]any{"type": "string", "enum": []string{"preserve", "new"}},
		"document_id": uuidProperty(), "reason": stringProperty(),
	}, "locator", "action", "reason")
	nodeResolution := objectSchema(map[string]any{
		"locator": stringProperty(), "action": map[string]any{"type": "string", "enum": []string{"preserve", "new", "rewrite", "split", "merge"}},
		"source_node_revision_ids": map[string]any{"type": "array", "items": uuidProperty(), "uniqueItems": true},
		"reason":                   stringProperty(),
	}, "locator", "action", "reason")
	return objectSchema(map[string]any{
		"request_id": uuidProperty(), "base_revision_id": uuidProperty(),
		"sources":                      map[string]any{"type": "array", "minItems": 1, "maxItems": 20, "items": source},
		"candidate_snapshot":           map[string]any{"type": "array", "maxItems": 1000, "items": document},
		"identity_review_basis_hash":   map[string]any{"type": "string", "pattern": "^[0-9a-f]{64}$"},
		"identity_review_operation_id": uuidProperty(),
		"identity_review_receipt":      map[string]any{"type": "string", "pattern": "^[0-9a-f]{64}$"},
		"document_resolutions":         map[string]any{"type": "array", "items": documentResolution},
		"node_resolutions":             map[string]any{"type": "array", "items": nodeResolution},
	}, "request_id", "base_revision_id", "sources", "candidate_snapshot")
}

func knowledgeMaintenanceListSchema() any {
	return objectSchema(map[string]any{
		"status": map[string]any{"type": "string", "enum": []string{"all", "open", "applied", "rejected", "stale", "redacted"}},
		"cursor": stringProperty(), "limit": integerProperty(1, 100),
	})
}

func knowledgeMaintenanceGetSchema() any {
	return objectSchema(map[string]any{"proposal_id": uuidProperty()}, "proposal_id")
}

func knowledgeMaintenanceProposalOutputSchema() any {
	object := map[string]any{"type": "object"}
	array := map[string]any{"type": "array"}
	return objectSchema(map[string]any{
		"proposal_id": uuidProperty(), "request_id": uuidProperty(),
		"kind":             map[string]any{"type": "string", "enum": []string{"candidate", "rollback"}},
		"status":           map[string]any{"type": "string", "enum": []string{"open", "applied", "rejected", "stale", "redacted"}},
		"base_revision_id": uuidProperty(), "current_revision_id": uuidProperty(), "rollback_target_revision_id": uuidProperty(),
		"sources": array, "candidate_snapshot": array, "diff": array,
		"identity_impact": object, "lineage_impact": object, "accepted_learning_evidence_impact": object,
		"risk": object, "basis_hash": stringProperty(), "knowledge_generation": map[string]any{"type": "integer", "minimum": 1},
		"canonicalizer_version": stringProperty(), "identity_policy_version": stringProperty(), "diff_version": stringProperty(),
		"risk_version": stringProperty(), "auto_apply_policy_version": stringProperty(), "decision": object,
		"applied_revision_id": uuidProperty(), "origin": object, "created_by_device_id": uuidProperty(),
		"created_at": map[string]any{"type": "string", "format": "date-time"},
		"updated_at": map[string]any{"type": "string", "format": "date-time"},
		"redacted":   map[string]any{"type": "boolean"}, "replayed": map[string]any{"type": "boolean"},
	}, "proposal_id", "request_id", "kind", "status", "base_revision_id", "identity_impact", "lineage_impact",
		"accepted_learning_evidence_impact", "risk", "knowledge_generation", "canonicalizer_version", "identity_policy_version",
		"diff_version", "risk_version", "auto_apply_policy_version", "created_by_device_id", "created_at", "updated_at", "redacted")
}

func knowledgeMaintenancePageOutputSchema() any {
	return objectSchema(map[string]any{
		"items":       map[string]any{"type": "array", "items": knowledgeMaintenanceProposalOutputSchema()},
		"next_cursor": stringProperty(),
	}, "items")
}

func evidenceCarryoverListSchema() any {
	return objectSchema(map[string]any{
		"status": map[string]any{"type": "string", "enum": []string{"all", "open", "approved", "rejected", "stale", "redacted"}},
		"cursor": map[string]any{"type": "string", "maxLength": 4096}, "limit": integerProperty(1, 100),
	})
}

func evidenceCarryoverGetSchema() any {
	return objectSchema(map[string]any{"proposal_id": uuidProperty()}, "proposal_id")
}

func evidenceCarryoverProposalOutputSchema() any {
	hash := map[string]any{"type": "string", "pattern": "^[0-9a-f]{64}$"}
	dateTime := map[string]any{"type": "string", "format": "date-time"}
	candidate := objectSchema(map[string]any{
		"knowledge_revision_id": uuidProperty(), "node_id": uuidProperty(),
		"node_revision_id": uuidProperty(), "document_revision_id": uuidProperty(),
	}, "knowledge_revision_id", "node_id", "node_revision_id", "document_revision_id")
	decision := objectSchema(map[string]any{
		"decision_id": uuidProperty(), "operation_id": uuidProperty(),
		"requested_decision": map[string]any{"type": "string", "enum": []string{"approve", "reject"}},
		"outcome":            map[string]any{"type": "string", "enum": []string{"approved", "rejected", "stale"}},
		"reason":             map[string]any{"type": "string", "maxLength": 4000}, "actor_device_id": uuidProperty(),
		"event_id": uuidProperty(), "event_seq": map[string]any{"type": "integer", "minimum": 1}, "created_at": dateTime,
	}, "decision_id", "operation_id", "requested_decision", "outcome", "actor_device_id", "event_id", "event_seq", "created_at")
	link := objectSchema(map[string]any{
		"link_id": uuidProperty(), "proposal_id": uuidProperty(), "source_evidence_id": uuidProperty(),
		"target_knowledge_revision_id": uuidProperty(), "target_node_id": uuidProperty(),
		"target_node_revision_id": uuidProperty(), "target_document_revision_id": uuidProperty(),
		"decision_id": uuidProperty(), "event_id": uuidProperty(),
		"event_seq": map[string]any{"type": "integer", "minimum": 1}, "created_at": dateTime,
	}, "link_id", "proposal_id", "decision_id", "event_id", "event_seq", "created_at")
	return objectSchema(map[string]any{
		"proposal_id": uuidProperty(), "knowledge_proposal_id": uuidProperty(),
		"status":             map[string]any{"type": "string", "enum": []string{"open", "approved", "rejected", "stale", "redacted"}},
		"source_evidence_id": uuidProperty(), "source_knowledge_revision_id": uuidProperty(),
		"source_node_revision_id": uuidProperty(), "target_knowledge_revision_id": uuidProperty(),
		"candidates":           map[string]any{"type": "array", "items": candidate},
		"knowledge_basis_hash": hash, "accepted_evidence_fingerprint": hash,
		"candidate_fingerprint": hash, "basis_fingerprint": hash,
		"knowledge_generation": map[string]any{"type": "integer", "minimum": 1},
		"learning_generation":  map[string]any{"type": "integer", "minimum": 1},
		"policy_version":       map[string]any{"type": "string", "const": "evidence-carryover-v1"},
		"decision":             decision, "links": map[string]any{"type": "array", "items": link},
		"created_at": dateTime, "updated_at": dateTime,
		"redacted": map[string]any{"type": "boolean"}, "replayed": map[string]any{"type": "boolean"},
	}, "proposal_id", "knowledge_proposal_id", "status", "knowledge_generation", "learning_generation",
		"policy_version", "created_at", "updated_at", "redacted")
}

func evidenceCarryoverPageOutputSchema() any {
	return objectSchema(map[string]any{
		"items":       map[string]any{"type": "array", "items": evidenceCarryoverProposalOutputSchema()},
		"next_cursor": map[string]any{"type": "string", "minLength": 1, "maxLength": 4096},
	}, "items")
}

func knowledgeRetrieveSchema() any {
	return objectSchema(map[string]any{
		"query": stringProperty(), "knowledge_revision_id": uuidProperty(),
		"query_context_schema_version": stringProperty(), "context": map[string]any{"type": "object"},
		"limits": objectSchema(map[string]any{
			"max_depth": integerProperty(1, 8), "candidates_per_layer": integerProperty(1, 20),
			"max_hits": integerProperty(1, 10), "total_candidates": integerProperty(1, 200),
		}),
	}, "query")
}

func pageSchema() any { return objectSchema(pageProperties()) }
func timelineSchema() any {
	return objectSchema(mergeProperties(pageProperties(), map[string]any{"session_id": uuidProperty()}))
}
func routesSchema() any {
	return objectSchema(mergeProperties(pageProperties(), map[string]any{"current_only": map[string]any{"type": "boolean"}}))
}
func evidenceSchema() any {
	return objectSchema(mergeProperties(pageProperties(), map[string]any{"node_revision_id": uuidProperty()}))
}
func reviewsSchema() any {
	return objectSchema(mergeProperties(pageProperties(), map[string]any{"due_before": map[string]any{"type": "string", "format": "date-time"}}))
}

func createGoalSchema() any {
	return objectSchema(mergeProperties(operationProperties(), map[string]any{
		"goal_id": uuidProperty(), "text": stringProperty(), "source": stringProperty(), "previous_revision_id": uuidProperty(),
	}), "operation_id", "payload_schema_version", "aggregate_type", "aggregate_id", "expected_version", "text", "source")
}

func createSessionSchema() any {
	return objectSchema(mergeProperties(operationProperties(), map[string]any{"goal_revision_id": uuidProperty()}),
		"operation_id", "payload_schema_version", "aggregate_type", "aggregate_id", "expected_version", "goal_revision_id")
}

func proposeSchema() any {
	return objectSchema(map[string]any{
		"request_id": uuidProperty(), "proposal_type": stringProperty(), "aggregate_type": stringProperty(),
		"aggregate_id": uuidProperty(), "aggregate_version": map[string]any{"type": "integer", "minimum": 0},
		"goal_revision_id": uuidProperty(), "route_revision_id": uuidProperty(), "route_step_id": uuidProperty(),
		"focus_node_revision_id": uuidProperty(), "activity_id": uuidProperty(), "attempt_id": uuidProperty(),
		"free_question_id": uuidProperty(), "free_answer_id": uuidProperty(), "focus_frame_id": uuidProperty(),
		"tutoring_state": stringProperty(), "knowledge_revision_id": uuidProperty(),
		"node_revision_ids": map[string]any{"type": "array", "items": uuidProperty(), "minItems": 1, "maxItems": 100},
		"input":             map[string]any{"type": "object"},
	}, "request_id", "proposal_type", "aggregate_type", "aggregate_id", "aggregate_version", "knowledge_revision_id", "node_revision_ids", "input")
}

func applyActionSchema() any {
	knowledgeReference := objectSchema(map[string]any{
		"knowledge_revision_id": uuidProperty(), "node_id": uuidProperty(), "node_revision_id": uuidProperty(),
		"document_revision_id": uuidProperty(), "range": objectSchema(map[string]any{
			"start": map[string]any{"type": "integer", "minimum": 0}, "end": map[string]any{"type": "integer", "minimum": 1},
		}, "start", "end"), "slice": stringProperty(), "slice_sha256": stringProperty(),
	}, "node_revision_id")
	return objectSchema(mergeProperties(operationProperties(), map[string]any{
		"session_id": uuidProperty(), "action": stringProperty(), "proposal_id": uuidProperty(),
		"question": stringProperty(), "answer": stringProperty(), "help": stringProperty(),
		"goal_revision_id": uuidProperty(), "exposure_kind": stringProperty(), "exposure_text": stringProperty(),
		"knowledge_references": map[string]any{"type": "array", "items": knowledgeReference, "maxItems": 100},
		"question_id":          uuidProperty(), "answer_id": uuidProperty(),
	}), "session_id", "operation_id", "payload_schema_version", "aggregate_type", "aggregate_id", "expected_version", "action")
}
