package agentui

import (
	"fmt"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
)

type sessionGenerationProvider interface {
	Generation() uint64
}

type sessionTranscriptProvider interface {
	SessionTranscript() agentsession.TranscriptV1
}

type sessionTitleProvider interface {
	SessionTitle() string
}

type contextSubscriptionProvider interface {
	SubscribeContextUpdates() (<-chan agentloop.ContextEvent, func())
}

func sessionGeneration(session Conversation) uint64 {
	if provider, ok := session.(sessionGenerationProvider); ok {
		if generation := provider.Generation(); generation > 0 {
			return generation
		}
	}
	return 1
}

func subscribeSessionContext(session Conversation) (<-chan agentloop.ContextEvent, func()) {
	if provider, ok := session.(contextSubscriptionProvider); ok {
		return provider.SubscribeContextUpdates()
	}
	return session.ContextUpdates(), func() {}
}

func durableTranscriptEntries(session Conversation) []transcriptEntry {
	provider, ok := session.(sessionTranscriptProvider)
	if !ok {
		return nil
	}
	value := provider.SessionTranscript()
	result := make([]transcriptEntry, 0, len(value.Entries))
	for _, entry := range value.Entries {
		switch entry.Kind {
		case agentsession.TranscriptKindUser:
			result = append(result, transcriptEntry{kind: entryUser, text: entry.Text, turnID: entry.PresentationTurn})
		case agentsession.TranscriptKindAssistant:
			item := transcriptEntry{kind: entryAssistant, text: entry.Text, turnID: entry.PresentationTurn}
			item.stopped = entry.AssistantState == agentsession.AssistantStateStopped
			item.failed = entry.AssistantState == agentsession.AssistantStateFailed
			result = append(result, item)
		case agentsession.TranscriptKindTool:
			activities := make([]agentloop.Activity, 0, len(entry.Tools))
			for index, tool := range entry.Tools {
				status := agentloop.EventSucceeded
				switch tool.State {
				case agentsession.ToolStateFailed, agentsession.ToolStateStopped:
					status = agentloop.EventFailed
				case agentsession.ToolStateUnknown:
					status = agentloop.EventOutcomeUnknown
				}
				activities = append(activities, agentloop.Activity{
					Kind:  agentloop.ActivityTool,
					Event: agentloop.Event{ID: fmt.Sprintf("restored-%d-%d", entry.Sequence, index), Tool: tool.Name, Summary: tool.Summary, Status: status},
				})
			}
			if len(activities) > 0 {
				result = append(result, transcriptEntry{kind: entryTools, activities: activities, turnID: entry.PresentationTurn})
			}
		case agentsession.TranscriptKindError:
			if entry.Error != nil {
				result = append(result, transcriptEntry{kind: entryError, text: "[" + safeSingleLineTerminalText(entry.Error.Code) + "] 历史轮次未完成。", turnID: entry.PresentationTurn})
			}
		case agentsession.TranscriptKindContext:
			if entry.Context != nil {
				kind := agentloop.ContextEventCompacted
				switch entry.Context.Type {
				case agentsession.ContextEventDegraded:
					kind = agentloop.ContextEventDegraded
				case agentsession.ContextEventSourceUnavailable, agentsession.ContextEventPrivacyRevalidation:
					kind = agentloop.ContextEventSourceUnavailable
				}
				result = append(result, transcriptEntry{kind: entryContext, contextEvent: agentloop.ContextEvent{Kind: kind, Code: entry.Context.Message}, turnID: entry.PresentationTurn})
			}
		case agentsession.TranscriptKindFileNotice, agentsession.TranscriptKindPreferenceNotice, agentsession.TranscriptKindSessionNotice:
			if entry.Notice != nil {
				message := strings.TrimSpace(entry.Notice.Message)
				if message == "" {
					message = "[" + entry.Notice.Code + "]"
				}
				result = append(result, transcriptEntry{kind: entryNotice, text: message, turnID: entry.PresentationTurn})
			}
		}
	}
	return result
}
