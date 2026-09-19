package postgresstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/privacy"
	privacydb "github.com/edu-agent/edu-agent/server/internal/privacy/postgresstore"
	"github.com/google/uuid"
)

func TestOfflineDevicePurgeReceiptRecoversOnlyCompletedOriginalGeneration(t *testing.T) {
	ctx := context.Background()
	pool := privacyIntegrationPool(t)
	keyring, err := privacy.NewOfflineChallengeKeyring(map[int][]byte{1: []byte("0123456789abcdef0123456789abcdef")})
	if err != nil {
		t.Fatal(err)
	}
	store := privacydb.New(pool, privacydb.WithOfflineChallengeKeyring(keyring))
	erasureID, deviceID := uuid.NewString(), uuid.NewString()
	seedOfflinePurgeReceipt(t, pool, keyring, erasureID, []string{deviceID}, time.Now().UTC().Truncate(time.Microsecond))
	if _, found, err := store.OfflineDevicePurgeReceipt(ctx, deviceID, 1); err != nil || found {
		t.Fatalf("未完成任务不能伪装成清除回执：found=%t err=%v", found, err)
	}
	task, found, err := store.CurrentOfflineDevicePurge(ctx, deviceID)
	if err != nil || !found {
		t.Fatalf("读取原清除任务：found=%t err=%v", found, err)
	}
	ack, err := store.AcknowledgeOfflineDevicePurge(ctx, erasureID, deviceID, privacy.OfflineDevicePurgeAcknowledgment{
		ChallengeRevision: task.ChallengeRevision, Challenge: task.Challenge,
		Outcome: privacy.OfflinePurgeOutcomeSucceeded, ManagedObjectsAbsent: boolPointer(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt, found, err := store.OfflineDevicePurgeReceipt(ctx, deviceID, 1)
	// PostgreSQL 时间戳精度为微秒；其余回执字段必须完全一致。
	if !receipt.UpdatedAt.Equal(ack.UpdatedAt.Truncate(time.Microsecond)) {
		t.Fatalf("回执时间不一致：%v / %v", receipt.UpdatedAt, ack.UpdatedAt)
	}
	ack.UpdatedAt = receipt.UpdatedAt
	if err != nil || !found || receipt != ack {
		t.Fatalf("原回执恢复不一致：receipt=%+v ack=%+v found=%t err=%v", receipt, ack, found, err)
	}
	for _, owner := range []struct {
		device     string
		generation int64
	}{{uuid.NewString(), 1}, {deviceID, 2}} {
		if _, found, err := store.OfflineDevicePurgeReceipt(ctx, owner.device, owner.generation); err != nil || found {
			t.Fatalf("不得返回其他设备或代次的回执：found=%t err=%v", found, err)
		}
	}
}
