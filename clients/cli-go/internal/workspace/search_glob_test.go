package workspace

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSearchRecursiveGlobAndLegacyIntersection(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"src/a.go", "src/deep/b.go", "src/deep/c.txt", "other.go"} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("needle"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, test := range []struct {
		raw  string
		want []string
	}{
		{`{"query":"needle","output":"files","glob":"src/**/*.go"}`, []string{"src/a.go", "src/deep/b.go"}},
		{`{"query":"needle","path":"src","output":"files","glob":"src/**/*.go","include":["src/*.go"]}`, []string{"src/a.go"}},
		{`{"query":"needle","output":"files","glob":"src/**/*.go","exclude":["b.go"]}`, []string{"src/a.go"}},
		{`{"query":"needle","path":"src/a.go","output":"files","glob":"*.txt"}`, []string{}},
		{`{"query":"needle","output":"files","glob":"*.go"}`, []string{"other.go", "src/a.go", "src/deep/b.go"}},
	} {
		result := w.Execute(t.Context(), ToolSearch, test.raw)
		value := resultObject(t, result)
		if _, failed := value["error"]; failed {
			t.Fatalf("%s: %+v", test.raw, value)
		}
		if !reflect.DeepEqual(value["files"], test.want) || value["complete"] != true {
			t.Fatalf("%s: %+v want=%v", test.raw, value, test.want)
		}
	}
	for _, raw := range []string{`{"query":"x","glob":null}`, `{"query":"x","glob":""}`, `{"query":"x","glob":"["}`, `{"query":"x","glob":"../*"}`} {
		if resultCode(t, w.Execute(t.Context(), ToolSearch, raw)) != CodeInvalidArguments {
			t.Fatalf("accepted %s", raw)
		}
	}
}
