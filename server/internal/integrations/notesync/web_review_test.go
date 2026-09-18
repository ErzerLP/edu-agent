package notesync

import (
	"context"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/google/uuid"
)

func TestWebResolutionPreviewUsesOriginalPlanWithoutCommit(t *testing.T) {
	ctx := context.Background()
	review := reviewFixture(validRemoteMarkdown(t, "远端正文"))
	store := &reviewFixtureStore{reviews: map[string]Review{review.ReviewID: review}}
	remote := &reviewFixtureRemote{notes: map[string]Note{
		review.RemotePath: {Path: review.RemotePath, Content: review.Remote.Markdown, Version: review.Remote.RemoteVersion, LastTime: review.Remote.RemoteLastTime},
	}}
	importer := &reviewFixtureImporter{}
	service := newReviewFixtureServiceWithImporter(t, store, remote, importer)
	command := ResolutionCommand{ReviewID: review.ReviewID, BasisHash: review.BasisHash, OperationID: uuid.NewString(), DeviceID: uuid.NewString(), Kind: ResolutionAcceptRemote}
	plan, err := service.PreviewResolution(ctx, command)
	if err != nil || plan.Status != "ready" || plan.AffectedEvidence != 2 || importer.plans != 1 || importer.calls != 0 || store.keepCalls != 0 {
		t.Fatalf("预览未复用原计划器或产生提交：%+v，%v", plan, err)
	}
	if importer.command.OperationID != knowledgeImportOperationID(command) || importer.command.NotesyncResolution == nil || importer.command.ActorDeviceID != command.DeviceID {
		t.Fatal("预览丢失原操作、身份或同步合同")
	}
	command.Kind = ResolutionKeepCanonical
	if _, err = service.PreviewResolution(ctx, command); ReviewErrorCode(err) != CodeReviewInvalidRequest || store.keepCalls != 0 {
		t.Fatal("预览错误触发保留本地的发布操作")
	}
	command.Kind = ResolutionAcceptRemote
	note := remote.notes[review.RemotePath]
	note.Version++
	remote.notes[review.RemotePath] = note
	if _, err = service.PreviewResolution(ctx, command); ReviewErrorCode(err) != CodeReviewStale || importer.plans != 1 {
		t.Fatal("远端变化后仍生成旧审阅计划")
	}
}

func TestWebResolutionLookupIsReadOnlyAndDeviceBound(t *testing.T) {
	ctx := context.Background()
	device, operation := uuid.NewString(), uuid.NewString()
	result := ResolutionResult{ReviewID: uuid.NewString(), ResolutionKind: ResolutionKeepCanonical}
	store := &reviewFixtureStore{operations: map[string]ResolutionOperationRecord{device + "/" + operation: {Result: result}}}
	remote := &reviewFixtureRemote{}
	service := newReviewFixtureService(t, store, remote)
	got, err := service.Operation(ctx, device, operation)
	if err != nil || got != result || len(remote.gets) != 0 || store.keepCalls != 0 {
		t.Fatalf("核对错误或执行了副作用：%+v %v", got, err)
	}
	for _, pair := range [][2]string{{uuid.NewString(), operation}, {device, uuid.NewString()}} {
		if _, err := service.Operation(ctx, pair[0], pair[1]); ReviewErrorCode(err) != CodeReviewNotFound {
			t.Fatal("跨设备或未知操作获得收据")
		}
	}
	otherSpace, _ := learningspace.WithScope(ctx, uuid.NewString())
	otherCollection, _ := knowledge.WithCollection(ctx, uuid.NewString())
	for _, scope := range []context.Context{otherSpace, otherCollection} {
		if _, err := service.Operation(scope, device, operation); ReviewErrorCode(err) != CodeReviewInvalidRequest {
			t.Fatal("未映射来源继承了同步权限")
		}
	}
}
