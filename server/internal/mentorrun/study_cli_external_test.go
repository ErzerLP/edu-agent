package mentorrun_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	identitydb "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/integrations/learningknowledge"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningchange"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	spacedb "github.com/edu-agent/edu-agent/server/internal/learningspace/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningstart"
	"github.com/edu-agent/edu-agent/server/internal/mentorrun"
	"github.com/edu-agent/edu-agent/server/internal/platform/health"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/edu-agent/edu-agent/server/internal/transport/httpapi"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/google/uuid"
)

type studyReady struct{}

func (studyReady) Ready(context.Context) health.Report { return health.Report{} }

// 使用真实生产 CLI、HTTP、PG 与搜索/正文/模型 fixture；Web 端走真实 Cookie/CSRF。
func TestPostgreSQLStudyCLIResearchAndWebContent(t *testing.T) {
	binary := os.Getenv("EDU_AGENT_INTEGRATION_CLI")
	if binary == "" {
		t.Skip("需要生产 CLI 二进制 EDU_AGENT_INTEGRATION_CLI")
	}
	f := mentorrun.NewStudyFixture(t)
	ctx := t.Context()
	ids, err := identity.NewService(identitydb.New(f.Pool), identity.Options{PairingCodeTTL: time.Minute, PairingCodeMaxAttempts: 5, LastUsedTouchInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ks, err := knowledge.NewService(f.Starter.Knowledge, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ls, err := learning.NewService(f.Starter.Learning, f.Starter.Learning, learningknowledge.New(ks), learning.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	origin := "http://" + server.Listener.Addr().String()
	base, _ := url.Parse(origin)
	handler, err := httpapi.New(httpapi.Options{Identity: ids, Learning: ls, LearningSpaces: spacedb.New(f.Pool), Knowledge: ks, LearningContent: f.Starter.Content, LearningChanges: f.Changes, MentorRuns: f.Runtime, Settings: f.Settings, Readiness: studyReady{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), PairLimiter: httpapi.NewFixedWindowLimiter(1000, time.Minute), AuthLimiter: httpapi.NewFixedWindowLimiter(1000, time.Minute), DeviceLimiter: httpapi.NewFixedWindowLimiter(1000, time.Minute), WebUI: httpapi.WebUIOptions{Enabled: true, AllowLoopbackHTTP: true, PublicBaseURL: base, Identity: ids, Assets: fstest.MapFS{"index.html": {Data: []byte(`<script type="module" src="/app/assets/main.js"></script>`)}, "assets/main.js": {Data: []byte(`export {}`)}}}})
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	defer server.Close()
	var cookie *http.Cookie
	csrf := ""
	web := func(method, path string, body any, out any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		r, _ := http.NewRequestWithContext(ctx, method, origin+path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", origin)
		r.Header.Set("X-Learning-Space-ID", learningspace.DefaultID)
		r.Header.Set("X-Learning-Content-Version", "1")
		r.Header.Set("X-Learning-Change-Version", "1")
		if cookie != nil {
			r.AddCookie(cookie)
			r.Header.Set("X-CSRF-Token", csrf)
		}
		response, e := server.Client().Do(r)
		if e != nil {
			t.Fatal(e)
		}
		defer response.Body.Close()
		data, _ := io.ReadAll(response.Body)
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			t.Fatalf("Web %s：%d %s", path, response.StatusCode, data)
		}
		if cookies := response.Cookies(); len(cookies) > 0 {
			cookie = cookies[0]
		}
		if out != nil && json.Unmarshal(data, out) != nil {
			t.Fatal("Web 响应不是预期 JSON")
		}
	}
	code, _, err := ids.CreatePairingCodeForProfile(ctx, identity.PairingProfileResearch)
	if err != nil {
		t.Fatal(err)
	}
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	web("POST", "/v1/web/pairings", map[string]string{"code": code, "display_name": "Web 空资料开学"}, &session)
	csrf = session.CSRF
	goal := uuid.NewString()
	web("POST", "/v1/learning/goals", map[string]any{"operation_id": uuid.NewString(), "payload_schema_version": 1, "aggregate_type": "goal", "aggregate_id": goal, "expected_version": 0, "text": "Go 并发", "source": "web", "details": learning.GoalDetails{Name: "Go 并发", SelfAssessment: "已有基础", Purpose: "用于实际练习"}}, nil)
	create := mentorrun.Create{OperationID: uuid.NewString(), SessionID: uuid.NewString(), ExpectedVersion: 1, Prompt: "Go 并发", Save: true, RequestBudget: 8, TokenBudget: 50000, Research: &research.Request{Topic: "Go 并发", ExternalConsent: true, AutoAdopt: true, Policy: research.Policy{Mode: "supplement", Domains: []string{}}}, StartLearning: &learningstart.Request{NewSession: true, ModelConsent: true}}
	var receipt mentorrun.Receipt
	web("POST", "/v1/learning/goals/"+goal+"/runs", create, &receipt)
	if _, err = f.Runtime.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var run mentorrun.Snapshot
	web("GET", "/v1/learning/runs/"+receipt.RunID, nil, &run)
	if run.Status != "succeeded" || run.StartLearning == nil || run.StartLearning.Result == nil {
		t.Fatal("Web 未从空资料生成正式课堂", run.Status, run.Reason)
	}
	result := *run.StartLearning.Result
	var view learning.SessionView
	web("GET", "/v1/tutoring/sessions/"+result.SessionID, nil, &view)
	web("POST", "/v1/tutoring/sessions/"+result.SessionID+"/actions", map[string]any{"operation_id": uuid.NewString(), "payload_schema_version": 1, "aggregate_type": "session", "aggregate_id": result.SessionID, "expected_version": view.Session.AggregateVer, "action": "present_activity"}, nil)
	cliRoot := t.TempDir()
	cli := func(input string, args ...string) []byte {
		t.Helper()
		command := exec.CommandContext(ctx, binary, args...)
		command.Env = append(os.Environ(), "XDG_CONFIG_HOME="+cliRoot, "TERM=dumb", "NO_COLOR=1")
		command.Stdin = strings.NewReader(input)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		output, e := command.Output()
		if e != nil {
			t.Fatalf("生产 CLI %v：%v %s", args, e, stderr.String())
		}
		return output
	}
	code, _, err = ids.CreatePairingCodeForProfile(ctx, identity.PairingProfileResearch)
	if err != nil {
		t.Fatal(err)
	}
	cli(code+"\n", "pair", "--server", origin, "--name", "CLI 续学")
	var content map[string]json.RawMessage
	if json.Unmarshal(cli("", "study", "content", "--artifact", result.ArtifactID), &content) != nil || string(content["session_id"]) != "\""+result.SessionID+"\"" {
		t.Fatal("CLI 正文归属丢失")
	}
	if !bytes.Contains(cli("", "study", "context", "--session", result.SessionID), []byte(result.Context.ID)) {
		t.Fatal("上下文未共享")
	}
	if json.Unmarshal(cli("", "learn", "show", "--session", result.SessionID), &view) != nil || view.Session.ID != result.SessionID {
		t.Fatal("没有继续 Web 原会话")
	}
	answerOp := uuid.NewString()
	answer := map[string]any{"operation_id": answerOp, "payload_schema_version": 1, "aggregate_type": "session", "aggregate_id": result.SessionID, "expected_version": view.Session.AggregateVer, "action": "submit_attempt", "content_version": result.ArtifactVersion, "answer": "通过 Channel 发送并接收结果", "help": "none"}
	raw, _ := json.Marshal(answer)
	cli(string(raw), "study", "answer", "--artifact", result.ArtifactID, "--input", "-")
	web("GET", "/v1/tutoring/sessions/"+result.SessionID, nil, &view)
	if view.Session.State != tutoring.StateEvaluating || view.WorkItem.Attempt == nil || view.WorkItem.Attempt.Answer != answer["answer"] {
		t.Fatal("Web 未读取 CLI 已提交答案")
	}
	if !bytes.Contains(cli("", "study", "session-operation", "--session", result.SessionID, "--operation", answerOp), []byte(answerOp)) {
		t.Fatal("不能核对原答案操作")
	}
	// 新 CLI 独立发起研究开学，原 Web 运行和课堂不被替换。
	create.OperationID, create.SessionID = uuid.NewString(), uuid.NewString()
	raw, _ = json.Marshal(create)
	if json.Unmarshal(cli(string(raw), "study", "start", "--goal", goal, "--input", "-"), &receipt) != nil {
		t.Fatal("CLI 开学未返回真实回执")
	}
	if _, err = f.Runtime.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(cli("", "study", "run", "--run", receipt.RunID, "--wait"), &run) != nil || run.Status != "succeeded" {
		t.Fatal("CLI 研究未完成", run.Status)
	}
	var current struct {
		Run mentorrun.Snapshot `json:"run"`
	}
	if json.Unmarshal(cli("", "study", "current", "--goal", goal, "--kind", "start_learning"), &current) != nil || current.Run.RunID != receipt.RunID || current.Run.SessionID != create.SessionID {
		t.Fatal("不能按目标和类型恢复原运行")
	}
	web("GET", "/v1/tutoring/sessions/"+run.StartLearning.Result.SessionID, nil, &view)
	if view.Session.ID == result.SessionID {
		t.Fatal("新开学覆盖原课堂")
	}
	var snapshot learningchange.Snapshot
	if json.Unmarshal(cli("", "study", "change-context", "--goal", goal, "--session", result.SessionID), &snapshot) != nil || len(snapshot.Sources) == 0 {
		t.Fatal("缺少原课堂正式变更上下文")
	}
	f.PlanAdjustment(learningchange.Candidate{Kind: "route", Trigger: "user_request", Reason: "先补并发安全的前置概念", EvidenceIDs: []string{}, Steps: []learningchange.Step{{NodeRevisionID: snapshot.Sources[0].NodeRevisionID, Name: "并发安全", Prompt: "解释如何避免并发写入竞争", Criterion: "说明条件和原因", Difficulty: 1, Prerequisites: []int{}}}})
	adjust := mentorrun.Create{OperationID: uuid.NewString(), SessionID: uuid.NewString(), TeachingSessionID: result.SessionID, ExpectedVersion: snapshot.Goal.Revision, Prompt: "先补一个前置概念", Save: true, RequestBudget: 3, TokenBudget: 50000}
	raw, _ = json.Marshal(adjust)
	if json.Unmarshal(cli(string(raw), "study", "mentor", "--goal", goal, "--input", "-"), &receipt) != nil {
		t.Fatal("CLI 未请求真实服务端导师")
	}
	if _, err = f.Runtime.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var changes struct {
		Items []learningchange.Change `json:"items"`
	}
	if json.Unmarshal(cli("", "study", "changes", "--goal", goal), &changes) != nil || len(changes.Items) != 1 || changes.Items[0].Status != "queued_for_boundary" {
		t.Fatal("原题作答未处理时没有排队", changes.Items)
	}
	change := changes.Items[0]
	var fromWeb learningchange.Change
	web("GET", "/v1/learning/goals/"+goal+"/changes/"+change.ID, nil, &fromWeb)
	if fromWeb.Hash != change.Hash || fromWeb.SessionID != result.SessionID {
		t.Fatal("Web 未看到原课堂的具体候选")
	}
	decision := map[string]any{"operation_id": uuid.NewString(), "session_id": result.SessionID, "action": "apply_now", "expected_revision": change.Revision, "hash": change.Hash, "interaction_id": change.InteractionID, "immediate": false}
	raw, _ = json.Marshal(decision)
	first := cli(string(raw), "study", "change-command", "--goal", goal, "--change", change.ID, "--input", "-")
	if !bytes.Equal(first, cli(string(raw), "study", "change-command", "--goal", goal, "--change", change.ID, "--input", "-")) {
		t.Fatal("路径采用重试未复用原结果")
	}
	web("GET", "/v1/learning/goals/"+goal+"/changes/"+change.ID, nil, &fromWeb)
	web("GET", "/v1/tutoring/sessions/"+result.SessionID, nil, &view)
	if fromWeb.Status != "applied" || fromWeb.Applied.ActivityID == snapshot.Base.ActivityID || view.WorkItem.Activity.ID != fromWeb.Applied.ActivityID {
		t.Fatal("Web 未读取同一正式新活动")
	}
	var cliProgress, webProgress learning.ProgressPage
	if json.Unmarshal(cli("", "progress", "--goal-id", goal, "--status", "all", "--json"), &cliProgress) != nil {
		t.Fatal("CLI 进度 JSON 无效")
	}
	web("GET", "/v1/learning/progress?goal_id="+goal+"&status=all", nil, &webProgress)
	if cliProgress.Total != 1 || cliProgress.HighWater != webProgress.HighWater || !reflect.DeepEqual(cliProgress.Items, webProgress.Items) {
		t.Fatal("路径、会话、答案或证据的两端进度不一致")
	}
	var attempts int
	if err = f.Pool.QueryRow(ctx, `SELECT count(*) FROM learning_attempts WHERE session_id=$1`, result.SessionID).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatal("跨端复制答案", attempts, err)
	}
}
