// Command fakellm is a dependency-free OpenAI-compatible contract fixture.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	address := env("FAKE_LLM_ADDR", "127.0.0.1:18081")
	mode := env("FAKE_LLM_MODE", "success")
	apiKey := env("FAKE_LLM_API_KEY", "fake-development-key")
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+apiKey || mode == "unauthorized" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		request, err := decodeChatRequest(r)
		if err != nil {
			http.Error(w, "invalid chat completions profile", http.StatusBadRequest)
			return
		}
		if mode == "no-native-schema" && request.ResponseFormat.Type == "json_schema" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch mode {
		case "rate-limited":
			w.WriteHeader(http.StatusTooManyRequests)
		case "server-error":
			w.WriteHeader(http.StatusBadGateway)
		case "timeout":
			time.Sleep(60 * time.Second)
		case "invalid-json":
			_, _ = w.Write([]byte(`{"choices":`))
		case "schema-mismatch":
			writeResponse(w, `{"capability_probe":"yes"}`)
		default:
			writeResponse(w, `{"capability_probe":true}`)
		}
	})
	log.Printf("fake LLM listening on %s in %s mode", address, mode)
	log.Fatal(http.ListenAndServe(address, handler))
}

type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Stream         *bool `json:"stream"`
	ResponseFormat struct {
		Type string `json:"type"`
	} `json:"response_format"`
}

func decodeChatRequest(r *http.Request) (chatRequest, error) {
	var request chatRequest
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		return request, fmt.Errorf("content type must be application/json")
	}
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&request); err != nil {
		return request, err
	}
	if strings.TrimSpace(request.Model) == "" || request.Stream == nil || *request.Stream {
		return request, fmt.Errorf("model and stream=false are required")
	}
	if request.ResponseFormat.Type != "json_object" && request.ResponseFormat.Type != "json_schema" {
		return request, fmt.Errorf("structured response format is required")
	}
	roles := map[string]bool{}
	for _, message := range request.Messages {
		if strings.TrimSpace(message.Content) == "" {
			return request, fmt.Errorf("message content is required")
		}
		roles[message.Role] = true
	}
	if !roles["system"] || !roles["user"] || !roles["assistant"] {
		return request, fmt.Errorf("system, user, and assistant messages are required")
	}
	return request, nil
}

func writeResponse(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q}}]}`, content)
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
