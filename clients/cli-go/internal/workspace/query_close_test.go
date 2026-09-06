package workspace

import "testing"

func TestQueryPaginationConcurrentClose(t *testing.T) {
	root := t.TempDir()
	for range 16 {
		w, err := Open(root)
		if err != nil {
			t.Fatal(err)
		}
		start, done := make(chan struct{}), make(chan Result, 1)
		go func() { <-start; done <- w.Execute(t.Context(), ToolList, `{}`) }()
		close(start)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		result := <-done
		if code := resultCode(t, result); code != "" && code != CodeWorkspaceUnavailable {
			t.Fatalf("query/close: %+v", result)
		}
		if w.queryBytes != 0 || len(w.queries) != 0 {
			t.Fatal("closed workspace retained query resources")
		}
	}
}
