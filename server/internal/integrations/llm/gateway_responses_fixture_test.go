package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
)

// These fixtures preserve the event and field shapes observed from the real
// Responses upstream for #158. All text, IDs, opaque values and counts are
// synthetic. The text fixture also represents the tool loop's final answer.
func gatewayResponsesFixture(t *testing.T, name string, stream bool) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/gateway_responses_real_" + name + ".sse")
	if err != nil {
		t.Fatal(err)
	}
	if stream {
		return raw
	}
	// Also exercise non-streaming conversion using the captured terminal shape.
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		var event struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
		}
		if err := json.Unmarshal(line[6:], &event); err != nil {
			t.Fatal(err)
		}
		if event.Type == "response.completed" {
			return event.Response
		}
	}
	t.Fatal("fixture has no terminal response")
	return nil
}

func gatewayResponsesFixtureRequest(t *testing.T, stream bool) GatewayRequest {
	t.Helper()
	raw := fmt.Sprintf(`{"model":"teaching","messages":[{"role":"system","content":"Use the local file tool."},{"role":"user","content":"Read note.txt and summarize it."}],"tools":[{"type":"function","function":{"name":"read_file","description":"Read a synthetic local text file.","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}}}],"max_tokens":100,"stream":%t}`, stream)
	r, err := DecodeGatewayRequest(strings.NewReader(raw), 32768, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestGatewayResponsesRealTextFixture(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			body := gatewayResponsesFixture(t, "text", stream)
			c := gatewayClient(t, Responses, func(w http.ResponseWriter, r *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
				}
				_, _ = w.Write(body)
			})
			var text string
			r, err := c.Gateway(t.Context(), gatewayResponsesFixtureRequest(t, stream), func(chunk GatewayChunk) error {
				text += chunk.Delta.Content
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			const want = "合成项目有 7 本笔记本，总价 91 元；验收要求无破损。"
			if r.Message.Content != want || r.FinishReason != "stop" || len(r.Message.ToolCalls) != 0 || stream && text != want {
				t.Fatalf("text conversion lost or duplicated content: %+v, emitted=%q", r, text)
			}
			if string(r.Usage["prompt_tokens"]) != "50" || string(r.Usage["completion_tokens"]) != "12" || string(r.Usage["total_tokens"]) != "62" || string(r.Usage["completion_tokens_details"]) != `{"reasoning_tokens":0}` || string(r.Usage["prompt_tokens_details"]) != `{"cached_tokens":20}` {
				t.Fatalf("lost terminal usage: %v", r.Usage)
			}
		})
	}
}

func TestGatewayResponsesRealToolLoopFixture(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			toolBody := gatewayResponsesFixture(t, "tool", stream)
			textBody := gatewayResponsesFixture(t, "text", stream)
			calls := 0
			c := gatewayClient(t, Responses, func(w http.ResponseWriter, r *http.Request) {
				calls++
				var payload struct {
					Store    *bool            `json:"store"`
					Previous json.RawMessage  `json:"previous_response_id"`
					Input    []map[string]any `json:"input"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				if payload.Store == nil || *payload.Store || len(payload.Previous) != 0 {
					t.Error("request lost stateless Responses settings")
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
				}
				if calls == 1 {
					_, _ = w.Write(toolBody)
					return
				}
				if calls != 2 || len(payload.Input) != 4 {
					t.Errorf("unexpected tool history or retry: calls=%d input=%v", calls, payload.Input)
				} else if payload.Input[2]["type"] != "function_call" || payload.Input[2]["call_id"] != "call_fixture" || payload.Input[2]["name"] != "read_file" || payload.Input[2]["arguments"] != `{"path":"note.txt"}` || payload.Input[3]["type"] != "function_call_output" || payload.Input[3]["call_id"] != "call_fixture" || payload.Input[3]["output"] != "synthetic file contents" {
					t.Errorf("lost function call/result association: %v", payload.Input)
				}
				_, _ = w.Write(textBody)
			})
			request := gatewayResponsesFixtureRequest(t, stream)
			var reasoning, arguments string
			first, err := c.Gateway(t.Context(), request, func(chunk GatewayChunk) error {
				reasoning += chunk.Delta.ReasoningContent
				for _, tool := range chunk.Delta.ToolCalls {
					arguments += tool.Function.Arguments
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if first.FinishReason != "tool_calls" || first.Message.Content != "" || len(first.Message.ToolCalls) != 1 || first.Message.ToolCalls[0].ID != "call_fixture" || first.Message.ToolCalls[0].Function.Name != "read_file" || first.Message.ToolCalls[0].Function.Arguments != `{"path":"note.txt"}` {
				t.Fatalf("invalid tool call: %+v", first)
			}
			if stream && (reasoning != "先读取合成文件。" || arguments != `{"path":"note.txt"}`) {
				t.Fatalf("lost reasoning or duplicated done arguments: reasoning=%q arguments=%q", reasoning, arguments)
			}
			if string(first.Usage["prompt_tokens"]) != "30" || string(first.Usage["completion_tokens"]) != "11" || string(first.Usage["total_tokens"]) != "41" || string(first.Usage["completion_tokens_details"]) != `{"reasoning_tokens":3}` {
				t.Fatalf("lost reasoning/tool usage: %v", first.Usage)
			}
			request.Messages = append(request.Messages, first.Message, GatewayMessage{Role: "tool", ToolCallID: first.Message.ToolCalls[0].ID, Content: "synthetic file contents"})
			final, err := c.Gateway(t.Context(), request, func(GatewayChunk) error { return nil })
			if err != nil || final.FinishReason != "stop" || final.Message.Content != "合成项目有 7 本笔记本，总价 91 元；验收要求无破损。" || len(final.Message.ToolCalls) != 0 || calls != 2 {
				t.Fatalf("tool loop did not complete: result=%+v calls=%d err=%v", final, calls, err)
			}
			if string(final.Usage["total_tokens"]) != "62" {
				t.Fatalf("lost final usage: %v", final.Usage)
			}
		})
	}
}

func TestGatewayResponsesRejectsOpaqueReasoning(t *testing.T) {
	const start = `{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","encrypted_content":"opaque","content":[]}}`
	const text = `{"type":"response.output_text.delta","output_index":1,"delta":"answer"}`
	const completed = `{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"answer"}]}]}}`
	for _, tc := range []struct {
		name string
		done string
	}{
		{"missing_plaintext_item", ""},
		{"opaque_only", `{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","encrypted_content":"opaque","content":[]}}`},
		{"empty_plaintext", `{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","encrypted_content":"opaque","content":[{"type":"reasoning_text","text":""}]}}`},
		{"unsupported_content", `{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","encrypted_content":"opaque","content":[{"type":"image","text":"opaque"}]}}`},
		{"changed_item_id", `{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"different","content":[{"type":"reasoning_text","text":"public"}]}}`},
		{"duplicate_opaque_item", `{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"different","encrypted_content":"opaque","content":[]}}` + "\n\ndata: " + `{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"different","content":[{"type":"reasoning_text","text":"public"}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := gatewayClient(t, Responses, func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "text/event-stream")
				for _, event := range []string{start, tc.done, text, completed} {
					if event != "" {
						fmt.Fprintf(w, "data: %s\n\n", event)
					}
				}
			})
			_, err := c.Gateway(t.Context(), gatewayResponsesFixtureRequest(t, true), func(GatewayChunk) error { return nil })
			if err != GatewayError("upstream_incompatible") || calls != 1 {
				t.Fatalf("opaque reasoning accepted or retried: err=%v calls=%d", err, calls)
			}
		})
	}
}

func TestGatewayResponsesReasoningDoneIntegrity(t *testing.T) {
	fixture := gatewayResponsesFixture(t, "tool", true)
	for _, tc := range []struct {
		name string
		body string
	}{
		{"mismatched_done", strings.Replace(string(fixture), `"text":"先读取合成文件。"`, `"text":"changed"`, 1)},
		{"delta_after_done", strings.Replace(string(fixture), "event: response.content_part.done", "data: {\"type\":\"response.reasoning_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"late\"}\n\nevent: response.content_part.done", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := gatewayClient(t, Responses, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, tc.body)
			})
			if _, err := c.Gateway(t.Context(), gatewayResponsesFixtureRequest(t, true), func(GatewayChunk) error { return nil }); err != GatewayError("upstream_incompatible") {
				t.Fatalf("inconsistent reasoning accepted: %v", err)
			}
		})
	}
}
