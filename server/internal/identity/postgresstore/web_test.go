package postgresstore_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/identity"
)

func TestPostgreSQLWebImportProfileAndRevocation(t *testing.T) {
	pool := identityIntegrationPool(t)
	service := identityIntegrationService(t, pool)
	ctx := context.Background()
	code, _, err := service.CreatePairingCodeForProfile(ctx, identity.PairingProfileImport)
	if err != nil {
		t.Fatal(err)
	}
	cookie, p, err := service.ExchangeWebPairing(ctx, code, "导入验收")
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"knowledge:read", "knowledge:write", "knowledge:approve", "imports:web"} {
		if !slices.Contains(p.Device.Scopes, scope) {
			t.Fatalf("导入配对缺少 %s", scope)
		}
	}
	if _, err = pool.Exec(ctx, `UPDATE device_tokens SET scopes=array_remove(scopes,'imports:web') WHERE device_id=$1`, p.Device.ID); err != nil {
		t.Fatal(err)
	}
	p, err = service.AuthenticateWeb(ctx, cookie)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(p.Device.Scopes, "knowledge:write") || slices.Contains(p.Device.Scopes, "knowledge:approve") {
		t.Fatal("收回导入许可后仍能写入或审批")
	}
}

func TestPostgreSQLWebResearchProfileAndRevocation(t *testing.T) {
	pool := identityIntegrationPool(t)
	service := identityIntegrationService(t, pool)
	ctx := context.Background()
	code, _, err := service.CreatePairingCodeForProfile(ctx, identity.PairingProfileResearch)
	if err != nil {
		t.Fatal(err)
	}
	cookie, p, err := service.ExchangeWebPairing(ctx, code, "研究验收")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(p.Device.Scopes, "research:adopt") || !slices.Contains(p.Device.Scopes, "knowledge:write") || slices.Contains(p.Device.Scopes, "knowledge:approve") {
		t.Fatal("研究配对权限不正确")
	}
	if _, err = pool.Exec(ctx, `UPDATE device_tokens SET scopes=array_remove(scopes,'research:adopt') WHERE device_id=$1`, p.Device.ID); err != nil {
		t.Fatal(err)
	}
	p, err = service.AuthenticateWeb(ctx, cookie)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(p.Device.Scopes, "knowledge:write") {
		t.Fatal("研究许可收回后仍可采纳")
	}
}

func TestPostgreSQLWebPairingRollsBackWholeExchange(t *testing.T) {
	pool := identityIntegrationPool(t)
	service := identityIntegrationService(t, pool)
	ctx := context.Background()
	code, _, err := service.CreatePairingCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE FUNCTION fail_web_session_insert() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION '测试会话写入失败'; END $$;
		CREATE TRIGGER zz_fail_web_session BEFORE INSERT ON identity_web_sessions FOR EACH ROW EXECUTE FUNCTION fail_web_session_insert()`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.ExchangeWebPairing(ctx, code, "失败浏览器"); err == nil {
		t.Fatal("会话写入失败被吞掉")
	}
	var devices, tokens, sessions int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM devices),(SELECT count(*) FROM device_tokens),(SELECT count(*) FROM identity_web_sessions)`).Scan(&devices, &tokens, &sessions); err != nil {
		t.Fatal(err)
	}
	if devices != 0 || tokens != 0 || sessions != 0 {
		t.Fatal("配对失败遗留身份或会话")
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER zz_fail_web_session ON identity_web_sessions; DROP FUNCTION fail_web_session_insert()`); err != nil {
		t.Fatal(err)
	}
	cookie, p, err := service.ExchangeWebPairing(ctx, code, "真实浏览器")
	if err != nil {
		t.Fatal(err)
	}
	if p.Generation < 1 || !slices.Equal(p.Device.Scopes, []string{"knowledge:read", "learning:read", "learning:write"}) {
		t.Fatalf("浏览器权限与代次不符: %+v", p)
	}
	restarted := identityIntegrationService(t, pool)
	if _, err := restarted.AuthenticateWeb(ctx, cookie); err != nil {
		t.Fatalf("重启未恢复: %v", err)
	}
	if _, _, err := restarted.ExchangeWebPairing(ctx, code, "重放浏览器"); !errors.Is(err, identity.ErrInvalidPairingCode) {
		t.Fatal("配对码重放未拒绝")
	}
	if err := restarted.LogoutWeb(ctx, cookie); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.AuthenticateWeb(ctx, cookie); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatal("退出后会话仍有效")
	}
}
