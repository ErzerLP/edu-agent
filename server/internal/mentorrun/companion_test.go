package mentorrun

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/packages/agentcore/companion"
	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/google/uuid"
)

func TestCompanionArgumentsCannotEnterHistory(t *testing.T) {
	m := modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: "native-call", Type: "function", Function: modelclient.ToolFunction{Name: "local_operation", Arguments: `{"tool":"shell","arguments":{"command":"私密命令","env":{"KEY":"私密环境"},"input":"私密输入"}}`}}}}
	redacted := redactLocalCalls(m)
	raw, _ := json.Marshal(redacted)
	if strings.Contains(string(raw), "私密") || !strings.Contains(string(raw), "native-call") {
		t.Fatal("原始本地参数进入历史或丢失调用身份")
	}
	if !strings.Contains(m.ToolCalls[0].Function.Arguments, "私密命令") {
		t.Fatal("脱敏破坏了实际执行参数")
	}
	var b *companion.Broker
	if b.ModelConnection("token", "device", "conversation", "model") != "" {
		t.Fatal("无 provider 注册工具")
	}
}

func TestPostgreSQLCompanionModelDispatchAndPrivateArguments(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		raw, _ := io.ReadAll(r.Body)
		if n == 1 {
			if !strings.Contains(string(raw), "local_operation") {
				t.Error("已授权的本地工具没有注册")
			}
			stream(w, modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: "local-once", Type: "function", Function: modelclient.ToolFunction{Name: "local_operation", Arguments: `{"tool":"shell","arguments":{"command":"私密命令","env":{"KEY":"私密环境"}}}`}}}})
		} else {
			if strings.Contains(string(raw), "私密命令") || strings.Contains(string(raw), "私密环境") {
				t.Error("原始参数进入后续历史")
			}
			if !strings.Contains(string(raw), "设备真实回执") {
				t.Error("模型没有读取设备回执")
			}
			stream(w, modelclient.Message{Role: "assistant", Content: "已取得设备回执"})
		}
	})
	id := newHistory(t, f, true, f.goal)
	c := historyPage(t, f, id, 0, 1).Conversation
	b := companion.New("https://learn.example")
	f.service.ConfigureCompanion(b)
	pair, err := b.Create(f.actor.TokenID, f.actor.Device.ID)
	if err != nil {
		t.Fatal(err)
	}
	channel, err := b.Attach(companion.Attach{ID: pair.ID, Code: pair.Code, Device: companion.Device{ID: "native", Host: "host", User: "student", OS: "linux", Workspace: "/work"}})
	if err != nil {
		t.Fatal(err)
	}
	g := companion.Grant{Generation: c.Generation, Space: c.SpaceID, Conversation: c.ID, Shell: true, Model: true, Destination: c.Destination}
	if err = b.Authorize(f.actor.TokenID, pair.ID, pair.Secret, "native", g); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	operations := make(chan companion.Operation, 1)
	go func() {
		defer close(done)
		var seq uint64
		var result *companion.Receipt
		for {
			seq++
			body, _ := json.Marshal(companion.Exchange{Receipt: result})
			d, e := b.Exchange(pair.ID, seq, companion.MAC(channel.Token, "https://learn.example", pair.ID, seq, body), body)
			if e != nil {
				return
			}
			result = nil
			if d.Operation != nil {
				operations <- *d.Operation
				result = &companion.Receipt{ID: d.Operation.ID, Device: "native", Conversation: c.ID, Run: d.Operation.Run, State: "completed", Value: json.RawMessage(`{"evidence":"设备真实回执"}`)}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	defer func() { cancel(); <-done }()
	run := sendHistory(t, f, id, "使用已授权本机")
	f.work(t)
	select {
	case op := <-operations:
		if op.Run != run.RunID || !strings.Contains(string(op.Arguments), "私密命令") {
			t.Fatal("实际本机调用参数或运行身份丢失")
		}
	default:
		t.Fatal("未发送真实通道操作")
	}
	page := historyPage(t, f, id, 0, 20)
	raw, _ := json.Marshal(page)
	if strings.Contains(string(raw), "私密命令") || strings.Contains(string(raw), "私密环境") || len(page.Items) != 1 || page.Items[0].Status != "succeeded" {
		t.Fatalf("持久历史或运行结算错误：%s", raw)
	}
	if err = b.Revoke(f.actor.TokenID, pair.ID, pair.Secret); err != nil {
		t.Fatal(err)
	}
	if b.ModelConnection(f.actor.TokenID, f.actor.Device.ID, c.ID, c.Destination) != "" {
		t.Fatal("撤销后仍注册模型工具")
	}
}

func TestPostgreSQLCompanionConversationAndRevocationGates(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		stream(w, modelclient.Message{Role: "assistant", Content: "完成"})
	})
	id := newHistory(t, f, true, f.goal)
	c := historyPage(t, f, id, 0, 1).Conversation
	g := companion.Grant{Generation: c.Generation, Space: c.SpaceID, Conversation: c.ID, Files: true, Shell: true, Model: true, Destination: c.Destination}
	ctx := context.Background()
	if err := f.service.CheckCompanion(ctx, f.actor, &g); err != nil {
		t.Fatal(err)
	}
	wrong := g
	wrong.Conversation = uuid.NewString()
	if err := f.service.CheckCompanion(ctx, f.actor, &wrong); err == nil {
		t.Fatal("未核对真实对话")
	}
	wrong = g
	wrong.Generation++
	if err := f.service.CheckCompanion(ctx, f.actor, &wrong); err == nil {
		t.Fatal("未核对隐私代次")
	}
	wrong = g
	wrong.Destination = "other"
	if err := f.service.CheckCompanion(ctx, f.actor, &wrong); err == nil {
		t.Fatal("模型目的地变化后继续外发")
	}
	actor := identity.Credential{TokenID: f.actor.TokenID, Device: identity.Device{ID: uuid.NewString()}}
	if err := f.service.CheckCompanion(ctx, actor, &g); err == nil {
		t.Fatal("设备置换绕过身份")
	}
	if _, err := f.pool.Exec(ctx, `UPDATE devices SET revoked_at=clock_timestamp() WHERE id=$1`, f.actor.Device.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.service.CheckCompanion(ctx, f.actor, &g); err == nil {
		t.Fatal("设备撤销后仍有 OS 权限")
	}
}
