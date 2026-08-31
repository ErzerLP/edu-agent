package agentloop

import (
	"fmt"
	"strings"
)

func (r *ContextRuntime) recallMemory(memoryID string) map[string]any {
	if !validOpaqueID(memoryID, "obs_") && !validOpaqueID(memoryID, "ref_") {
		return map[string]any{"error": "memory_not_found", "code": ContextMemoryNotFound, "memory_id": memoryID}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return map[string]any{"error": "session_closed", "code": ContextMemoryNotFound, "memory_id": memoryID}
	}
	if strings.HasPrefix(memoryID, "obs_") {
		observation, exists := r.ledger.Observations[memoryID]
		if !exists {
			return map[string]any{"error": "memory_not_found", "code": ContextMemoryNotFound, "memory_id": memoryID}
		}
		value, unavailable := r.recallObservationLocked(observation)
		if unavailable {
			value["error"] = "source_unavailable"
			value["code"] = ContextSourceUnavailable
		}
		return value
	}
	reflection, exists := r.ledger.Reflections[memoryID]
	if !exists {
		return map[string]any{"error": "memory_not_found", "code": ContextMemoryNotFound, "memory_id": memoryID}
	}
	value := map[string]any{
		"memory_id":   reflection.ID,
		"memory_type": "reflection",
		"status":      memoryFreshnessStatus(reflection.Freshness, false),
		"kind":        reflection.Kind,
		"authority":   reflection.Authority,
		"freshness":   reflection.Freshness,
		"created_at":  reflection.CreatedAt,
	}
	if reflection.Freshness != FreshnessInvalidated {
		value["content"] = sanitizeContextEvidence(reflection.Content)
	}
	support := make([]map[string]any, 0, len(reflection.Support))
	unavailable := reflection.Freshness == FreshnessInvalidated
	for _, edge := range reflection.Support {
		entry := map[string]any{"observation_id": edge.ObservationID, "fidelity": edge.Fidelity}
		if observation, ok := r.ledger.Observations[edge.ObservationID]; ok {
			if reflection.Freshness == FreshnessInvalidated {
				_, dropped := r.ledger.Tombstones[observation.ID]
				entry["observation"] = map[string]any{
					"memory_id": observation.ID,
					"status":    memoryFreshnessStatus(observation.Freshness, dropped),
				}
			} else {
				recalled, missing := r.recallObservationLocked(observation)
				entry["observation"] = recalled
				unavailable = unavailable || missing
			}
		} else {
			entry["observation"] = map[string]any{"memory_id": edge.ObservationID, "status": "unknown"}
			unavailable = true
		}
		support = append(support, entry)
	}
	value["support"] = support
	if unavailable {
		value["error"] = "source_unavailable"
		value["code"] = ContextSourceUnavailable
	}
	return value
}

func (r *ContextRuntime) recallObservationLocked(observation Observation) (map[string]any, bool) {
	tombstone, dropped := r.ledger.Tombstones[observation.ID]
	value := map[string]any{
		"memory_id":   observation.ID,
		"memory_type": "observation",
		"status":      memoryFreshnessStatus(observation.Freshness, dropped),
		"kind":        observation.Kind,
		"authority":   observation.Authority,
		"freshness":   observation.Freshness,
		"created_at":  observation.CreatedAt,
	}
	if observation.Freshness != FreshnessInvalidated {
		value["content"] = sanitizeContextEvidence(observation.Content)
	}
	if dropped {
		value["tombstone_reason"] = tombstone.Reason
		value["dropped_at"] = tombstone.DroppedAt
	}
	sources := make([]map[string]any, 0, len(observation.SourceEntryIDs))
	unavailable := observation.Freshness == FreshnessInvalidated
	for _, sourceID := range observation.SourceEntryIDs {
		source, exists := r.ledger.Sources[sourceID]
		if !exists {
			sources = append(sources, map[string]any{"source_id": sourceID, "available": false})
			unavailable = true
			continue
		}
		available := source.SourceAvailable && source.Freshness != FreshnessInvalidated && strings.TrimSpace(source.RecallText) != ""
		metadata := map[string]any{
			"source_id": source.ID, "turn_id": source.TurnID, "kind": source.Kind,
			"created_at": source.CreatedAt, "retention": source.Retention,
			"authority": source.Authority, "freshness": source.Freshness,
			"available": available, "content_hash": source.ContentHash,
		}
		if source.ServerReference != nil {
			metadata["server_reference"] = cloneServerReference(source.ServerReference)
		}
		if available {
			metadata["recall_text"] = truncateUTF8(sanitizeContextEvidence(source.RecallText), maxContextSourceRecallBytes)
		} else {
			unavailable = true
		}
		sources = append(sources, metadata)
	}
	value["sources"] = sources
	return value, unavailable
}

func memoryFreshnessStatus(freshness FreshnessClass, dropped bool) string {
	if freshness == FreshnessInvalidated {
		return "invalidated"
	}
	if dropped {
		return "dropped"
	}
	return "active"
}

func recallSummary(value any) string {
	if toolResultCode(value) == ContextSourceUnavailable {
		return "会话证据来源已不可用"
	}
	if toolResultCode(value) != "" {
		return "未找到指定会话记忆"
	}
	object := normalizedProjectionObject(value)
	if object == nil {
		return "已回查会话证据"
	}
	return fmt.Sprintf("已回查%s会话证据", object["memory_type"])
}
