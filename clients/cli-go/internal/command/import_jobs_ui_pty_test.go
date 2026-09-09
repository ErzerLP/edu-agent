//go:build !windows

package command

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/creack/pty"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

func TestImportJobsPTYQueriesAndContinuesPersistentState(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	const id = "10000000-0000-4000-8000-000000000001"
	const collection = "00000000-0000-4000-8000-000000000002"
	j := api.ImportJob{ID: id, Actor: testDeviceID, Space: api.DefaultLearningSpaceID, Collection: collection, Version: 1, PlanVersion: 1, Status: "ready", BatchCount: 1, Created: time.Now(), Expires: time.Now().Add(time.Hour), Batches: []api.ImportJobBatch{{OperationID: id, Status: "ready", Item: api.ImportJobItem{Path: "go.md", Bytes: 10}}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" && r.URL.Path == "/v1/knowledge/import-jobs" {
			header := j
			header.Batches = []api.ImportJobBatch{}
			_ = json.NewEncoder(w).Encode(api.ImportJobPage{Items: []api.ImportJob{header}})
			return
		}
		if r.Method == "POST" {
			var c api.ImportJobCommand
			if json.NewDecoder(r.Body).Decode(&c) != nil {
				w.WriteHeader(400)
				return
			}
			switch c.Action {
			case "confirm":
				if c.PlanVersion != 1 {
					w.WriteHeader(409)
					return
				}
				j.Approved = true
				j.Status = "committing"
			case "continue":
				if !j.Approved {
					w.WriteHeader(409)
					return
				}
				j.Status = "completed"
				j.Approved = false
				j.Batches[0].Status = "completed"
				j.Batches[0].Result = &api.ImportResult{Revision: testRevision(), Summary: &api.ImportSummary{OperationID: id, SpaceID: j.Space, CollectionID: collection, ActorDeviceID: testDeviceID, DocumentIDs: []string{testDocID}, Added: 1}}
			}
			j.Version++
		}
		_ = json.NewEncoder(w).Encode(j)
	}))
	defer server.Close()
	primary, terminal, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	defer terminal.Close()
	if err = pty.Setsize(primary, &pty.Winsize{Rows: 36, Cols: 120}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	m := &importJobsModel{ctx: ctx, client: api.NewClient(server.URL, "test", time.Second, nil).WithCollection(collection), view: viewport.New(116, 28)}
	done := make(chan error, 1)
	go func() {
		_, e := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(terminal), tea.WithOutput(terminal), tea.WithAltScreen()).Run()
		done <- e
	}()
	chunks := make(chan string, 64)
	go func() {
		buffer := make([]byte, 4096)
		for {
			n, e := primary.Read(buffer)
			if n > 0 {
				select {
				case chunks <- string(buffer[:n]):
				case <-ctx.Done():
					return
				}
			}
			if e != nil {
				return
			}
		}
	}()
	output := ""
	wait := func(text string) {
		t.Helper()
		for !strings.Contains(output, text) {
			select {
			case chunk := <-chunks:
				output += chunk
			case e := <-done:
				t.Fatalf("任务页提前退出: %v %q", e, output)
			case <-ctx.Done():
				t.Fatalf("未出现 %q: %q", text, output)
			}
		}
	}
	wait(id)
	_, _ = io.WriteString(primary, "\r")
	wait("预览版本 1")
	_, _ = io.WriteString(primary, "\x13")
	wait("committing")
	_, _ = io.WriteString(primary, "r")
	wait("实际新增 1")
	_, _ = io.WriteString(primary, "\x03")
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-ctx.Done():
		t.Fatal("任务页未退出")
	}
	if !strings.Contains(output, "\x1b[?1049h") {
		t.Fatal("未启动全屏任务页")
	}
}
