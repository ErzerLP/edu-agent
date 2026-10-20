package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func gatewayClient(t *testing.T, style WireAPI, handler http.HandlerFunc) *Client {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	u, _ := url.Parse(s.URL)
	c, err := New(Options{BaseURL: u, WireAPI: style, Model: "actual-model", APIKey: "secret", ContextWindow: 32768, MinimumContext: 4096, MaxOutputTokens: 2048, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestGatewayResponsesToolsAndReasoning(t *testing.T) {
	c := gatewayClient(t, Responses, func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Error(err)
		}
		if r.URL.Path != "/v1/responses" || p["model"] != "actual-model" || p["store"] != false || p["max_output_tokens"] != float64(100) {
			t.Errorf("invalid upstream request: %v", p)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range []string{
			`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"你好🌍"}`,
			`{"type":"response.reasoning_summary_text.delta","delta":"检查"}`,
			`{"type":"response.output_item.added","output_index":1,"item":{"id":"item-a","type":"function_call","call_id":"call-a","name":"read_file","arguments":""}}`,
			`{"type":"response.output_item.added","output_index":2,"item":{"id":"item-b","type":"function_call","call_id":"call-b","name":"read_file","arguments":""}}`,
			`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"path\":"}`,
			`{"type":"response.function_call_arguments.delta","output_index":2,"delta":"{}"}`,
			`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"\"中文\"}"}`,
			`{"type":"response.function_call_arguments.done","output_index":1,"arguments":"{\"path\":\"中文\"}"}`,
			`{"type":"response.function_call_arguments.done","output_index":2,"arguments":"{}"}`,
			`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":12,"output_tokens":8,"total_tokens":20,"output_tokens_details":{"reasoning_tokens":3}}}}`,
		} {
			for _, b := range []byte("data: " + e + "\n\n") {
				_, _ = w.Write([]byte{b})
				w.(http.Flusher).Flush()
			}
		}
	})
	var chunks []GatewayChunk
	r, err := c.Gateway(context.Background(), GatewayRequest{Messages: []GatewayMessage{{Role: "user", Content: "check"}}, MaxTokens: 100, Stream: true}, func(c GatewayChunk) error { chunks = append(chunks, c); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if r.Message.Content != "你好🌍" || r.FinishReason != "tool_calls" || len(r.Message.ToolCalls) != 2 || r.Message.ToolCalls[0].ID != "call-a" || r.Message.ToolCalls[0].Function.Arguments != `{"path":"中文"}` || r.Message.ToolCalls[1].Function.Arguments != "{}" {
		t.Fatalf("bad result: %+v", r)
	}
	var text, reasoning string
	for _, c := range chunks {
		text += c.Delta.Content
		reasoning += c.Delta.ReasoningContent
	}
	if text != "你好🌍" || reasoning != "检查" || string(r.Usage["prompt_tokens"]) != "12" || string(r.Usage["completion_tokens_details"]) != `{"reasoning_tokens":3}` {
		t.Fatalf("lost deltas/usage: %q %q %+v", text, reasoning, r.Usage)
	}
}

func TestGatewayChatFallbackAndNoRetryAfterStart(t *testing.T) {
	for _, broken := range []bool{false, true} {
		t.Run(fmt.Sprint(broken), func(t *testing.T) {
			calls := 0
			c := gatewayClient(t, ChatCompletions, func(w http.ResponseWriter, r *http.Request) {
				calls++
				var p map[string]any
				_ = json.NewDecoder(r.Body).Decode(&p)
				if !broken && calls == 1 {
					w.WriteHeader(400)
					fmt.Fprint(w, `{"error":{"param":"stream_options","code":"unsupported_parameter"}}`)
					return
				}
				if !broken && p["stream_options"] != nil {
					t.Error("fallback retained stream_options")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"中文\"}}]}\n\n")
				if !broken {
					fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				}
			})
			r, err := c.Gateway(t.Context(), GatewayRequest{Messages: []GatewayMessage{{Role: "user", Content: "hi"}}, Stream: true}, func(GatewayChunk) error { return nil })
			if broken {
				if err == nil || calls != 1 {
					t.Fatalf("incomplete stream retried/succeeded: %d %v", calls, err)
				}
				return
			}
			if err != nil || calls != 2 || r.Message.Content != "中文" || len(r.Usage) != 0 {
				t.Fatalf("fallback: %+v %d %v", r, calls, err)
			}
		})
	}
}

func TestGatewayRejectsProxyAndInvalidHistory(t *testing.T) {
	for _, raw := range []string{`{"model":"teaching","messages":[{"role":"user","content":"x"}],"url":"https://bad"}`, `{"model":"teaching","messages":[{"role":"tool","tool_call_id":"unknown","content":"x"}]}`, `{"model":"teaching","messages":[{"role":"user","content":[{"type":"image_url"}]}]}`} {
		if _, err := DecodeGatewayRequest(strings.NewReader(raw), 32768, 2048); err == nil {
			t.Fatalf("accepted invalid input %s", raw)
		}
	}
}

func TestGatewayUsageTruncationAndNonStreaming(t *testing.T) {
	for _, style := range []WireAPI{ChatCompletions, Responses} {
		for _, stream := range []bool{false, true} {
			for _, coverage := range []string{"complete", "partial", "missing"} {
				t.Run(fmt.Sprintf("%s/%v/%s", style, stream, coverage), func(t *testing.T) {
					usage := map[string]any{}
					if coverage != "missing" {
						if style == Responses {
							usage["input_tokens"] = 11
						} else {
							usage["prompt_tokens"] = 11
						}
					}
					if coverage == "complete" {
						if style == Responses {
							usage["output_tokens"] = 7
							usage["output_tokens_details"] = map[string]int{"reasoning_tokens": 2}
						} else {
							usage["completion_tokens"] = 7
							usage["completion_tokens_details"] = map[string]int{"reasoning_tokens": 2}
						}
						usage["total_tokens"] = 18
					}
					c := gatewayClient(t, style, func(w http.ResponseWriter, r *http.Request) {
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
						}
						if style == Responses {
							body := map[string]any{"status": "incomplete", "incomplete_details": map[string]string{"reason": "max_output_tokens"}, "output": []any{map[string]any{"type": "message", "content": []any{map[string]string{"type": "output_text", "text": "回答"}}}}}
							if len(usage) > 0 {
								body["usage"] = usage
							}
							if stream {
								fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"回答\"}\n\n")
								b, _ := json.Marshal(map[string]any{"type": "response.incomplete", "response": body})
								fmt.Fprintf(w, "data: %s\n\n", b)
							} else {
								_ = json.NewEncoder(w).Encode(body)
							}
						} else {
							if stream {
								fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"回答\"}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"length\"}]}\n\n")
								if len(usage) > 0 {
									b, _ := json.Marshal(map[string]any{"choices": []any{}, "usage": usage})
									fmt.Fprintf(w, "data: %s\n\n", b)
								}
								fmt.Fprint(w, "data: [DONE]\n\n")
							} else {
								body := map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": "回答"}, "finish_reason": "length"}}}
								if len(usage) > 0 {
									body["usage"] = usage
								}
								_ = json.NewEncoder(w).Encode(body)
							}
						}
					})
					r, err := c.Gateway(t.Context(), GatewayRequest{Messages: []GatewayMessage{{Role: "user", Content: "hi"}}, Stream: stream}, func(GatewayChunk) error { return nil })
					if err != nil || r.FinishReason != "length" || r.Message.Content != "回答" {
						t.Fatalf("truncation: %+v %v", r, err)
					}
					if coverage == "missing" && len(r.Usage) != 0 || coverage == "partial" && (string(r.Usage["prompt_tokens"]) != "11" || len(r.Usage) != 1) || coverage == "complete" && (string(r.Usage["completion_tokens"]) != "7" || string(r.Usage["total_tokens"]) != "18") {
						t.Fatalf("fabricated/lost usage %+v", r.Usage)
					}
				})
			}
		}
	}
}

func TestGatewayChatMultiToolFragmentsAndStreamRefusal(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprint(fallback), func(t *testing.T) {
			calls := 0
			c := gatewayClient(t, ChatCompletions, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if fallback && calls == 1 {
					w.WriteHeader(400)
					fmt.Fprint(w, `{"error":{"param":"stream","code":"unsupported_feature"}}`)
					return
				}
				if fallback {
					fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"read_file","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				for _, raw := range []string{`{"choices":[{"index":0,"delta":{"reasoning_content":"计划","tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"read_","arguments":"{"}},{"index":1,"id":"b","type":"function","function":{"name":"list","arguments":"{}"}}]}}]}`, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"file","arguments":"}"}}]},"finish_reason":"tool_calls"}]}`} {
					fmt.Fprintf(w, "data: %s\n\n", raw)
				}
				fmt.Fprint(w, "data: [DONE]\n\n")
			})
			r, err := c.Gateway(t.Context(), GatewayRequest{Messages: []GatewayMessage{{Role: "user", Content: "hi"}}, Stream: true}, func(GatewayChunk) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			want := 2
			if fallback {
				want = 1
			}
			if len(r.Message.ToolCalls) != want || r.Message.ToolCalls[0].Function.Name != "read_file" || r.Message.ToolCalls[0].Function.Arguments != "{}" {
				t.Fatalf("lost tools %+v", r)
			}
			if fallback && calls != 2 {
				t.Fatal("stream fallback count")
			}
		})
	}
}

func TestGatewayResponsesHistoryAndDoneIntegrity(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(fmt.Sprint(mismatch), func(t *testing.T) {
			c := gatewayClient(t, Responses, func(w http.ResponseWriter, r *http.Request) {
				var p struct {
					Store bool             `json:"store"`
					Input []map[string]any `json:"input"`
					Tools []struct {
						Parameters map[string]any `json:"parameters"`
					} `json:"tools"`
				}
				if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
					t.Error(err)
				}
				if p.Store || len(p.Input) != 5 || p.Input[2]["type"] != "function_call" || p.Input[2]["call_id"] != "call-1" || p.Input[3]["type"] != "function_call_output" || p.Input[3]["call_id"] != "call-1" || p.Input[3]["output"] != "result" || len(p.Tools) != 1 || len(p.Tools[0].Parameters["required"].([]any)) != 1 {
					t.Errorf("lost history/schema %+v", p)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"原文\"}\n\n")
				text := "原文"
				if mismatch {
					text = "不同的完整内容"
				}
				b, _ := json.Marshal(map[string]any{"type": "response.output_text.done", "output_index": 0, "content_index": 0, "text": text})
				fmt.Fprintf(w, "data: %s\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n", b)
			})
			raw := `{"model":"teaching","messages":[{"role":"user","content":"inspect"},{"role":"assistant","content":"先读取","tool_calls":[{"id":"call-1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a\"}"}}]},{"role":"tool","tool_call_id":"call-1","content":"result"},{"role":"user","content":"continue"}],"tools":[{"type":"function","function":{"name":"read_file","description":"读取","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}}],"stream":true}`
			r, err := DecodeGatewayRequest(strings.NewReader(raw), 32768, 2048)
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Gateway(t.Context(), r, func(GatewayChunk) error { return nil })
			if mismatch && err == nil {
				t.Fatal("inconsistent done text accepted")
			}
			if !mismatch && err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGatewayChatToolIdentifierFragments(t *testing.T) {
	c := gatewayClient(t, ChatCompletions, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-","type":"function","function":{"name":"read","arguments":"{"}}]}}]}`, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"123","function":{"name":"_file","arguments":"}"}}]},"finish_reason":"tool_calls"}]}`} {
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	r, err := c.Gateway(t.Context(), GatewayRequest{Messages: []GatewayMessage{{Role: "user", Content: "read"}}, Stream: true}, func(GatewayChunk) error { return nil })
	if err != nil || len(r.Message.ToolCalls) != 1 || r.Message.ToolCalls[0].ID != "call-123" {
		t.Fatalf("fragmented tool identity: %+v %v", r, err)
	}
}

func TestGatewayRejectsEmptySuccessfulAnswer(t *testing.T) {
	c := gatewayClient(t, ChatCompletions, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant"},"finish_reason":"stop"}]}`)
	})
	if _, err := c.Gateway(t.Context(), GatewayRequest{Messages: []GatewayMessage{{Role: "user", Content: "hello"}}}, nil); err == nil {
		t.Fatal("empty output marked successful")
	}
}

func TestGatewayRejectsUpstreamRoleMismatch(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			c := gatewayClient(t, ChatCompletions, func(w http.ResponseWriter, r *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"user\",\"content\":\"wrong\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				} else {
					fmt.Fprint(w, `{"choices":[{"message":{"role":"user","content":"wrong"},"finish_reason":"stop"}]}`)
				}
			})
			if _, err := c.Gateway(t.Context(), GatewayRequest{Messages: []GatewayMessage{{Role: "user", Content: "hello"}}, Stream: stream}, func(GatewayChunk) error { return nil }); err == nil {
				t.Fatal("role mismatch accepted")
			}
		})
	}
}
