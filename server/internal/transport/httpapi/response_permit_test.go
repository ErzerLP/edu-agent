package httpapi

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
)

type gatedHTTPResponseWriter struct {
	header       http.Header
	status       int
	body         bytes.Buffer
	writeStarted chan struct{}
	releaseWrite chan struct{}
	startOnce    sync.Once
}

func newGatedHTTPResponseWriter() *gatedHTTPResponseWriter {
	return &gatedHTTPResponseWriter{
		header:       make(http.Header),
		writeStarted: make(chan struct{}),
		releaseWrite: make(chan struct{}),
	}
}

func (w *gatedHTTPResponseWriter) Header() http.Header { return w.header }

func (w *gatedHTTPResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *gatedHTTPResponseWriter) Write(value []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.startOnce.Do(func() { close(w.writeStarted) })
	<-w.releaseWrite
	return w.body.Write(value)
}

func TestHTTPResponseReadPermitLinearizesPrivacyClosure(t *testing.T) {
	t.Run("close wins without response bytes", func(t *testing.T) {
		manager := privacy.NewReadPermitManager()
		api := &API{
			readPermits: manager,
			logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		started := make(chan struct{})
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(started)
			<-r.Context().Done()
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "private response")
		})
		handler := api.handleResponseReadPermit(memory.CodeContentRedacted, privacy.OwnerKnowledge, privacy.OwnerLearning)(next)
		response := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
			close(done)
		}()
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("handler did not start")
		}

		drainDone := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			drainDone <- manager.CloseAndDrain(ctx, 2, privacy.OwnerLearning)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("handler did not stop after privacy closure")
		}
		if err := <-drainDone; err != nil {
			t.Fatal(err)
		}
		if response.Code != http.StatusServiceUnavailable || !bytes.Contains(response.Body.Bytes(), []byte(memory.CodeContentRedacted)) ||
			bytes.Contains(response.Body.Bytes(), []byte("private response")) {
			t.Fatalf("close-wins response status=%d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
		}
	})

	t.Run("response wins completely across multiple owners", func(t *testing.T) {
		manager := privacy.NewReadPermitManager()
		api := &API{
			readPermits: manager,
			logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "complete response")
		})
		handler := api.handleResponseReadPermit(memory.CodeContentRedacted, privacy.OwnerKnowledge, privacy.OwnerLearning)(next)
		response := newGatedHTTPResponseWriter()
		done := make(chan struct{})
		go func() {
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
			close(done)
		}()
		select {
		case <-response.writeStarted:
		case <-time.After(time.Second):
			t.Fatal("response write did not start")
		}

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		err := manager.CloseAndDrain(ctx, 2, privacy.OwnerLearning)
		cancel()
		if err != context.DeadlineExceeded {
			t.Fatalf("privacy close while response owns gate err=%v", err)
		}
		if closed, _ := manager.Closed(privacy.OwnerLearning); closed {
			t.Fatal("privacy close overtook the active HTTP response")
		}

		close(response.releaseWrite)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("response write did not finish")
		}
		if response.status != http.StatusCreated || response.header.Get("Content-Type") != "text/plain" || response.body.String() != "complete response" {
			t.Fatalf("response status=%d headers=%v body=%q", response.status, response.header, response.body.String())
		}

		drainCtx, drainCancel := context.WithTimeout(context.Background(), time.Second)
		defer drainCancel()
		if err := manager.CloseAndDrain(drainCtx, 2, privacy.OwnerKnowledge, privacy.OwnerLearning); err != nil {
			t.Fatalf("multi-owner close after response: %v", err)
		}
	})
}
