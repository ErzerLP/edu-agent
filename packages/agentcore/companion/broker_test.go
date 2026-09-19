package companion

import (
	"encoding/json"
	"testing"
	"time"
)

func paired(t *testing.T) (*Broker, Pair, Channel, Grant) {
	t.Helper()
	b := New("https://learn.example")
	p, err := b.Create("web-token", "web-device")
	if err != nil {
		t.Fatal(err)
	}
	c, err := b.Attach(Attach{ID: p.ID, Code: p.Code, Device: Device{ID: "native", Host: "电脑", User: "student", OS: "linux", Workspace: "/work"}})
	if err != nil {
		t.Fatal(err)
	}
	g := Grant{Generation: 1, Space: "space", Conversation: "conversation", Files: true, Shell: true, Model: true, Destination: "model"}
	return b, p, c, g
}

func exchange(t *testing.T, b *Broker, p Pair, c Channel, seq uint64, r *Receipt) Delivery {
	t.Helper()
	body, _ := json.Marshal(Exchange{Receipt: r})
	d, err := b.Exchange(p.ID, seq, MAC(c.Token, b.origin, p.ID, seq, body), body)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestPairAuthorizationAndChannelBoundary(t *testing.T) {
	b, p, c, g := paired(t)
	op := Operation{ID: "op", Tool: "shell", Arguments: json.RawMessage(`{"command":"true"}`)}
	if _, err := b.Submit("web-token", p.ID, p.Secret, op); err == nil {
		t.Fatal("配对未授权却能执行")
	}
	if _, err := b.Attach(Attach{ID: p.ID, Code: p.Code, Device: Device{ID: "other", Host: "h", User: "u", OS: "linux", Workspace: "/w"}}); err == nil {
		t.Fatal("配对码可重放")
	}
	for _, args := range [][4]string{{"other", p.ID, p.Secret, "native"}, {"web-token", p.ID, "cookie", "native"}, {"web-token", p.ID, p.Secret, "wrong-device"}} {
		if err := b.Authorize(args[0], args[1], args[2], args[3], g); err == nil {
			t.Fatal("错误身份获得授权")
		}
	}
	if err := b.Authorize("web-token", p.ID, p.Secret, "native", g); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Submit("web-token", p.ID, p.Secret, op); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{}`)
	if _, err := b.Exchange(p.ID, 1, MAC(c.Token, "https://evil.example", p.ID, 1, body), body); err == nil {
		t.Fatal("Origin 置换获得权限")
	}
	if _, err := b.Exchange(p.ID, 1, MAC("cookie", b.origin, p.ID, 1, body), body); err == nil {
		t.Fatal("Cookie 冒充通道")
	}
	first := exchange(t, b, p, c, 1, nil)
	if first.Operation == nil {
		t.Fatal("没有实际投递")
	}
	if _, err := b.Exchange(p.ID, 1, MAC(c.Token, b.origin, p.ID, 1, body), body); err == nil {
		t.Fatal("通道序号可重放")
	}
	if second := exchange(t, b, p, c, 2, nil); second.Operation != nil {
		t.Fatal("丢失响应后重投命令")
	}
	if id := b.ModelConnection("web-token", "wrong-device", g.Conversation, g.Destination); id != "" {
		t.Fatal("跨浏览器工具")
	}
	if id := b.ModelConnection("web-token", "web-device", "other", g.Destination); id != "" {
		t.Fatal("跨对话工具")
	}
	if id := b.ModelConnection("other-token", "web-device", g.Conversation, g.Destination); id != "" {
		t.Fatal("跨浏览器会话授权")
	}
	if _, err := b.ModelSubmit(p.ID, "web-token", "web-device", g.Conversation, g.Destination, Operation{ID: "approve", Tool: "commit", Arguments: json.RawMessage(`{"plan":"x"}`)}); err == nil {
		t.Fatal("模型批准文件")
	}
}

func TestReceiptRecoveryNeverReplaysInput(t *testing.T) {
	b, p, c, g := paired(t)
	if err := b.Authorize("web-token", p.ID, p.Secret, "native", g); err != nil {
		t.Fatal(err)
	}
	op := Operation{ID: "input", Run: "run", Tool: "task", Arguments: json.RawMessage(`{"action":"input","task_id":"task","content":"私密输入"}`)}
	r, err := b.Submit("web-token", p.ID, p.Secret, op)
	if err != nil {
		t.Fatal(err)
	}
	exchange(t, b, p, c, 1, nil)
	r.State = "completed"
	r.Value = json.RawMessage(`{"Written":3,"Outcome":"partial"}`)
	exchange(t, b, p, c, 2, &r)
	if d := exchange(t, b, p, c, 3, &r); d.Operation != nil {
		t.Fatal("重复结算重放输入")
	}
	actual, err := b.Submit("web-token", p.ID, p.Secret, op)
	if err != nil || string(actual.Value) != string(r.Value) {
		t.Fatalf("没有保留部分输入回执：%+v %v", actual, err)
	}
	op.Arguments = json.RawMessage(`{"action":"input","content":"changed"}`)
	if _, err = b.Submit("web-token", p.ID, p.Secret, op); err != ErrConflict {
		t.Fatal("同身份换载荷被接受")
	}
	restarted := New(b.origin)
	if restarted.Verify(p.ID, 4, MAC(c.Token, b.origin, p.ID, 4, []byte(`{}`)), []byte(`{}`)) {
		t.Fatal("重启恢复旧凭据")
	}
}

func TestLeaseRevocationAndMemoryBounds(t *testing.T) {
	b, p, c, g := paired(t)
	if err := b.Authorize("web-token", p.ID, p.Secret, "native", g); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Submit("web-token", p.ID, p.Secret, Operation{ID: "pending", Tool: "shell", Arguments: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	exchange(t, b, p, c, 1, nil)
	now := time.Now().Add(Lease + time.Second)
	b.now = func() time.Time { return now }
	v, err := b.View("web-token", p.ID, p.Secret)
	if err != nil || v.State != "revoked" || v.Receipts[0].State != "unknown" {
		t.Fatalf("过期伪装成功：%+v %v", v, err)
	}
	if b.ModelConnection("web-token", "web-device", g.Conversation, g.Destination) != "" {
		t.Fatal("过期仍注册工具")
	}
	body := []byte(`{}`)
	if _, err = b.Exchange(p.ID, 2, MAC(c.Token, b.origin, p.ID, 2, body), body); err == nil {
		t.Fatal("过期通道恢复授权")
	}
	for i := 0; i < 15; i++ {
		if _, err = b.Create("owner", "device"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = b.Create("owner", "device"); err != ErrLimit {
		t.Fatal("连接内存没有上限")
	}
}
