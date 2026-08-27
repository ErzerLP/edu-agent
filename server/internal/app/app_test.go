package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/integrations/llm"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/platform/config"
	outboxpostgres "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
)

type compositionTreeReader struct{}

func (compositionTreeReader) Tree(context.Context, string) (knowledge.TreeResult, error) {
	return knowledge.TreeResult{}, nil
}

type observedListener struct {
	net.Listener
	closed chan struct{}
	once   sync.Once
}

func (l *observedListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return l.Listener.Close()
}

func TestShutdownClosesListenerAndUsesIndependentWorkerAndHTTPBudgets(t *testing.T) {
	rawListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := &observedListener{Listener: rawListener, closed: make(chan struct{})}
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(requestStarted)
		<-releaseRequest
	})}
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(listener) }()
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		response, requestErr := http.Get("http://" + listener.Addr().String())
		if requestErr == nil {
			_ = response.Body.Close()
		}
	}()
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("slow request did not start")
	}

	workerStarted := make(chan struct{})
	releaseWorker := make(chan struct{})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	workers := startWorkerGroup(context.Background(), logger, []workerSpec{periodicWorker("slow", time.Hour, 1, func(context.Context) (int, error) {
		close(workerStarted)
		<-releaseWorker
		return 0, nil
	})})
	select {
	case <-workerStarted:
	case <-time.After(time.Second):
		t.Fatal("slow worker did not start")
	}

	const budget = 150 * time.Millisecond
	startedAt := time.Now()
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- shutdownRuntime(listener, server, workers, budget, logger) }()
	select {
	case <-listener.closed:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("listener was not closed immediately")
	}
	connection, dialErr := net.DialTimeout("tcp", listener.Addr().String(), 25*time.Millisecond)
	if dialErr == nil {
		_ = connection.Close()
		t.Fatal("listener accepted a new connection during shutdown")
	}
	shutdownErr := <-shutdownDone
	elapsed := time.Since(startedAt)
	if !errors.Is(shutdownErr, errWorkerShutdownTimeout) || !errors.Is(shutdownErr, errHTTPShutdownTimeout) {
		t.Fatalf("shutdown error=%v", shutdownErr)
	}
	if elapsed >= 260*time.Millisecond {
		t.Fatalf("shutdown budgets were consumed serially: elapsed=%s", elapsed)
	}
	close(releaseWorker)
	close(releaseRequest)
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("slow request did not exit after forced close")
	}
	select {
	case <-serveResult:
	case <-time.After(time.Second):
		t.Fatal("HTTP server did not stop")
	}
}

func TestOfflineEvaluationWorkerIsComposedWithoutModel(t *testing.T) {
	cfg := config.Config{Model: config.ModelConfig{Name: "test-model", ContextWindow: 8192}}
	composition, err := composeLearning(nil, compositionTreeReader{}, nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	spec, workerHealth, err := newOfflineEvaluationWorkerSpec(composition.service, composition.learningStore, outboxpostgres.New(nil), 0)
	if err != nil {
		t.Fatal(err)
	}
	if spec.name != "offline_evaluation" || spec.runOnce == nil || workerHealth == nil || workerHealth.Probe(context.Background()) != nil {
		t.Fatalf("offline evaluation worker was not composed: spec=%+v health=%v", spec, workerHealth)
	}
}

func TestWorkerHealthTracksFailureAndRecovery(t *testing.T) {
	workerHealth := &workerHealth{}
	trackedFailure := workerHealth.track(func(context.Context) (int, error) { return 0, errors.New("private failure") })
	if _, err := trackedFailure(context.Background()); err == nil || workerHealth.Probe(context.Background()) == nil {
		t.Fatalf("worker failure was not tracked: err=%v probe=%v", err, workerHealth.Probe(context.Background()))
	}
	trackedSuccess := workerHealth.track(func(context.Context) (int, error) { return 1, nil })
	if count, err := trackedSuccess(context.Background()); err != nil || count != 1 || workerHealth.Probe(context.Background()) != nil {
		t.Fatalf("worker recovery was not tracked: count=%d err=%v probe=%v", count, err, workerHealth.Probe(context.Background()))
	}
}

func TestComposeLearningInjectsPostgresKnowledgeAndOptionalModel(t *testing.T) {
	cfg := config.Config{Model: config.ModelConfig{Name: "test-model", ContextWindow: 8192}}
	withoutModel, err := composeLearning(nil, compositionTreeReader{}, nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if withoutModel.learningStore == nil || withoutModel.tutoringStore == nil || withoutModel.resolver == nil || withoutModel.service == nil || withoutModel.model != nil {
		t.Fatalf("nil-model composition=%+v", withoutModel)
	}

	baseURL, _ := url.Parse("http://127.0.0.1:1/v1")
	client, err := llm.New(llm.Options{BaseURL: baseURL, Model: "test-model", APIKey: "test-key", ContextWindow: 8192, MinimumContext: 4096, Timeout: time.Second, ProbeCacheTTL: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	withModel, err := composeLearning(nil, compositionTreeReader{}, client, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if withModel.learningStore == nil || withModel.tutoringStore == nil || withModel.resolver == nil || withModel.service == nil || withModel.model == nil {
		t.Fatalf("model-enabled composition=%+v", withModel)
	}
}
