package migrations

import "testing"

func TestStudioMigrationUpgrade(t *testing.T) {
	pool := migrationPoolThrough(t, 34)
	if err := Run(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	var tables, required int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_class WHERE relnamespace=current_schema()::regnamespace AND relname IN ('learning_studio_artifacts','learning_studio_revisions','learning_studio_operations')`).Scan(&tables); err != nil || tables != 3 {
		t.Fatal(tables, err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='learning_content_artifacts' AND column_name IN ('session_id','activity_id') AND is_nullable='NO'`).Scan(&required); err != nil || required != 2 {
		t.Fatal("legacy constraints weakened", required, err)
	}
	var unique bool
	if err := pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='learning_content_artifacts'::regclass AND contype='u')`).Scan(&unique); err != nil || !unique {
		t.Fatal("legacy uniqueness lost", err)
	}
	if err := Run(t.Context(), pool); err != nil {
		t.Fatal("migration replay", err)
	}
}

func TestStudioQuizMigrationUpgrade(t *testing.T) {
	pool := migrationPoolThrough(t, 36)
	if err := Run(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	var tables int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_class WHERE relnamespace=current_schema()::regnamespace AND relname IN ('learning_studio_quiz_questions','learning_studio_quiz_operations')`).Scan(&tables); err != nil || tables != 2 {
		t.Fatal(tables, err)
	}
	if err := Run(t.Context(), pool); err != nil {
		t.Fatal("migration replay", err)
	}
}
