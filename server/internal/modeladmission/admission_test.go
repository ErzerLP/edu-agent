package modeladmission

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/edu-agent/edu-agent/server/testutil/dbtest"
	"github.com/google/uuid"
)

func TestAdmissionSerializesDevicesAndReservesWebCapacity(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	device := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'准入测试',now())`, device); err != nil {
		t.Fatal(err)
	}
	acquire := func(source, device string) error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if _, err = AcquireTx(ctx, tx, 1, 3, source, device, ""); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- acquire("cli", device) }()
	}
	wg.Wait()
	close(results)
	accepted, busy := 0, 0
	for err := range results {
		if err == nil {
			accepted++
		} else if errors.Is(err, ErrBusy) {
			busy++
		} else {
			t.Fatal(err)
		}
	}
	if accepted != 1 || busy != 1 {
		t.Fatalf("device concurrency: %d %d", accepted, busy)
	}
	other := uuid.NewString()
	_, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'第二设备',now())`, other)
	if err != nil {
		t.Fatal(err)
	}
	if err = acquire("cli", other); err != nil {
		t.Fatal(err)
	}
	third := uuid.NewString()
	_, err = pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'第三设备',now())`, third)
	if err != nil {
		t.Fatal(err)
	}
	if err = acquire("cli", third); !errors.Is(err, ErrBusy) {
		t.Fatalf("CLI borrowed reserved slot: %v", err)
	}
	if err = acquire("web", ""); err != nil {
		t.Fatal(err)
	}
	if err = acquire("server", ""); !errors.Is(err, ErrBusy) {
		t.Fatalf("total pool exceeded: %v", err)
	}
}
