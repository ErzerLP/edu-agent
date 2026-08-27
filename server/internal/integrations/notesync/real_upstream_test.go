package notesync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/platform/outbox"
	"github.com/google/uuid"
)

const (
	realBaseURLEnv = "NOTESYNC_REAL_BASE_URL"
	realTokenEnv   = "NOTESYNC_REAL_API_TOKEN"
	realVaultEnv   = "NOTESYNC_REAL_VAULT"
)

type realCandidateConfig struct {
	baseURL string
	token   string
	vault   string
}

type realCandidateNote struct {
	path     string
	markdown string
	note     Note
}

func TestRealUpstreamCandidate(t *testing.T) {
	config, ok := realCandidateConfigFromEnv()
	if !ok {
		t.Skip("set NOTESYNC_REAL_BASE_URL, NOTESYNC_REAL_API_TOKEN, and NOTESYNC_REAL_VAULT to run the real Fast Note Sync candidate")
	}
	client := newRealCandidateClient(t, config.baseURL, config.token)
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	runID := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]

	t.Log("scenario: pinned routes, bearer/client identity, exact vault, create-only, readback, update, and list metadata")
	published := exerciseRealClientContract(t, ctx, config, client, runID)

	t.Log("scenario: real remote drift, preview, and explicit accept_remote import")
	exerciseRealReviewImport(t, ctx, config, client, published, runID)

	t.Log("scenario: real POST response loss reconciles by exact GET without a duplicate write")
	exerciseRealResponseLoss(t, ctx, config, client, runID)
}

func realCandidateConfigFromEnv() (realCandidateConfig, bool) {
	config := realCandidateConfig{
		baseURL: strings.TrimSpace(os.Getenv(realBaseURLEnv)),
		token:   os.Getenv(realTokenEnv),
		vault:   strings.TrimSpace(os.Getenv(realVaultEnv)),
	}
	return config, config.baseURL != "" && config.token != "" && config.vault != ""
}

func newRealCandidateClient(t *testing.T, rawBaseURL, token string) *Client {
	t.Helper()
	baseURL, err := url.Parse(rawBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(Options{BaseURL: baseURL, APIToken: token, Timeout: 8 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func exerciseRealClientContract(t *testing.T, ctx context.Context, config realCandidateConfig, client *Client, runID string) realCandidateNote {
	t.Helper()
	version, err := client.Version(ctx)
	if err != nil || version.Version != SupportedVersion {
		t.Fatalf("version=%+v err=%v", version, err)
	}
	health, err := client.Health(ctx)
	if err != nil || health.Status != "healthy" || health.Database != "connected" || health.Version != SupportedVersion {
		t.Fatalf("health=%+v err=%v", health, err)
	}
	vaults, err := client.Vaults(ctx)
	if err != nil || len(vaults) != 1 || vaults[0].Name != config.vault {
		t.Fatalf("restricted vaults=%+v err=%v", vaults, err)
	}
	assertRealAuthRejected(t, ctx, config, "", ClientType)
	assertRealAuthRejected(t, ctx, config, config.token, "webgui")

	capability := client.Probe(ctx, config.vault)
	if !capability.Compatible || capability.Version != SupportedVersion || capability.Vault != config.vault || capability.Reason != "" {
		t.Fatalf("capability=%+v", capability)
	}
	_, err = client.CreateOrUpdateNote(ctx, NoteWrite{
		Vault: config.vault, Path: capabilityProbePath, Ctime: 1, Mtime: 1, CreateOnly: true,
	})
	if !hasBusinessCode(err, businessInvalidPath) {
		t.Fatalf("configured-vault capability probe err=%v code=%d", err, businessCode(err))
	}
	_, err = client.CreateOrUpdateNote(ctx, NoteWrite{
		Vault: capabilityProbeVault, Path: capabilityProbePath, Ctime: 1, Mtime: 1, CreateOnly: true,
	})
	if !hasBusinessCode(err, businessVaultDenied) {
		t.Fatalf("sentinel-vault capability probe err=%v code=%d", err, businessCode(err))
	}

	path := "edu-agent/candidate-" + runID + ".md"
	initialMarkdown := validRemoteMarkdown(t, "real candidate initial "+runID)
	now := time.Now().UTC()
	created, err := client.CreateOrUpdateNote(ctx, NoteWrite{
		Vault: config.vault, Path: path, Content: initialMarkdown,
		Ctime: now.UnixMilli(), Mtime: now.UnixMilli(), CreateOnly: true,
	})
	if err != nil || created.Path != path || created.Content != initialMarkdown || created.Ctime <= 0 || created.Mtime <= 0 {
		t.Fatalf("initial create note=%+v err=%v", created, err)
	}
	_, err = client.CreateOrUpdateNote(ctx, NoteWrite{
		Vault: config.vault, Path: path, Content: initialMarkdown,
		Ctime: created.Ctime, Mtime: now.Add(time.Second).UnixMilli(), CreateOnly: true,
	})
	if !hasBusinessCode(err, businessNoteExists) || Category(err) != CategoryConflict {
		t.Fatalf("second create-only err=%v category=%s code=%d", err, Category(err), businessCode(err))
	}
	read, err := client.GetNote(ctx, config.vault, path)
	if err != nil || read.Content != initialMarkdown || read.Path != path {
		t.Fatalf("initial readback note=%+v err=%v", read, err)
	}

	updatedMarkdown := validRemoteMarkdown(t, "real candidate updated "+runID)
	_, err = client.CreateOrUpdateNote(ctx, NoteWrite{
		Vault: config.vault, Path: path, Content: updatedMarkdown,
		Ctime: read.Ctime, Mtime: now.Add(2 * time.Second).UnixMilli(), CreateOnly: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := client.GetNote(ctx, config.vault, path)
	if err != nil || updated.Content != updatedMarkdown || updated.Path != path {
		t.Fatalf("updated readback note=%+v err=%v", updated, err)
	}
	page, err := client.ListNotes(ctx, config.vault, 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	listed, found := listedNote(page, path)
	if !found || listed.Vault != config.vault || listed.Path != path ||
		listed.Version != updated.Version || listed.Ctime != updated.Ctime || listed.Mtime != updated.Mtime || listed.LastTime != updated.LastTime {
		t.Fatalf("list metadata note=%+v found=%t updated=%+v page=%+v", listed, found, updated, page)
	}
	return realCandidateNote{path: path, markdown: updatedMarkdown, note: updated}
}

func assertRealAuthRejected(t *testing.T, ctx context.Context, config realCandidateConfig, token, clientType string) {
	t.Helper()
	endpoint := strings.TrimRight(config.baseURL, "/") + "/api/vault?client=" + url.QueryEscape(clientType)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Client", clientType)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := (&http.Client{Timeout: 8 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Code   int  `json:"code"`
		Status bool `json:"status"`
	}
	if response.StatusCode != http.StatusOK || json.Unmarshal(payload, &envelope) != nil || envelope.Status || envelope.Code < 306 || envelope.Code > 315 {
		t.Fatalf("authentication rejection status=%d business_status=%t code=%d", response.StatusCode, envelope.Status, envelope.Code)
	}
}

func listedNote(page NotePage, path string) (Note, bool) {
	for _, note := range page.Notes {
		if note.Path == path {
			return note, true
		}
	}
	return Note{}, false
}

func businessCode(err error) int {
	var target *Error
	if errors.As(err, &target) {
		return target.BusinessCode()
	}
	return 0
}

func exerciseRealReviewImport(t *testing.T, ctx context.Context, config realCandidateConfig, client *Client, published realCandidateNote, runID string) {
	t.Helper()
	remoteMarkdown := validRemoteMarkdown(t, "real remote drift accepted "+runID)
	_, err := client.CreateOrUpdateNote(ctx, NoteWrite{
		Vault: config.vault, Path: published.path, Content: remoteMarkdown,
		Ctime: published.note.Ctime, Mtime: time.Now().UTC().Add(3 * time.Second).UnixMilli(), CreateOnly: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	remote, err := client.GetNote(ctx, config.vault, published.path)
	if err != nil || remote.Content != remoteMarkdown {
		t.Fatalf("remote drift note=%+v err=%v", remote, err)
	}

	canonicalPath := strings.TrimPrefix(published.path, "edu-agent/")
	store := &reviewFixtureStore{state: PreviewState{
		Generation: 1, HeadRevisionID: "10000000-0000-4000-8000-000000000000", HeadRevisionNo: 2,
		DocumentID: "20000000-0000-4000-8000-000000000000", CanonicalPath: canonicalPath,
		Mapping: &PublicationMapping{
			DocumentID: "20000000-0000-4000-8000-000000000000", RemoteVault: config.vault, RemotePath: published.path,
			KnowledgeRevisionID: "10000000-0000-4000-8000-000000000000", DocumentRevisionID: "40000000-0000-4000-8000-000000000000",
			RevisionNo: 2, BaseMarkdown: published.markdown, RemoteVersion: published.note.Version,
			RemoteLastTime: published.note.LastTime, Generation: 1,
		},
		Local: ReviewSnapshot{
			KnowledgeRevisionID: "10000000-0000-4000-8000-000000000000", KnowledgeRevisionNo: 2,
			DocumentRevisionID: "40000000-0000-4000-8000-000000000000", Path: canonicalPath,
			Markdown: published.markdown, SHA256: markdownSHA(published.markdown),
		},
	}, operations: make(map[string]ResolutionOperationRecord)}
	importer := &reviewFixtureImporter{result: knowledge.ImportResult{Revision: knowledge.KnowledgeRevision{
		ID: "a0000000-0000-4000-8000-000000000000", Documents: []knowledge.SnapshotDocument{{
			Path: canonicalPath, Revision: knowledge.DocumentRevision{
				ID: "b0000000-0000-4000-8000-000000000000", DocumentID: "20000000-0000-4000-8000-000000000000",
			},
		}},
	}}}
	service, err := NewReviewService(ReviewServiceOptions{
		Store: store, Remote: client, Importer: importer, Canonicalizer: knowledge.NewCanonicalizer(),
		Vault: config.vault, PathPrefix: "edu-agent", ScanPageSize: 25, ScanMaxPages: 20,
		NewUUID: func() string { return "90000000-0000-4000-8000-000000000001" },
		Now:     func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := service.Preview(ctx, PreviewCommand{Path: published.path})
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Items) != 1 || preview.Items[0].Category != PreviewCategoryRemoteChanged ||
		preview.Items[0].ReviewID == "" || preview.Items[0].Remote.SHA256 != markdownSHA(remoteMarkdown) {
		t.Fatalf("real drift preview=%+v", preview)
	}
	item := preview.Items[0]
	result, err := service.Resolve(ctx, ResolutionCommand{
		ReviewID: item.ReviewID, BasisHash: item.BasisHash,
		OperationID: "80000000-0000-4000-8000-000000000000",
		DeviceID:    "70000000-0000-4000-8000-000000000000",
		Kind:        ResolutionAcceptRemote,
	})
	if err != nil {
		t.Fatal(err)
	}
	if importer.calls != 1 || len(importer.command.Documents) != 1 || importer.command.Documents[0].Markdown != remoteMarkdown ||
		importer.command.ExpectedParentRevisionID == nil || *importer.command.ExpectedParentRevisionID != store.state.HeadRevisionID ||
		importer.command.Source != KnowledgeImportSource || importer.command.NotesyncResolution == nil ||
		importer.command.NotesyncResolution.ObservedRemoteMarkdown != remoteMarkdown ||
		result.KnowledgeRevisionID != importer.result.Revision.ID || result.DocumentRevisionID == "" {
		t.Fatalf("accept_remote result=%+v import=%+v", result, importer.command)
	}
}

func exerciseRealResponseLoss(t *testing.T, ctx context.Context, config realCandidateConfig, upstreamClient *Client, runID string) {
	t.Helper()
	path := "edu-agent/response-loss-" + runID + ".md"
	baseMarkdown := validRemoteMarkdown(t, "response loss base "+runID)
	targetMarkdown := validRemoteMarkdown(t, "response loss target "+runID)
	now := time.Now().UTC()
	created, err := upstreamClient.CreateOrUpdateNote(ctx, NoteWrite{
		Vault: config.vault, Path: path, Content: baseMarkdown,
		Ctime: now.UnixMilli(), Mtime: now.UnixMilli(), CreateOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	proxy := newResponseLossProxy(t, config.baseURL, path)
	defer proxy.server.Close()
	proxyClient := newRealCandidateClient(t, proxy.server.URL, config.token)
	mapping := &PublicationMapping{
		DocumentID: "20000000-0000-4000-8000-000000000000", RemoteVault: config.vault, RemotePath: path,
		KnowledgeRevisionID: "10000000-0000-4000-8000-000000000000", DocumentRevisionID: "30000000-0000-4000-8000-000000000000",
		RevisionNo: 1, BaseMarkdown: baseMarkdown, RemoteVersion: created.Version, RemoteLastTime: created.LastTime, Generation: 1,
	}
	work := publicationFixtureWork(now.Add(time.Second), mapping)
	work.RemoteVault = config.vault
	work.RemotePath = path
	work.CanonicalPath = strings.TrimPrefix(path, "edu-agent/")
	work.TargetMarkdown = targetMarkdown
	store := &consumerFixtureStore{decision: outbox.ApplyDecision{Apply: true}, work: work}
	consumer, err := NewConsumer(ConsumerOptions{
		Store: store, Remote: proxyClient, Vault: config.vault, PathPrefix: "edu-agent",
		RetryBackoff: 3 * time.Second, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := consumer.Apply(ctx, publicationFixtureMessage(nil)); !errors.Is(err, outbox.ErrConsumerFinalized) {
		t.Fatalf("response-loss apply err=%v", err)
	}
	if proxyErr := proxy.err(); proxyErr != nil {
		t.Fatal(proxyErr)
	}
	if store.outcome.Kind != OutcomeApplied || store.outcome.Remote.Markdown != targetMarkdown || proxy.dropped.Load() != 1 {
		t.Fatalf("response-loss outcome=%+v dropped_posts=%d", store.outcome, proxy.dropped.Load())
	}
	readback, err := upstreamClient.GetNote(ctx, config.vault, path)
	if err != nil || readback.Content != targetMarkdown {
		t.Fatalf("response-loss upstream readback=%+v err=%v", readback, err)
	}
}

type responseLossProxy struct {
	server  *httptest.Server
	dropped atomic.Int32
	errors  chan error
}

func newResponseLossProxy(t *testing.T, rawUpstreamURL, lossPath string) *responseLossProxy {
	t.Helper()
	upstream, err := url.Parse(rawUpstreamURL)
	if err != nil {
		t.Fatal(err)
	}
	result := &responseLossProxy{errors: make(chan error, 1)}
	reverse := httputil.NewSingleHostReverseProxy(upstream)
	reverse.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		result.record(fmt.Errorf("response-loss reverse proxy: %w", err))
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}
	result.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/note" {
			reverse.ServeHTTP(w, request)
			return
		}
		payload, err := io.ReadAll(request.Body)
		if err != nil {
			result.failProxyRequest(w, err)
			return
		}
		request.Body = io.NopCloser(bytes.NewReader(payload))
		var write NoteWrite
		if err := json.Unmarshal(payload, &write); err != nil || write.Path != lossPath {
			reverse.ServeHTTP(w, request)
			return
		}
		if result.dropped.Add(1) != 1 {
			reverse.ServeHTTP(w, request)
			return
		}
		if err := forwardRealWriteAndDrop(w, request, upstream, payload); err != nil {
			result.record(err)
		}
	}))
	return result
}

func forwardRealWriteAndDrop(w http.ResponseWriter, request *http.Request, upstream *url.URL, payload []byte) error {
	endpoint := *upstream
	endpoint.Path = strings.TrimRight(upstream.Path, "/") + request.URL.Path
	endpoint.RawQuery = request.URL.RawQuery
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	forward, err := http.NewRequestWithContext(ctx, request.Method, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	forward.Header = request.Header.Clone()
	forward.Host = upstream.Host
	response, err := http.DefaultTransport.RoundTrip(forward)
	if err != nil {
		return fmt.Errorf("forward real response-loss write: %w", err)
	}
	defer response.Body.Close()
	responsePayload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read real response-loss write: %w", err)
	}
	var envelope struct {
		Code   int  `json:"code"`
		Status bool `json:"status"`
	}
	if response.StatusCode != http.StatusOK || json.Unmarshal(responsePayload, &envelope) != nil || !envelope.Status || envelope.Code != businessSuccess {
		return fmt.Errorf("real response-loss write was not accepted: http=%d status=%t code=%d", response.StatusCode, envelope.Status, envelope.Code)
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return errors.New("response-loss proxy does not support connection hijacking")
	}
	connection, _, err := hijacker.Hijack()
	if err != nil {
		return fmt.Errorf("hijack response-loss connection: %w", err)
	}
	return connection.Close()
}

func (p *responseLossProxy) failProxyRequest(w http.ResponseWriter, err error) {
	p.record(err)
	http.Error(w, "proxy request failed", http.StatusBadGateway)
}

func (p *responseLossProxy) record(err error) {
	select {
	case p.errors <- err:
	default:
	}
}

func (p *responseLossProxy) err() error {
	select {
	case err := <-p.errors:
		return err
	default:
		return nil
	}
}
