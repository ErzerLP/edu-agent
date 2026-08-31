package agentloop

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

const maxPreferenceMetadataConcurrency = 4

type preferenceMetadataResult struct {
	index     int
	candidate api.MemoryCandidateView
	err       error
}

func (s *Session) listLongTermPreferences(ctx context.Context, cursor, activityID string) (any, string) {
	result, err := s.server.ExportMemory(ctx, cursor, 20)
	if ctx.Err() != nil {
		return toolFailure(ctx.Err(), "preferences_unavailable"), "长期偏好读取已取消"
	}
	if err != nil {
		return toolFailure(err, "preferences_unavailable"), "长期偏好不可用"
	}
	degraded := result.Degraded
	reasonCodes := append([]string(nil), result.ReasonCodes...)
	privacyInvalidated := false
	for _, code := range reasonCodes {
		if code == "content_redacted" || code == "privacy_clear_in_progress" {
			privacyInvalidated = true
		}
	}
	now := s.options.Now().UTC()
	items := make([]map[string]any, len(result.Items))
	available := make([]int, 0, len(result.Items))
	processed := 0
	for index, item := range result.Items {
		if item.ContentStatus == "redacted" {
			degraded = true
			privacyInvalidated = true
			reasonCodes = appendUnique(reasonCodes, "content_redacted")
		}
		if item.ContentStatus != "available" || strings.TrimSpace(item.Content) == "" {
			processed++
			continue
		}
		available = append(available, index)
	}
	if len(result.Items) > 0 && processed > 0 {
		s.publishPreferenceProgress(ctx, activityID, processed, len(result.Items))
	}

	workerCount := min(maxPreferenceMetadataConcurrency, len(available))
	if workerCount > 0 {
		var nextMu sync.Mutex
		next := 0
		results := make(chan preferenceMetadataResult, workerCount)
		var workers sync.WaitGroup
		workers.Add(workerCount)
		for worker := 0; worker < workerCount; worker++ {
			go func() {
				defer workers.Done()
				for {
					if ctx.Err() != nil {
						return
					}
					nextMu.Lock()
					if next >= len(available) || ctx.Err() != nil {
						nextMu.Unlock()
						return
					}
					index := available[next]
					next++
					nextMu.Unlock()
					candidate, candidateErr := s.server.MemoryCandidate(ctx, result.Items[index].Record.CandidateID)
					select {
					case results <- preferenceMetadataResult{index: index, candidate: candidate, err: candidateErr}:
					case <-ctx.Done():
						return
					}
				}
			}()
		}
		go func() {
			workers.Wait()
			close(results)
		}()
		for current := range results {
			processed++
			if ctx.Err() == nil {
				if current.err != nil {
					degraded = true
					reasonCodes = appendUnique(reasonCodes, "candidate_metadata_unavailable")
				} else {
					candidate := current.candidate.Candidate
					item := result.Items[current.index]
					if preferenceCategory(candidate.Category) && candidate.ValidUntil.After(now) {
						items[current.index] = map[string]any{
							"memory_id": item.Record.LogicalMemoryID, "revision": item.Record.Revision,
							"category": candidate.Category, "sensitivity": candidate.Sensitivity,
							"stability": candidate.Stability, "valid_until": candidate.ValidUntil,
							"content": item.Content,
						}
					}
				}
				s.publishPreferenceProgress(ctx, activityID, processed, len(result.Items))
			}
		}
	}
	if ctx.Err() != nil {
		return toolFailure(ctx.Err(), "preferences_unavailable"), "长期偏好读取已取消"
	}

	ordered := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if item != nil {
			ordered = append(ordered, item)
		}
	}
	value := map[string]any{
		"items": ordered, "read_generation": result.ReadGeneration,
		"degraded": degraded, "privacy_invalidated": privacyInvalidated,
		"reason_codes": reasonCodes, "has_more": result.NextCursor != "",
	}
	if result.NextCursor != "" {
		value["next_cursor"] = result.NextCursor
	}
	summary := fmt.Sprintf("已读取 %d 条长期偏好", len(ordered))
	if degraded {
		summary += "（部分内容暂不可用）"
	}
	return value, summary
}

func (s *Session) publishPreferenceProgress(ctx context.Context, activityID string, completed, total int) {
	if activityID == "" || total <= 0 {
		return
	}
	s.publishActivity(ctx, Activity{
		Kind:     ActivityTool,
		Event:    Event{ID: activityID, Tool: "list_long_term_preferences", Summary: fmt.Sprintf("已处理 %d/%d 条长期偏好元数据", completed, total), Status: EventRunning},
		Phase:    ActivityExecutingTool,
		Progress: &ActivityProgress{Completed: completed, Total: total},
	})
}
