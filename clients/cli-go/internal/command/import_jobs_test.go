package command

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

func TestImportJobResumeChecksEntireManifestBeforeUpload(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer server.Close()
	docs := []api.ImportDocument{{Path: "a.md", Markdown: "first"}, {Path: "b.md", Markdown: "original"}}
	manifest := jobManifest(docs)
	j := api.ImportJob{ID: "10000000-0000-4000-8000-000000000001", Batches: []api.ImportJobBatch{{Item: manifest[0], Status: "pending"}, {Item: manifest[1], Status: "uploaded"}}}
	docs[1].Markdown = "changed"
	_, err := uploadImportJob(context.Background(), api.NewClient(server.URL, "token", time.Second, nil), j, docs)
	if err == nil || calls != 0 {
		t.Fatalf("本地变化仍上传其他文件: calls=%d err=%v", calls, err)
	}
}
func TestImportJobUILateReplyCannotSwitchTask(t *testing.T) {
	id := "10000000-0000-4000-8000-000000000001"
	m := &importJobsModel{generation: 2, job: &api.ImportJob{ID: id}, view: viewport.New(80, 20)}
	_, cmd := m.Update(jobMessage{generation: 1, value: api.ImportJob{ID: "10000000-0000-4000-8000-000000000002"}})
	if m.job.ID != id || cmd != nil {
		t.Fatal("迟到结果替换了当前任务")
	}
}
