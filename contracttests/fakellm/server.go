// Command fakellm is a dependency-free OpenAI-compatible contract fixture.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	address := env("FAKE_LLM_ADDR", "127.0.0.1:18081")
	mode := env("FAKE_LLM_MODE", "success")
	apiKey := env("FAKE_LLM_API_KEY", "fake-development-key")
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+apiKey || mode == "unauthorized" {
			w.WriteHeader(http.StatusUnauthorized)
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
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			writeResponse(w, `{"capability_probe":true}`)
		}
	})
	log.Printf("fake LLM listening on %s in %s mode", address, mode)
	log.Fatal(http.ListenAndServe(address, handler))
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
