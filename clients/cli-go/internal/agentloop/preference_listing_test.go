package agentloop

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

type preferenceConcurrencyServer struct {
	*fakeServer

	mu         sync.Mutex
	started    int
	active     int
	maxActive  int
	startedIDs chan int
	release    []chan struct{}
	cancelOnly bool
}

func newPreferenceConcurrencyServer(count int, cancelOnly bool) *preferenceConcurrencyServer {
	items := make([]api.MemoryExportItem, count)
	release := make([]chan struct{}, count)
	for index := range count {
		items[index] = api.MemoryExportItem{
			Record: api.MemoryRecord{
				LogicalMemoryID: fmt.Sprintf("memory-%d", index),
				CandidateID:     fmt.Sprintf("candidate-%d", index),
				Revision:        int64(index + 1),
			},
			ContentStatus: "available",
			Content:       fmt.Sprintf("preference-%d", index),
		}
		release[index] = make(chan struct{})
	}
	return &preferenceConcurrencyServer{
		fakeServer: &fakeServer{exportResult: api.MemoryExportPage{Items: items}},
		startedIDs: make(chan int, count),
		release:    release,
		cancelOnly: cancelOnly,
	}
}

func (s *preferenceConcurrencyServer) MemoryCandidate(ctx context.Context, candidateID string) (api.MemoryCandidateView, error) {
	index, err := strconv.Atoi(strings.TrimPrefix(candidateID, "candidate-"))
	if err != nil || index < 0 || index >= len(s.release) {
		return api.MemoryCandidateView{}, fmt.Errorf("invalid candidate ID %q", candidateID)
	}
	s.mu.Lock()
	s.started++
	s.active++
	if s.active > s.maxActive {
		s.maxActive = s.active
	}
	s.mu.Unlock()
	s.startedIDs <- index

	if s.cancelOnly {
		<-ctx.Done()
	} else {
		select {
		case <-s.release[index]:
		case <-ctx.Done():
		}
	}

	s.mu.Lock()
	s.active--
	s.mu.Unlock()
	if ctx.Err() != nil {
		return api.MemoryCandidateView{}, ctx.Err()
	}
	return api.MemoryCandidateView{Candidate: api.MemoryCandidate{
		ID:          candidateID,
		Category:    "interaction_preference",
		Sensitivity: "non_sensitive",
		Stability:   "stable",
		ValidUntil:  time.Date(2036, 1, 1, 0, 0, 0, 0, time.UTC),
	}}, nil
}

func (s *preferenceConcurrencyServer) snapshotCounts() (started, active, maximum int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started, s.active, s.maxActive
}

func TestPreferenceMetadataUsesBoundedConcurrencyStableOrderAndProgress(t *testing.T) {
	server := newPreferenceConcurrencyServer(6, false)
	model := &scriptedCompleteModel{steps: []completeStep{
		func(context.Context, modelclient.Request) (modelclient.Response, error) {
			return modelclient.Response{Message: toolMessage("preferences-call", "list_long_term_preferences", `{}`)}, nil
		},
		func(context.Context, modelclient.Request) (modelclient.Response, error) {
			return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "偏好读取完成"}}, nil
		},
	}}
	session := newTestSession(t, model, server)
	var progressMu sync.Mutex
	var progress []ActivityProgress
	ctx := WithActivityReporter(t.Context(), func(activity Activity) {
		if activity.Event.ID == "preferences-call" && activity.Progress != nil {
			progressMu.Lock()
			progress = append(progress, *activity.Progress)
			progressMu.Unlock()
		}
	})
	resultCh := make(chan struct {
		result Result
		err    error
	}, 1)
	go func() {
		result, err := session.Send(ctx, "读取长期偏好")
		resultCh <- struct {
			result Result
			err    error
		}{result: result, err: err}
	}()

	started := make(map[int]struct{})
	for len(started) < 4 {
		select {
		case index := <-server.startedIDs:
			started[index] = struct{}{}
		case <-time.After(time.Second):
			t.Fatalf("only %d metadata requests started", len(started))
		}
	}
	if count, _, maximum := server.snapshotCounts(); count != 4 || maximum != 4 {
		t.Fatalf("initial metadata concurrency started=%d max=%d", count, maximum)
	}
	for _, index := range []int{3, 2, 1, 0} {
		close(server.release[index])
	}
	for len(started) < 6 {
		select {
		case index := <-server.startedIDs:
			started[index] = struct{}{}
		case <-time.After(time.Second):
			t.Fatalf("only %d metadata requests started", len(started))
		}
	}
	close(server.release[5])
	close(server.release[4])

	select {
	case outcome := <-resultCh:
		if outcome.err != nil || outcome.result.Text != "偏好读取完成" {
			t.Fatalf("result=%+v err=%v", outcome.result, outcome.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("preference listing did not finish")
	}
	requests := model.snapshotRequests()
	if len(requests) != 2 {
		t.Fatalf("model requests=%d", len(requests))
	}
	value := decodedToolResult(t, requests[1].Messages, "preferences-call")
	rawItems, ok := value["items"].([]any)
	if !ok || len(rawItems) != 6 {
		t.Fatalf("items=%#v", value["items"])
	}
	for index, raw := range rawItems {
		item := raw.(map[string]any)
		if item["memory_id"] != fmt.Sprintf("memory-%d", index) || item["content"] != fmt.Sprintf("preference-%d", index) {
			t.Fatalf("item %d out of order: %+v", index, item)
		}
	}
	_, _, maximum := server.snapshotCounts()
	if maximum > maxPreferenceMetadataConcurrency {
		t.Fatalf("metadata concurrency=%d limit=%d", maximum, maxPreferenceMetadataConcurrency)
	}
	progressMu.Lock()
	defer progressMu.Unlock()
	if len(progress) != 6 {
		t.Fatalf("progress=%+v", progress)
	}
	for index, current := range progress {
		if current.Completed != index+1 || current.Total != 6 {
			t.Fatalf("progress[%d]=%+v", index, current)
		}
	}
}

func TestPreferenceMetadataCancellationStopsSchedulingAndNextModel(t *testing.T) {
	server := newPreferenceConcurrencyServer(10, true)
	model := &scriptedCompleteModel{steps: []completeStep{
		func(context.Context, modelclient.Request) (modelclient.Response, error) {
			return modelclient.Response{Message: toolMessage("preferences-call", "list_long_term_preferences", `{}`)}, nil
		},
	}}
	session := newTestSession(t, model, server)
	ctx, cancel := context.WithCancel(t.Context())
	resultCh := make(chan error, 1)
	go func() {
		_, err := session.Send(ctx, "读取长期偏好")
		resultCh <- err
	}()
	for index := 0; index < maxPreferenceMetadataConcurrency; index++ {
		select {
		case <-server.startedIDs:
		case <-time.After(time.Second):
			t.Fatalf("metadata request %d did not start", index+1)
		}
	}
	cancel()
	select {
	case err := <-resultCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled preference listing did not stop")
	}
	started, active, maximum := server.snapshotCounts()
	if started != maxPreferenceMetadataConcurrency || active != 0 || maximum > maxPreferenceMetadataConcurrency {
		t.Fatalf("metadata counts started=%d active=%d max=%d", started, active, maximum)
	}
	if len(model.snapshotRequests()) != 1 {
		t.Fatalf("model continued after cancellation: requests=%d", len(model.snapshotRequests()))
	}
}

func TestPreferenceMetadataPartialFailureRemainsDegraded(t *testing.T) {
	server := &selectivePreferenceServer{
		fakeServer: &fakeServer{exportResult: api.MemoryExportPage{Items: []api.MemoryExportItem{
			{Record: api.MemoryRecord{LogicalMemoryID: "memory-0", CandidateID: "candidate-0", Revision: 1}, ContentStatus: "available", Content: "keep"},
			{Record: api.MemoryRecord{LogicalMemoryID: "memory-1", CandidateID: "candidate-1", Revision: 1}, ContentStatus: "available", Content: "drop"},
		}}},
	}
	model := &fakeModel{responses: []modelclient.Response{
		{Message: toolMessage("preferences-call", "list_long_term_preferences", `{}`)},
		{Message: modelclient.Message{Role: "assistant", Content: "完成"}},
	}}
	session := newTestSession(t, model, server)
	if _, err := session.Send(t.Context(), "读取偏好"); err != nil {
		t.Fatal(err)
	}
	value := decodedToolResult(t, model.requests[1].Messages, "preferences-call")
	if value["degraded"] != true {
		t.Fatalf("degraded=%#v", value)
	}
	codes := value["reason_codes"].([]any)
	if len(codes) != 1 || codes[0] != "candidate_metadata_unavailable" {
		t.Fatalf("reason_codes=%+v", codes)
	}
	items := value["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["content"] != "keep" {
		t.Fatalf("items=%+v", items)
	}
}

type selectivePreferenceServer struct {
	*fakeServer
}

func (s *selectivePreferenceServer) MemoryCandidate(_ context.Context, candidateID string) (api.MemoryCandidateView, error) {
	if candidateID == "candidate-1" {
		return api.MemoryCandidateView{}, fmt.Errorf("metadata unavailable")
	}
	return api.MemoryCandidateView{Candidate: api.MemoryCandidate{
		ID: candidateID, Category: "interaction_preference", Sensitivity: "non_sensitive", Stability: "stable",
		ValidUntil: time.Date(2036, 1, 1, 0, 0, 0, 0, time.UTC),
	}}, nil
}
