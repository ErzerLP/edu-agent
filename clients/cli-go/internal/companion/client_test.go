package companion

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	wire "github.com/edu-agent/edu-agent/packages/agentcore/companion"
)

func TestOutboundBridgeRealProviderAndLostDelivery(t *testing.T) {
	p, g := native(t)
	var broker *wire.Broker
	var lose atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "http://"+r.Host {
			w.WriteHeader(403)
			return
		}
		if r.URL.Path == "/v1/companion/attach" {
			var a wire.Attach
			json.NewDecoder(r.Body).Decode(&a)
			c, err := broker.Attach(a)
			if err != nil {
				w.WriteHeader(403)
				return
			}
			json.NewEncoder(w).Encode(c)
			return
		}
		body, _ := io.ReadAll(r.Body)
		seq, _ := strconv.ParseUint(r.Header.Get("X-Companion-Sequence"), 10, 64)
		d, err := broker.Exchange(r.Header.Get("X-Companion-ID"), seq, r.Header.Get("X-Companion-MAC"), body)
		if err != nil {
			w.WriteHeader(403)
			return
		}
		if d.Operation != nil && lose.Swap(false) {
			w.WriteHeader(503)
			return
		}
		json.NewEncoder(w).Encode(d)
	}))
	defer server.Close()
	broker = wire.New(server.URL)
	pair, err := broker.Create("browser", "web-device")
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(server.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	d := p.device
	d.Host = "host"
	d.User = "student"
	d.OS = "linux"
	channel, err := client.Attach(t.Context(), wire.Attach{ID: pair.ID, Code: pair.Code, Device: d})
	if err != nil {
		t.Fatal(err)
	}
	g.Space = "space"
	if err = broker.Authorize("browser", pair.ID, pair.Secret, d.ID, g); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx, pair.ID, channel.Token, p) }()
	defer func() { cancel(); <-done }()
	command := func(id, path string) {
		t.Helper()
		args, _ := json.Marshal(map[string]any{"command": "printf '真实执行' > " + path, "wait_ms": 0})
		if _, err := broker.Submit("browser", pair.ID, pair.Secret, wire.Operation{ID: id, Run: "run", Tool: "shell", Arguments: args}); err != nil {
			t.Fatal(err)
		}
	}
	command("first", "actual")
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, _ := broker.Receipt("browser", pair.ID, pair.Secret, "first")
		if r.State == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("主动通道未结算")
		}
		time.Sleep(10 * time.Millisecond)
	}
	data, err := os.ReadFile(filepath.Join(p.device.Workspace, "actual"))
	if err != nil || string(data) != "真实执行" {
		t.Fatal("通道未调用真实 provider", err)
	}
	lose.Store(true)
	command("lost", "must-not-exist")
	deadline = time.Now().Add(5 * time.Second)
	for lose.Load() {
		if time.Now().After(deadline) {
			t.Fatal("未触发丢包")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(1200 * time.Millisecond)
	if _, err = os.Stat(filepath.Join(p.device.Workspace, "must-not-exist")); !os.IsNotExist(err) {
		t.Fatal("丢失投递后自动重跑")
	}
	if err = broker.Revoke("browser", pair.ID, pair.Secret); err != nil {
		t.Fatal(err)
	}
}

func TestClientRejectsRemoteHTTPAndRedirect(t *testing.T) {
	for _, origin := range []string{"http://example.com", "http://localhost:8080", "https://user:pass@example.com", "https://example.com/path", "https://example.com?token=secret"} {
		if _, err := NewClient(origin, true); err == nil {
			t.Fatalf("接受不安全站点 %s", origin)
		}
	}
	var forwarded atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Store(true) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer redirect.Close()
	c, err := NewClient(redirect.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Attach(t.Context(), wire.Attach{ID: "pair", Code: "private"}); err == nil || forwarded.Load() {
		t.Fatal("跨地址转发配对凭据")
	}
	u, _ := url.Parse(redirect.URL)
	if c.Origin != "http://"+u.Host {
		t.Fatal("Origin 规范化错误")
	}
}
