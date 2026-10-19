package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	identitydb "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgedb "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	spacedb "github.com/edu-agent/edu-agent/server/internal/learningspace/postgresstore"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Exercise the encoded URLs emitted by the web client through the real router,
// publication store and source reader, including historical artifact revisions.
func TestPostgreSQLStudioCitationEncodedSectionsAndLegacyVersions(t *testing.T) {
	pool := webTestPool(t)
	ctx, _ := learningspace.WithScope(context.Background(), learningspace.DefaultID)
	ids, err := identity.NewService(identitydb.New(pool), identity.Options{PairingCodeTTL: time.Minute, PairingCodeMaxAttempts: 5, LastUsedTouchInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := ids.CreatePairingCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := ids.ExchangePairingCode(ctx, code, "引用回归")
	if err != nil {
		t.Fatal(err)
	}
	actor, err := ids.Authenticate(ctx, issued.Token, "learning:write")
	if err != nil {
		t.Fatal(err)
	}
	content, err := learningcontent.New(pool, nil, bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	kstore := knowledgedb.New(pool)
	content.ConfigureStudioSources(kstore)
	ks, err := knowledge.NewService(kstore, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	collection := uuid.NewString()
	if _, err = ks.ChangeCollection(ctx, knowledge.CollectionCommand{ID: collection, Action: "create", Name: "引用来源", Source: "回归测试"}); err != nil {
		t.Fatal(err)
	}
	sourceCtx, _ := knowledge.WithCollection(ctx, collection)
	imported, err := ks.Import(sourceCtx, knowledge.ImportCommand{OperationID: uuid.NewString(), ActorDeviceID: actor.Device.ID, ExpectedParentProvided: true, Source: "回归测试", Documents: []knowledge.ImportDocument{{Path: "source.md", Markdown: "第一段原文。\n第二段原文。\n"}}})
	if err != nil {
		t.Fatal(err)
	}
	doc := imported.Revision.Documents[0].Revision
	sourceID := knowledge.SourceIdentity{CollectionID: collection, RevisionID: imported.Revision.ID, DocumentID: doc.DocumentID, DocumentRevisionID: doc.ID}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	manifest, err := kstore.FreezeSourceSelectionTx(ctx, tx, knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{sourceID}})
	if err != nil {
		t.Fatal(err)
	}
	sources, err := kstore.ReadSourceSelectionTx(ctx, tx, *manifest)
	if err != nil || len(sources) != 1 || len(sources[0].Fragments) != 1 {
		t.Fatalf("来源片段不可用: %v", err)
	}
	fragment := sources[0].Fragments[0]
	legacyCitation := learningcontent.StudioCitation{ID: "1", Source: sources[0].SourceItem, Fragment: fragment, DocumentSHA256: manifest.Entries[0].SHA256, SliceSHA256: knowledge.SourceTextHash(fragment.Text)}
	claim := learningcontent.StudioClaim{Text: "来源支持的主张", Citations: []string{"1"}}
	legacyBody := learningcontent.StudioBody{Schema: 1, Title: "旧指南", SourceManifest: manifest, Citations: []learningcontent.StudioCitation{legacyCitation}, Guide: &learningcontent.StudioGuide{KeyPoints: []learningcontent.StudioClaim{claim}}}
	legacy, err := content.StudioPublishTx(ctx, tx, actor, uuid.NewString(), learningcontent.StudioTarget{Generation: 1, Kind: "guide"}, legacyBody)
	if err != nil {
		t.Fatal(err)
	}
	var slices []knowledge.SourceDetail
	for i := 0; i < 2; i++ {
		start := fragment.Range.Start + i*len("第一段原文。\n")
		detail, e := kstore.ReadSourceSliceTx(ctx, tx, *manifest, knowledge.SourceSliceRequest{SourceIdentity: sourceID, NodeID: fragment.NodeID, NodeRevisionID: fragment.NodeRevisionID, Start: start, End: start + len("第一段原文。\n")})
		if e != nil {
			t.Fatal(e)
		}
		slices = append(slices, detail)
	}
	// Publish two schema 2 versions with the same citation IDs and different
	// slices, so resolving the latest version cannot satisfy a historical read.
	for version := int64(2); version <= 3; version++ {
		job := learningcontent.StudioJob{ID: uuid.NewString(), SpaceID: learningspace.DefaultID, DeviceID: actor.Device.ID, Generation: 1, Version: 1, Kind: "guide", Status: "summarizing", Input: learningcontent.StudioJobInput{Manifest: *manifest, ArtifactID: legacy.ArtifactID, ExpectedArtifactVersion: version - 1, Summary: &learningcontent.StudioSummary{}}, TotalUnits: 2, CompletedUnits: 2}
		if err = content.InsertStudioJobTx(ctx, tx, &job); err != nil {
			t.Fatal(err)
		}
		if err = content.AppendStudioPlanTx(ctx, tx, &job, learningcontent.StudioPlan{Title: "分段指南", Coverage: "complete", Sections: []learningcontent.StudioPlanSection{{ID: "ip-subnetting"}, {ID: "tcp-handshake"}}}); err != nil {
			t.Fatal(err)
		}
		job.ConfirmedPlanRevision = job.ActivePlanRevision
		for i, id := range []string{"ip-subnetting-1", "tcp-handshake-1"} {
			detail := slices[(i+int(version)-2)%2]
			body := learningcontent.StudioBody{Schema: 2, Title: "分段", SourceManifest: manifest, Coverage: "complete", Claims: []learningcontent.StudioClaim{claim}, Citations: []learningcontent.StudioCitation{{ID: "1", Source: detail.SourceItem, Fragment: detail.Fragments[0], DocumentSHA256: manifest.Entries[0].SHA256, SliceSHA256: knowledge.SourceTextHash(detail.Fragments[0].Text)}}}
			if err = content.SaveStudioUnitTx(ctx, tx, job, learningcontent.StudioUnit{ID: id, SectionID: job.Plan.Sections[i].ID, Ordinal: i + 1, Status: "succeeded", Body: &body}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = content.PublishStudioJobTx(ctx, tx, actor, job, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// Advance the source head too; saved citations must keep the original source.
	parent := imported.Revision.ID
	if _, err = ks.Import(sourceCtx, knowledge.ImportCommand{OperationID: uuid.NewString(), ActorDeviceID: actor.Device.ID, ExpectedParentProvided: true, ExpectedParentRevisionID: &parent, Source: "来源更新", Documents: []knowledge.ImportDocument{{Path: "source.md", Markdown: strings.Replace(doc.CanonicalMarkdown, "第一段原文。", "来源已更新。", 1)}}}); err != nil {
		t.Fatal(err)
	}
	h, err := New(Options{Identity: ids, LearningSpaces: spacedb.New(pool), LearningContent: content, Readiness: fakeReadiness{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), PairLimiter: NewFixedWindowLimiter(1000, time.Minute), AuthLimiter: NewFixedWindowLimiter(1000, time.Minute), DeviceLimiter: NewFixedWindowLimiter(1000, time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	get := func(id, query string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("GET", "/v1/learning/studio/artifacts/"+legacy.ArtifactID+"/sources/"+id+query, nil)
		r.Header.Set("Authorization", "Bearer "+issued.Token)
		r.Header.Set(learningspace.Header, learningspace.DefaultID)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, test := range []struct {
		path, id, text string
		version        int64
		slice          int
	}{
		{"1", "1", "第一段原文。\n第二段原文。\n", 1, -1},
		{"ip-subnetting-1%3A1", "ip-subnetting-1:1", "第一段原文。\n", 2, 0},
		{"tcp-handshake-1%3A1", "tcp-handshake-1:1", "第二段原文。\n", 2, 1},
		{"ip-subnetting-1:1", "ip-subnetting-1:1", "第一段原文。\n", 2, 0},
		{"ip-subnetting-1%3a1", "ip-subnetting-1:1", "第二段原文。\n", 3, 1},
	} {
		t.Run(test.path+"/v"+strconv.FormatInt(test.version, 10), func(t *testing.T) {
			w := get(test.path, "?version="+strconv.FormatInt(test.version, 10))
			if w.Code != 200 {
				t.Fatalf("引用无法打开: %d %s", w.Code, w.Body.String())
			}
			var got learningcontent.StudioCitation
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.ID != test.id || got.Source.Identity() != sourceID || got.Fragment.Text != test.text || got.DocumentSHA256 != manifest.Entries[0].SHA256 || got.SliceSHA256 != knowledge.SourceTextHash(test.text) {
				t.Fatal("引用未返回对应版本的来源和片段")
			}
			wantFragment := fragment
			if test.slice >= 0 {
				wantFragment = slices[test.slice].Fragments[0]
			}
			if !reflect.DeepEqual(got.Fragment, wantFragment) {
				t.Fatal("片段身份、范围或子范围证明发生变化")
			}
		})
	}
	for _, test := range []struct {
		id, query string
		status    int
	}{
		{"unknown-1%3A1", "?version=2", 404},
		{"ip-subnetting-1%3A999", "?version=2", 404},
		{"ip-subnetting-1%3A1%3A1", "?version=2", 400},
		{"ip-subnetting-1%253A1", "?version=2", 400},
		{"2", "?version=1", 404},
		{"ip-subnetting-1%3A1", "?version=1", 404},
		{"ip-subnetting-1%3A1", "?version=999", 404},
		{"ip-subnetting-1%3A1", "", 400},
		{"ip-subnetting-1%3A1", "?version=0", 400},
	} {
		t.Run(test.id+test.query, func(t *testing.T) {
			if w := get(test.id, test.query); w.Code != test.status {
				t.Fatalf("无效引用状态码: got %d want %d", w.Code, test.status)
			}
		})
	}
}

func TestStudioCitationRejectsMalformedPathEscape(t *testing.T) {
	router := chi.NewRouter()
	router.Get("/sources/{citationID}", (&API{}).studioCitation)
	r := httptest.NewRequest("GET", "/sources/invalid?version=1", nil)
	r.URL.RawPath = "/sources/ip-subnetting-1%ZZ1"
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("无效路径编码状态码: got %d want 400", w.Code)
	}
}
