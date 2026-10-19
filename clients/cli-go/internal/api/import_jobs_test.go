package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestImportJobTransportPreservesSingleFileBudgetAndDoesNotRetryUnknown(t *testing.T) {
	const id = "10000000-0000-4000-8000-000000000001"
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var c ImportJobCommand
		if json.Unmarshal(raw, &c) != nil {
			t.Fatal("请求无效")
		}
		if c.Action == "upload" {
			if len(raw) > 16<<20 || c.ContentBase64 == "" || c.Document != nil {
				t.Fatal("合法单文件超出请求预算")
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(ImportJob{ID: id, Actor: id, Space: DefaultLearningSpaceID, Collection: "00000000-0000-4000-8000-000000000002", Version: 1, Batches: []ImportJobBatch{{OperationID: id, Status: "uploaded"}}})
			return
		}
		connection, _, e := w.(http.Hijacker).Hijack()
		if e == nil {
			connection.Close()
		}
	}))
	defer server.Close()
	// 等待真实 HTTP 响应；期限只防止整个测试挂死，不约束大请求的处理速度。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	c := NewClient(server.URL, "token", time.Until(deadline), nil)
	if _, err := c.RunImportJob(ctx, ImportJobCommand{ID: id, Action: "upload", Document: &ImportDocument{Path: "file.md", Markdown: strings.Repeat("<", 4<<20)}}); err != nil {
		t.Fatal(err)
	}
	_, err := c.RunImportJob(ctx, ImportJobCommand{ID: id, Action: "continue"})
	var transportErr *TransportError
	if !errors.As(err, &transportErr) || transportErr.Category != "connection_failed" {
		t.Fatalf("丢失响应应报告连接断开，不能由超时替代: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("未知结果自动重发: %d", calls.Load())
	}
}
