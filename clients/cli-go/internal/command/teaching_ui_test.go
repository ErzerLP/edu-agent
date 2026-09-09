package command

import (
	"context"
	"errors"
	"io"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

type delayedTeachingClient struct {
	APIClient
	started   chan struct{}
	release   chan struct{}
	returned  chan struct{}
	failure   error
	refetches atomic.Int32
}

func (c *delayedTeachingClient) KnowledgeHead(context.Context) (api.KnowledgeRevision, error) {
	return testRevision(), nil
}
func (c *delayedTeachingClient) RetrieveKnowledge(context.Context, api.KnowledgeRetrievalRequest) (api.KnowledgeRetrievalResult, error) {
	return commandRetrieval(false, false), nil
}
func (c *delayedTeachingClient) CreateProposal(context.Context, api.TutoringProposalRequest) (api.TutoringProposal, error) {
	close(c.started)
	<-c.release
	close(c.returned)
	return api.TutoringProposal{}, c.failure
}
func (c *delayedTeachingClient) Session(context.Context, string) (api.SessionView, error) {
	c.refetches.Add(1)
	return api.SessionView{}, errors.New("取消后不应继续刷新")
}

func TestTeachingF2DetachesLateOriginResults(t *testing.T) {
	for _, failure := range []error{nil, &api.APIError{Code: "model_unavailable"}, context.Canceled} {
		t.Run(map[bool]string{true: "迟到成功", false: "迟到失败"}[failure == nil], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			input, writer := io.Pipe()
			defer input.Close()
			defer writer.Close()
			client := &delayedTeachingClient{started: make(chan struct{}), release: make(chan struct{}), returned: make(chan struct{}), failure: failure}
			app := &App{teachingInput: input, teachingOutput: io.Discard, InputIsTTY: func() bool { return true }, OutputIsTTY: func() bool { return true }, Getenv: func(string) string { return "xterm" }, Out: io.Discard, Err: io.Discard, NewUUID: func() (string, error) { return "10000000-0000-4000-8000-000000000001", nil }}
			result := make(chan error, 1)
			go func() {
				result <- app.runTeachingLoop(ctx, client, commandSessionView("Diagnostic", "open", "", false, false))
			}()
			select {
			case <-client.started:
			case err := <-result:
				t.Fatalf("界面提前退出：%v", err)
			case <-ctx.Done():
				t.Fatal("模型请求未发出")
			}
			if _, err := writer.Write([]byte("\x1bOQ")); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-result:
				if !errors.Is(err, errSelectTeachingSession) {
					t.Fatalf("F2 未切换：%v", err)
				}
			case <-ctx.Done():
				t.Fatal("切换被 A 请求阻塞")
			}
			app.learningSpace = "10000000-0000-4000-8000-000000000002"
			close(client.release)
			<-client.returned
			if client.refetches.Load() != 0 {
				t.Fatal("迟到结果发出了后续刷新")
			}
		})
	}
}

func TestTeachingDraftsAreIsolatedAndProcessLocal(t *testing.T) {
	app := &App{Terminal: &fakeTerminal{lines: []string{"Go 草稿", ":switch", "English draft", ":switch", "."}}, Out: io.Discard, Err: io.Discard}
	if _, err := app.readMultilineAnswer("A/question"); !errors.Is(err, errSelectTeachingSession) {
		t.Fatal(err)
	}
	if _, err := app.readMultilineAnswer("B/question"); !errors.Is(err, errSelectTeachingSession) {
		t.Fatal(err)
	}
	answer, err := app.readMultilineAnswer("A/question")
	if err != nil || answer != "Go 草稿" || app.learningDrafts["B/question"][0] != "English draft" {
		t.Fatalf("草稿串题：%q %v", answer, err)
	}
	restarted := &App{}
	if len(restarted.learningDrafts) != 0 {
		t.Fatal("草稿越过进程边界")
	}
}

func TestTeachingInputDraftFollowsIssuedActivity(t *testing.T) {
	input := textinput.New()
	input.SetValue("第一题正在编辑")
	m := &teachingModel{input: input, draftKey: "session/activity-A", drafts: map[string]string{"session/activity-B": "第二题草稿"}}
	m.Update(teachingFocus{"session/activity-B"})
	if m.input.Value() != "第二题草稿" || m.drafts["session/activity-A"] != "第一题正在编辑" {
		t.Fatal("同会话换题覆盖草稿")
	}
	m.Update(teachingFocus{"session/activity-A"})
	if m.input.Value() != "第一题正在编辑" {
		t.Fatal("恢复原题未找回编辑内容")
	}
}

func TestNativeTeachingDraftSwitch(t *testing.T) {
	if os.Getenv("EDU_AGENT_NATIVE_TEACHING_TEST") != "1" {
		t.Skip("仅在原生终端人工操作时运行")
	}
	app := &App{teachingInput: os.Stdin, teachingOutput: os.Stdout, InputIsTTY: func() bool { return true }, OutputIsTTY: func() bool { return true }, Getenv: os.Getenv, Out: io.Discard, Err: io.Discard}
	a := commandSessionView("AwaitingResponse", "open", "", false, false)
	b := commandSessionView("AwaitingResponse", "open", "", false, false)
	b.Session.SessionID = "11000000-0000-4000-8000-000000000002"
	for _, view := range []api.SessionView{a, b, a} {
		if err := app.runTeachingLoop(t.Context(), &sessionRefreshClient{source: view}, view); !errors.Is(err, errSelectTeachingSession) {
			t.Fatalf("原生切换失败：%v", err)
		}
	}
	if app.learningDrafts[a.Session.SessionID+"/"+commandActivityID][0] != "Go draft" || app.learningDrafts[b.Session.SessionID+"/"+commandActivityID][0] != "English draft" {
		t.Fatal("原生终端草稿串题")
	}
}
