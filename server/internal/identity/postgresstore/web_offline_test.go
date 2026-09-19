package postgresstore_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/identity"
)

func TestPostgreSQLWebOfflineExpiryAndLivePermissions(t *testing.T) {
	pool := identityIntegrationPool(t)
	service := identityIntegrationService(t, pool)
	ctx := context.Background()
	code, _, err := service.CreatePairingCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	web, original, err := service.ExchangeWebPairing(ctx, code, "离线到期验收")
	if err != nil {
		t.Fatal(err)
	}
	cookie, p, err := service.EnableWebOffline(ctx, web)
	if err != nil || p.Device.ID != original.Device.ID || !p.ContentAllowed {
		t.Fatalf("离线身份不属于原设备：%v", err)
	}
	if slices.Contains(p.Credential.Scopes, "devices:manage") || slices.Contains(p.Credential.Scopes, "learning:approve") {
		t.Fatal("离线身份扩大权限")
	}
	if _, err := pool.Exec(ctx, `UPDATE device_tokens SET scopes=array_remove(scopes,'learning:write') WHERE device_id=$1`, p.Device.ID); err != nil {
		t.Fatal(err)
	}
	p, err = service.AuthenticateWebOffline(ctx, cookie)
	if err != nil || slices.Contains(p.Credential.Scopes, "learning:write") {
		t.Fatal("撤回权限后仍可写")
	}
	if _, err = service.RenewWebOffline(ctx, cookie); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("失去写权限仍续期：%v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE identity_web_offline_sessions SET expires_at=clock_timestamp()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if _, err = service.AuthenticateWebOffline(ctx, cookie); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("到期后仍认证：%v", err)
	}
	if _, err = service.RenewWebOffline(ctx, cookie); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("到期身份复活：%v", err)
	}
}
