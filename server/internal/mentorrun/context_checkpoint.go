package mentorrun

import "github.com/edu-agent/edu-agent/packages/agentcore/modelclient"

func (b *Body) appendMessage(m modelclient.Message) {
	b.Messages = append(b.Messages, m)
	if b.TurnMessages != nil && m.Role != "tool" && len(m.ToolCalls) == 0 {
		b.TurnMessages = append(b.TurnMessages, m)
	}
}

// Tool payloads are internal model data. Preserve the user-readable turn in a
// separate array before dropping completed internal groups; pending groups and
// their call IDs remain available to the executor and resume path.
func (b *Body) compactToolCheckpoint() {
	if b.Context == nil {
		return
	}
	b.Messages = projectOldTools(b.Messages, 2048)
	if checkpointJSONSize(*b) <= MaxBody/2 {
		return
	}
	// A single exceptional tool payload must also fit the checkpoint, while
	// ordinary current results remain intact for the next model request.
	b.Messages = projectTools(b.Messages, MaxBody/8)
	if b.TurnMessages == nil {
		b.TurnMessages = []modelclient.Message{}
		for _, m := range b.Messages[b.Context.TurnStart:] {
			if m.Role != "tool" && len(m.ToolCalls) == 0 {
				b.TurnMessages = append(b.TurnMessages, m)
			}
		}
	}
	for checkpointJSONSize(*b) > MaxBody/2 {
		removed := false
		for i, m := range b.Messages {
			if len(m.ToolCalls) == 0 {
				continue
			}
			end := i + 1 + len(m.ToolCalls)
			if end >= len(b.Messages) {
				break
			} // retain latest complete or in-flight group
			complete := true
			for j, c := range m.ToolCalls {
				if b.Messages[i+1+j].Role != "tool" || b.Messages[i+1+j].ToolCallID != c.ID {
					complete = false
					break
				}
			}
			if !complete {
				break
			}
			b.Messages = append(b.Messages[:i:i], b.Messages[end:]...)
			removed = true
			break
		}
		if !removed {
			break
		}
	}
	b.ContextNotice = historyNotice
}
