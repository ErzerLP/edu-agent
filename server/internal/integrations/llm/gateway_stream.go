package llm

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
)

type gatewayStreamState struct {
	result        GatewayResult
	emit          func(GatewayChunk) error
	started       bool
	finished      bool
	tools         map[int]int
	argumentsDone map[int]bool
	textDone      map[string]string
	text          map[string]string
}

func (s *gatewayStreamState) send(c GatewayChunk) error {
	if c.Delta.Role != "" && c.Delta.Role != "assistant" {
		return GatewayError("upstream_incompatible")
	}
	if !s.started {
		s.started = true
		if err := s.emit(GatewayChunk{Delta: GatewayDelta{Role: "assistant"}}); err != nil {
			return err
		}
	}
	if c.Delta.Role == "assistant" && c.Delta.Content == "" && c.Delta.ReasoningContent == "" && len(c.Delta.ToolCalls) == 0 && c.FinishReason == "" && len(c.Usage) == 0 {
		return nil
	}
	c.Delta.Role = ""
	if len(c.Delta.Content) > 64<<10 || len(c.Delta.ReasoningContent) > 64<<10 || !utf8.ValidString(c.Delta.Content) || !utf8.ValidString(c.Delta.ReasoningContent) {
		return GatewayError("upstream_incompatible")
	}
	if s.finished && (c.Delta.Content != "" || c.Delta.ReasoningContent != "" || len(c.Delta.ToolCalls) > 0 || c.FinishReason != "") {
		return GatewayError("upstream_incompatible")
	}
	s.result.Message.Content += c.Delta.Content
	if len(s.result.Message.Content) > 1<<20 {
		return GatewayError("upstream_incompatible")
	}
	for _, d := range c.Delta.ToolCalls {
		if d.Index < 0 || d.Index > 1023 || d.Type != "" && d.Type != "function" || len(d.Function.Arguments) > 128<<10 {
			return GatewayError("upstream_incompatible")
		}
		if d.Index > len(s.result.Message.ToolCalls) {
			return GatewayError("upstream_incompatible")
		}
		if d.Index == len(s.result.Message.ToolCalls) {
			s.result.Message.ToolCalls = append(s.result.Message.ToolCalls, modelclient.ToolCall{Type: "function"})
		}
		t := &s.result.Message.ToolCalls[d.Index]
		if d.ID != "" {
			t.ID += d.ID
		}
		t.Function.Name += d.Function.Name
		t.Function.Arguments += d.Function.Arguments
		if len(t.ID) > 512 || len(t.Function.Name) > 256 || len(t.Function.Arguments) > 1<<20 {
			return GatewayError("upstream_incompatible")
		}
	}
	if c.FinishReason != "" {
		s.result.FinishReason = c.FinishReason
		if err := validGatewayResult(s.result); err != nil {
			return err
		}
		s.finished = true
	}
	if len(c.Usage) > 0 {
		s.result.Usage = c.Usage
	}
	return s.emit(c)
}

func (c *Client) gatewayStream(reader io.Reader, emit func(GatewayChunk) error, progress func()) (GatewayResult, error) {
	s := &gatewayStreamState{result: GatewayResult{Message: GatewayMessage{Role: "assistant"}}, emit: emit, tools: map[int]int{}, argumentsDone: map[int]bool{}, text: map[string]string{}, textDone: map[string]string{}}
	scanner := bufio.NewScanner(io.LimitReader(reader, (256<<20)+1))
	frameLimit := 512 << 10
	if c.wireAPI == Responses {
		frameLimit = 16 << 20
	}
	scanner.Buffer(make([]byte, 4096), frameLimit)
	var frame []byte
	done := false
	events, lines := 0, 0
	process := func() error {
		if len(frame) == 0 {
			return nil
		}
		events++
		if events > (1<<20)+4096 {
			return GatewayError("upstream_incompatible")
		}
		data := bytes.TrimSpace(frame)
		if bytes.Equal(data, []byte("[DONE]")) {
			if !s.finished {
				return GatewayError("upstream_incompatible")
			}
			done = true
			return nil
		}
		if !utf8.Valid(data) {
			return GatewayError("upstream_incompatible")
		}
		var err error
		if c.wireAPI == Responses {
			done, err = s.responsesEvent(data, progress)
		} else {
			var e struct {
				Choices []struct {
					Index        int          `json:"index"`
					Delta        GatewayDelta `json:"delta"`
					FinishReason string       `json:"finish_reason"`
				} `json:"choices"`
				Usage map[string]json.RawMessage `json:"usage"`
				Error json.RawMessage            `json:"error"`
			}
			if json.Unmarshal(data, &e) != nil || len(e.Error) > 0 {
				return GatewayError("upstream_incompatible")
			}
			if len(e.Choices) > 1 {
				return GatewayError("upstream_incompatible")
			}
			if len(e.Choices) == 1 {
				v := e.Choices[0]
				if v.Index != 0 {
					return GatewayError("upstream_incompatible")
				}
				progress()
				err = s.send(GatewayChunk{Delta: v.Delta, FinishReason: v.FinishReason})
			}
			if err == nil && len(e.Usage) > 0 {
				progress()
				err = s.send(GatewayChunk{Usage: cleanUsage(e.Usage, false)})
			}
		}
		return err
	}
	for scanner.Scan() {
		lines++
		if lines > 4*((1<<20)+4096) {
			return s.result, GatewayError("upstream_incompatible")
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			if err := process(); err != nil {
				return s.result, err
			}
			frame = frame[:0]
			if done {
				return s.result, nil
			}
		} else if bytes.HasPrefix(line, []byte("data:")) {
			if len(frame)+len(line) > frameLimit {
				return s.result, GatewayError("upstream_incompatible")
			}
			frame = append(frame, bytes.TrimPrefix(line[5:], []byte(" "))...)
			frame = append(frame, '\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return s.result, err
	}
	return s.result, GatewayError("upstream_incompatible")
}

type gatewayResponseItem struct {
	Type             string `json:"type"`
	ID               string `json:"id"`
	CallID           string `json:"call_id"`
	Name             string `json:"name"`
	Arguments        string `json:"arguments"`
	EncryptedContent string `json:"encrypted_content"`
	Content          []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}
type gatewayResponse struct {
	Status            string `json:"status"`
	IncompleteDetails struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Output []gatewayResponseItem      `json:"output"`
	Usage  map[string]json.RawMessage `json:"usage"`
}

func responseFinish(r gatewayResponse, tools bool) (string, error) {
	switch r.Status {
	case "completed":
		if tools {
			return "tool_calls", nil
		}
		return "stop", nil
	case "incomplete":
		switch r.IncompleteDetails.Reason {
		case "max_output_tokens":
			return "length", nil
		case "content_filter":
			return "content_filter", nil
		}
	}
	return "", GatewayError("upstream_incompatible")
}
func supportedItem(item gatewayResponseItem) bool {
	return item.EncryptedContent == "" && (item.Type == "message" || item.Type == "function_call" || item.Type == "reasoning")
}

func gatewayResponsesComplete(reader io.Reader) (GatewayResult, error) {
	var body gatewayResponse
	if err := gatewayReadJSON(reader, &body); err != nil {
		return GatewayResult{}, err
	}
	r := GatewayResult{Message: GatewayMessage{Role: "assistant"}, Usage: cleanUsage(body.Usage, true)}
	for _, item := range body.Output {
		if !supportedItem(item) {
			return r, GatewayError("upstream_incompatible")
		}
		switch item.Type {
		case "message":
			for _, p := range item.Content {
				if p.Type != "output_text" {
					return r, GatewayError("upstream_incompatible")
				}
				r.Message.Content += p.Text
			}
		case "function_call":
			r.Message.ToolCalls = append(r.Message.ToolCalls, modelclient.ToolCall{ID: item.CallID, Type: "function", Function: modelclient.ToolFunction{Name: item.Name, Arguments: item.Arguments}})
		}
	}
	var err error
	r.FinishReason, err = responseFinish(body, len(r.Message.ToolCalls) > 0)
	if err != nil {
		return r, err
	}
	return r, validGatewayResult(r)
}

func (s *gatewayStreamState) responsesEvent(raw []byte, progress func()) (bool, error) {
	var e struct {
		Type         string              `json:"type"`
		OutputIndex  int                 `json:"output_index"`
		ContentIndex int                 `json:"content_index"`
		Delta        string              `json:"delta"`
		Text         string              `json:"text"`
		Arguments    string              `json:"arguments"`
		Item         gatewayResponseItem `json:"item"`
		Response     gatewayResponse     `json:"response"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return false, GatewayError("upstream_incompatible")
	}
	switch e.Type {
	case "response.created", "response.in_progress", "response.queued":
		progress()
		return false, nil
	case "response.output_item.added":
		progress()
		if !supportedItem(e.Item) {
			return false, GatewayError("upstream_incompatible")
		}
		if e.Item.Type == "function_call" {
			if _, exists := s.tools[e.OutputIndex]; exists {
				return false, GatewayError("upstream_incompatible")
			}
			index := len(s.tools)
			s.tools[e.OutputIndex] = index
			return false, s.send(GatewayChunk{Delta: GatewayDelta{ToolCalls: []GatewayToolDelta{{Index: index, ID: e.Item.CallID, Type: "function", Function: modelclient.ToolFunction{Name: e.Item.Name, Arguments: e.Item.Arguments}}}}})
		}
	case "response.output_text.delta":
		progress()
		key := fmt.Sprintf("%d/%d", e.OutputIndex, e.ContentIndex)
		if _, done := s.textDone[key]; done {
			return false, GatewayError("upstream_incompatible")
		}
		s.text[key] += e.Delta
		return false, s.send(GatewayChunk{Delta: GatewayDelta{Content: e.Delta}})
	case "response.output_text.done":
		progress()
		key := fmt.Sprintf("%d/%d", e.OutputIndex, e.ContentIndex)
		if s.text[key] != e.Text {
			return false, GatewayError("upstream_incompatible")
		}
		s.textDone[key] = e.Text
	case "response.reasoning_summary_text.delta":
		progress()
		return false, s.send(GatewayChunk{Delta: GatewayDelta{ReasoningContent: e.Delta}})
	case "response.function_call_arguments.delta":
		progress()
		index, ok := s.tools[e.OutputIndex]
		if !ok || s.argumentsDone[e.OutputIndex] {
			return false, GatewayError("upstream_incompatible")
		}
		return false, s.send(GatewayChunk{Delta: GatewayDelta{ToolCalls: []GatewayToolDelta{{Index: index, Function: modelclient.ToolFunction{Arguments: e.Delta}}}}})
	case "response.function_call_arguments.done":
		progress()
		index, ok := s.tools[e.OutputIndex]
		if !ok || s.result.Message.ToolCalls[index].Function.Arguments != e.Arguments {
			return false, GatewayError("upstream_incompatible")
		}
		s.argumentsDone[e.OutputIndex] = true
	case "response.output_item.done":
		progress()
		if !supportedItem(e.Item) {
			return false, GatewayError("upstream_incompatible")
		}
		if e.Item.Type == "function_call" {
			index, ok := s.tools[e.OutputIndex]
			if !ok {
				return false, GatewayError("upstream_incompatible")
			}
			c := s.result.Message.ToolCalls[index]
			if c.ID != e.Item.CallID || c.Function.Name != e.Item.Name || c.Function.Arguments != e.Item.Arguments {
				return false, GatewayError("upstream_incompatible")
			}
		}
	case "response.completed", "response.incomplete":
		progress()
		for _, item := range e.Response.Output {
			if !supportedItem(item) {
				return false, GatewayError("upstream_incompatible")
			}
		}
		finish, err := responseFinish(e.Response, len(s.tools) > 0)
		if err != nil {
			return false, err
		}
		if err = s.send(GatewayChunk{FinishReason: finish}); err != nil {
			return false, err
		}
		if usage := cleanUsage(e.Response.Usage, true); len(usage) > 0 {
			if err = s.send(GatewayChunk{Usage: usage}); err != nil {
				return false, err
			}
		}
		return true, nil
	case "response.failed", "error":
		return false, GatewayError("upstream_error")
	case "response.content_part.added", "response.content_part.done", "response.reasoning_summary_part.added", "response.reasoning_summary_part.done", "response.reasoning_summary_text.done":
		progress()
	default:
		if strings.HasPrefix(e.Type, "response.") {
			return false, GatewayError("upstream_incompatible")
		}
	}
	return false, nil
}
