package agentcontroller

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentcontext"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

func TestRejectedLearningBindingReleasesStore(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	for _, binding := range []agentcontext.Binding{{SpaceID: "非法学习区"}, {SpaceID: "10000000-0000-4000-8000-000000000001"}} {
		store := controllerStore(t, t.TempDir(), &controllerSecretBackend{})
		defer store.Close()
		deps := controllerDependencies(store, &controllerModel{}, api.NewClient(server.URL, "test", time.Second, nil), t.TempDir(), Provider{})
		deps.LoopOptions.LearningBinding = binding
		if _, err := Start(t.Context(), deps, false); err == nil {
			t.Fatal("接受了非法或不可用的学习区")
		}
		if _, err := store.List(t.Context()); err == nil {
			t.Fatal("拒绝绑定后仍持有存储")
		}
	}
}

func TestLearningBindingSurvivesResumeSwitchAndNoSave(t *testing.T) {
	root, work := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
	provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}
	a := agentcontext.Binding{SpaceID: "10000000-0000-4000-8000-000000000001", GoalID: "20000000-0000-4000-8000-000000000001"}
	b := agentcontext.Binding{SpaceID: "10000000-0000-4000-8000-000000000002"}
	deps := controllerDependencies(controllerStore(t, root, secrets), &controllerModel{}, server, work, provider)
	deps.LoopOptions.LearningBinding = a
	current, err := Start(t.Context(), deps, false)
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	if _, err = current.Send(t.Context(), "Go 并发"); err != nil {
		t.Fatal(err)
	}
	deps = controllerDependencies(controllerStore(t, root, secrets), &controllerModel{}, server, work, provider)
	deps.LoopOptions.LearningBinding = b
	target, err := Start(t.Context(), deps, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = target.Send(t.Context(), "英语练习"); err != nil {
		t.Fatal(err)
	}
	targetID := target.SessionID()
	if err = target.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	items, err := current.ListSessions(t.Context(), SessionListRequest{})
	if err != nil || len(items) != 1 || items[0].Summary.LearningBinding != a {
		t.Fatalf("默认筛选泄漏其他区：%+v %v", items, err)
	}
	items, err = current.ListSessions(t.Context(), SessionListRequest{All: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Summary.SessionID != targetID {
			continue
		}
		if _, err = current.CommitSwitch(t.Context(), current.PlanSwitch(item.Summary), SwitchConfirmation{}); err != nil {
			t.Fatal(err)
		}
	}
	if current.LearningBinding() != b || current.SessionID() != targetID {
		t.Fatal("恢复未安装原学习身份")
	}
	for _, entry := range current.SessionTranscript().Entries {
		if entry.Text == "Go 并发" {
			t.Fatal("跨区携带了旧正文")
		}
	}
	deps = controllerDependencies(nil, &controllerModel{}, server, work, provider)
	deps.LoopOptions.LearningBinding = a
	unsaved, err := Start(t.Context(), deps, true)
	if err != nil {
		t.Fatal(err)
	}
	defer unsaved.Close()
	if unsaved.LearningBinding() != a || unsaved.Status().Persistent || unsaved.LearningWorkspaceRoot() != work {
		t.Fatal("no-save 丢失绑定或意外保存")
	}
}

func TestRebindingReplacesClientAndModelGuard(t *testing.T) {
	base := &controllerModel{}
	d := Dependencies{Model: base, Server: api.NewClient("http://127.0.0.1:1", "token", time.Second, nil), LoopOptions: agentloop.Options{}}
	first := agentcontext.Binding{SpaceID: "10000000-0000-4000-8000-000000000001"}
	second := agentcontext.Binding{SpaceID: "10000000-0000-4000-8000-000000000002"}
	if err := bindLearningDependencies(t.Context(), &d, first); err != nil {
		t.Fatal(err)
	}
	old := d.Server.(*agentcontext.Client)
	if err := bindLearningDependencies(t.Context(), &d, second); err != nil {
		t.Fatal(err)
	}
	guard := d.Model.(*agentcontext.GuardedModel)
	if old.Binding != first || guard.Client.Binding != second || guard.Base != base || guard.Client != d.Server {
		t.Fatal("绑定修改了旧请求或模型保留了旧区守卫")
	}
}
