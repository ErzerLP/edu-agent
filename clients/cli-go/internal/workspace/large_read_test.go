package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestLargeFileReadRangesAndWholeHash(t *testing.T) {
	line := strings.Repeat("0123456789abcdef", 4) + "\r\n"
	body := strings.Repeat(line, 32768)
	raw := "\ufeff" + body
	w, _ := largeReadWorkspace(t, DefaultLimits(), "large.txt", raw)
	value := largeReadPage(t, w, map[string]any{"path": "large.txt", "offset": 26000, "limit": 2})
	if value["content"] != line+line || value["content_hash"] != largeReadHash(raw) || value["hash_scope"] != "whole_file" ||
		intValue(value["file_bytes"]) != len(raw) || value["read_byte_limit"] != DefaultReadFileBytes ||
		intValue(value["total_lines"]) != 32768 || intValue(value["start_line"]) != 26000 || intValue(value["end_line"]) != 26001 ||
		intValue(value["next_offset"]) != 26002 || intValue(value["next_byte_offset"]) != 0 || value["complete"] != false {
		t.Fatal("large-file range, original BOM/CRLF hash, or continuation metadata mismatch")
	}
	window, total, err := readLineWindow(t.Context(), body, 26000, 2)
	if err != nil || len(window) != 2 || total != 32768 || window[0] != line || window[1] != line {
		t.Fatalf("bounded line window: retained=%d total=%d err=%v", len(window), total, err)
	}
}

func TestLargeFileReadLongLineAndMillionLineCursors(t *testing.T) {
	t.Run("long line", func(t *testing.T) {
		body := strings.Repeat("界", 400000) + "\r\nlast\n"
		w, _ := largeReadWorkspace(t, DefaultLimits(), "long.txt", body)
		const byteOffset = 1199970
		value := largeReadPage(t, w, map[string]any{"path": "long.txt", "byte_offset": byteOffset, "limit": 1})
		if value["content"] != body[byteOffset:1200002] || value["content_hash"] != largeReadHash(body) ||
			intValue(value["total_lines"]) != 2 || intValue(value["next_offset"]) != 2 || intValue(value["next_byte_offset"]) != 0 {
			t.Fatal("long-line suffix or boundary cursor mismatch")
		}
		for _, offset := range []int{byteOffset + 1, 1200003} {
			result := largeReadExecute(t, t.Context(), w, map[string]any{"path": "long.txt", "byte_offset": offset})
			if code := resultCode(t, result); code != CodeInvalidArguments {
				t.Fatalf("invalid long-line byte_offset=%d code=%q", offset, code)
			}
		}
	})
	t.Run("million lines", func(t *testing.T) {
		body := strings.Repeat("\n", 1000003) + "last\n"
		w, _ := largeReadWorkspace(t, DefaultLimits(), "lines.txt", body)
		value := largeReadPage(t, w, map[string]any{"path": "lines.txt", "offset": 1000002, "limit": 2})
		if value["content"] != "\n\n" || intValue(value["total_lines"]) != 1000004 ||
			intValue(value["end_line"]) != 1000003 || intValue(value["next_offset"]) != 1000004 {
			t.Fatal("million-line range or cursor mismatch")
		}
		last := largeReadPage(t, w, map[string]any{"path": "lines.txt", "offset": 1000004, "expected_hash": value["content_hash"]})
		if last["content"] != "last\n" || last["complete"] != true || last["content_hash"] != largeReadHash(body) {
			t.Fatal("million-line final page mismatch")
		}
	})
}

func TestLargeFileReadPaginationReassemblesOriginalText(t *testing.T) {
	limits := DefaultLimits()
	limits.ResultBytes = 128 << 10
	limits.ReadLines = 1000
	line := strings.Repeat("界<>&\"\\\t", 32) + "\r\n"
	body := strings.Repeat(line, 4096)
	w, _ := largeReadWorkspace(t, limits, "pages.txt", "\ufeff"+body)
	largeReadReassemble(t, w, "pages.txt", 1, 0, largeReadHash("\ufeff"+body), body)
}

func TestLargeFileReadEscapedWindowKeepsGlobalCursor(t *testing.T) {
	prefix := strings.Repeat("x\n", 1000002)
	body := strings.Repeat(strings.Repeat("<>&\"\\\t界", 20)+"\r\n", 50)
	limits := DefaultLimits()
	limits.ResultBytes = 4096
	w, _ := largeReadWorkspace(t, limits, "escaped.txt", prefix+body)
	largeReadReassemble(t, w, "escaped.txt", 1000003, 3, largeReadHash(prefix+body), body[3:])
}

func TestLargeFileReadLineBoundariesAndEmptyFiles(t *testing.T) {
	for _, test := range []struct {
		name  string
		raw   string
		text  string
		lines int
	}{
		{name: "empty"},
		{name: "BOM only", raw: "\ufeff"},
		{name: "one empty line", raw: "\n", text: "\n", lines: 1},
		{name: "CRLF with BOM", raw: "\ufeffone\r\ntwo\r\n", text: "one\r\ntwo\r\n", lines: 2},
		{name: "trailing blank", raw: "one\n\n", text: "one\n\n", lines: 2},
		{name: "no trailing LF", raw: "one\nlast", text: "one\nlast", lines: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			w, _ := largeReadWorkspace(t, DefaultLimits(), "text.txt", test.raw)
			value := largeReadPage(t, w, map[string]any{"path": "text.txt"})
			if value["content"] != test.text || value["content_hash"] != largeReadHash(test.raw) || value["complete"] != true ||
				intValue(value["total_lines"]) != test.lines || intValue(value["returned_lines"]) != test.lines {
				t.Fatal("empty/newline/BOM compatibility mismatch")
			}
			for _, offset := range []int{test.lines + 1, readOffsetLimit(w.limits)} {
				end := largeReadPage(t, w, map[string]any{"path": "text.txt", "offset": offset})
				if end["content"] != "" || end["complete"] != true || intValue(end["returned_lines"]) != 0 || intValue(end["end_line"]) != 0 || end["next_offset"] != nil {
					t.Fatalf("EOF range mismatch at offset %d", offset)
				}
			}
		})
	}
	t.Run("exact LF budget", func(t *testing.T) {
		limits := DefaultLimits()
		limits.ResultBytes = 2048
		budget := limits.ResultBytes - len("line.txt") - 1400
		line := strings.Repeat("x", budget-1) + "\n"
		w, _ := largeReadWorkspace(t, limits, "line.txt", line+"last\n")
		value := largeReadPage(t, w, map[string]any{"path": "line.txt"})
		if value["content"] != line || intValue(value["next_offset"]) != 2 || intValue(value["next_byte_offset"]) != 0 {
			t.Fatal("result boundary at LF did not advance to next line")
		}
		atLF := largeReadPage(t, w, map[string]any{"path": "line.txt", "byte_offset": len(line) - 1, "limit": 1})
		if atLF["content"] != "\n" || intValue(atLF["next_offset"]) != 2 || intValue(atLF["next_byte_offset"]) != 0 {
			t.Fatal("byte cursor on LF mismatch")
		}
		afterLF := largeReadPage(t, w, map[string]any{"path": "line.txt", "byte_offset": len(line), "limit": 1})
		if afterLF["content"] != "" || intValue(afterLF["next_offset"]) != 2 || intValue(afterLF["next_byte_offset"]) != 0 {
			t.Fatal("byte cursor immediately after LF did not advance")
		}
		end, returned, next, nextByte := readPrefixPosition([]string{line, "last\n"}, 0, len(line), 0)
		if end != 0 || returned != 0 || next != 1 || nextByte != len(line) {
			t.Fatal("empty projected prefix changed the original cursor")
		}
	})
}

func TestLargeFileReadRejectsInvalidUnselectedTailAndChangedHash(t *testing.T) {
	prefix := "selected\n" + strings.Repeat("a", 2<<20)
	for _, test := range []struct{ name, tail, code string }{
		{"invalid UTF8", "\xff", CodeInvalidUTF8},
		{"NUL", "\x00", CodeBinaryFile},
		{"binary controls", "\x01\x02\x03\x04\x05", CodeBinaryFile},
	} {
		t.Run(test.name, func(t *testing.T) {
			w, _ := largeReadWorkspace(t, DefaultLimits(), "invalid.txt", prefix+test.tail)
			result := largeReadExecute(t, t.Context(), w, map[string]any{"path": "invalid.txt", "limit": 1})
			value := resultObject(t, result)
			if resultCode(t, result) != test.code || value["content"] != nil || value["content_hash"] != nil || value["complete"] != false {
				t.Fatalf("unselected invalid tail code=%q want=%q", resultCode(t, result), test.code)
			}
		})
	}
	t.Run("changed outside selected range", func(t *testing.T) {
		w, root := largeReadWorkspace(t, DefaultLimits(), "version.txt", prefix+"x")
		first := largeReadPage(t, w, map[string]any{"path": "version.txt", "limit": 1})
		if err := os.WriteFile(filepath.Join(root, "version.txt"), []byte(prefix+"y"), 0o600); err != nil {
			t.Fatal(err)
		}
		result := largeReadExecute(t, t.Context(), w, map[string]any{"path": "version.txt", "limit": 1, "expected_hash": first["content_hash"]})
		if resultCode(t, result) != CodeContentChanged || result.Reference == nil || !result.Reference.InvalidateObserved ||
			result.Reference.ContentHash != first["content_hash"] || resultObject(t, result)["content_hash"] != nil {
			t.Fatal("unselected version change did not reject/invalidate the old full hash")
		}
	})
}

func TestLargeFileReadIndependentLimitsAndSchemas(t *testing.T) {
	defaults := DefaultLimits()
	if defaults.ReadFileBytes != 64<<20 || defaults.FileBytes != 1<<20 {
		t.Fatal("default read and mutation budgets are not independent")
	}
	for _, test := range []struct {
		name string
		read int64
		file int64
		want int64
	}{
		{name: "independent", read: 64, file: 32, want: 64},
		{name: "legacy explicit limits", read: 0, file: 32, want: 32},
	} {
		t.Run(test.name, func(t *testing.T) {
			limits := defaults
			limits.ReadFileBytes, limits.FileBytes, limits.ReadLines = test.read, test.file, 3
			w, root := largeReadWorkspace(t, limits, "budget.txt", strings.Repeat("x", int(test.want)+1))
			if w.limits.ReadFileBytes != test.want || w.limits.FileBytes != test.file {
				t.Fatal("constructor changed independent/legacy limits")
			}
			result := largeReadExecute(t, t.Context(), w, map[string]any{"path": "budget.txt"})
			value := resultObject(t, result)
			if resultCode(t, result) != CodeFileTooLarge || value["read_byte_limit"] != test.want || value["content_hash"] != nil || result.Reference != nil || value["complete"] != false ||
				!strings.Contains(value["message"].(string), fmt.Sprint(test.want)) || !strings.Contains(value["suggestion"].(string), "--file-read-limit") || !strings.Contains(value["suggestion"].(string), "Shell") {
				t.Fatal("over-budget read did not provide an honest bound/recovery hint")
			}
			if err := os.WriteFile(filepath.Join(root, "budget.txt"), []byte(strings.Repeat("x", int(test.want))), 0o600); err != nil {
				t.Fatal(err)
			}
			atLimit := largeReadPage(t, w, map[string]any{"path": "budget.txt"})
			if atLimit["complete"] != true || intValue(atLimit["file_bytes"]) != int(test.want) {
				t.Fatal("file exactly at read budget was refused")
			}
			for _, args := range []map[string]any{
				{"path": "budget.txt", "offset": readOffsetLimit(w.limits) + 1},
				{"path": "budget.txt", "byte_offset": test.want + 1},
				{"path": "budget.txt", "limit": limits.ReadLines + 1},
				{"path": "budget.txt", "expected_hash": "sha256:bad"},
			} {
				if code := resultCode(t, largeReadExecute(t, t.Context(), w, args)); code != CodeInvalidArguments {
					t.Fatalf("argument beyond configured schema accepted: code=%q", code)
				}
			}
			base, instance := Definitions(), w.Definitions()
			if len(instance) != len(base) {
				t.Fatal("instance definitions changed tool count")
			}
			for index, definition := range instance {
				if definition.Function.Name != ToolRead {
					if !reflect.DeepEqual(definition, base[index]) {
						t.Fatalf("non-read definition changed: %s", definition.Function.Name)
					}
					continue
				}
				var schema struct {
					Properties map[string]struct {
						Maximum int64 `json:"maximum"`
					} `json:"properties"`
				}
				if err := json.Unmarshal(definition.Function.Parameters, &schema); err != nil {
					t.Fatal(err)
				}
				if schema.Properties["offset"].Maximum != 1000000 || schema.Properties["limit"].Maximum != 3 || schema.Properties["byte_offset"].Maximum != test.want ||
					!strings.Contains(definition.Function.Description, fmt.Sprint(test.want)) || !strings.Contains(definition.Function.Description, "whole-file hash") {
					t.Fatal("instance read schema/description does not match effective limits")
				}
			}
			if !reflect.DeepEqual(base, Definitions()) {
				t.Fatal("instance definitions mutated package defaults")
			}
		})
	}
	maxInt := int64(^uint(0) >> 1)
	for _, readLimit := range []int64{-1, maxInt} {
		limits := defaults
		limits.ReadFileBytes = readLimit
		if w, err := OpenWithLimits(t.TempDir(), limits); err == nil {
			w.Close()
			t.Fatalf("unsafe read limit accepted: %d", readLimit)
		}
	}
	limits := defaults
	limits.ReadFileBytes = maxInt - 1
	w, err := OpenWithLimits(t.TempDir(), limits)
	if err != nil {
		t.Fatalf("safe platform read limit refused: %v", err)
	}
	defer w.Close()
	if int64(readOffsetLimit(w.limits)) != maxInt {
		t.Fatal("platform maximum line cursor overflowed")
	}
}

func TestLargeFileReadDoesNotWidenOtherTools(t *testing.T) {
	body := strings.Repeat("x", (1<<20)+1)
	w, _ := largeReadWorkspace(t, DefaultLimits(), "large.txt", body)
	largeReadPage(t, w, map[string]any{"path": "large.txt"})
	if code := resultCode(t, w.Execute(t.Context(), ToolStat, `{"path":"large.txt","hash":true}`)); code != CodeFileTooLarge {
		t.Fatalf("stat.hash budget changed: code=%q", code)
	}
	search := resultObject(t, w.Execute(t.Context(), ToolSearch, `{"path":"large.txt","query":"x"}`))
	if search["complete"] != false || search["truncation_reason"] != "file_too_large" {
		t.Fatal("search accepted a file beyond its original budget")
	}
	for _, tool := range []string{ToolWrite, ToolEdit} {
		args := map[string]any{"path": "large.txt", "expected_hash": largeReadHash(body)}
		if tool == ToolWrite {
			args["mode"], args["content"] = "replace", "small"
		} else {
			args["edits"] = []map[string]string{{"old_text": "x", "new_text": "y"}}
		}
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		prepared, result := w.PrepareMutation(t.Context(), tool, string(raw))
		if prepared != nil || resultCode(t, result) != CodeFileTooLarge {
			t.Fatalf("%s existing-file budget was widened", tool)
		}
	}
}

func TestLargeFileReadCancellationAndEntryBoundaries(t *testing.T) {
	w, root := largeReadWorkspace(t, DefaultLimits(), "large.txt", strings.Repeat("x", 2<<20))
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	timedOut, stop := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer stop()
	for _, test := range []struct {
		ctx  context.Context
		code string
	}{{cancelled, CodeCancelled}, {timedOut, CodeTimeout}} {
		result := largeReadExecute(t, test.ctx, w, map[string]any{"path": "large.txt"})
		if resultCode(t, result) != test.code || resultObject(t, result)["content"] != nil {
			t.Fatalf("cancelled/deadline read code=%q want=%q", resultCode(t, result), test.code)
		}
	}
	during, cancelDuring := context.WithCancel(t.Context())
	defer cancelDuring()
	during = WithProgressReporter(during, func(progress Progress) {
		if progress.Bytes > 0 {
			cancelDuring()
		}
	})
	if code := resultCode(t, largeReadExecute(t, during, w, map[string]any{"path": "large.txt"})); code != CodeCancelled {
		t.Fatalf("cancellation after snapshot returned code=%q", code)
	}
	if _, _, err := readLineWindow(cancelled, "x\n", 1, 1); err != context.Canceled {
		t.Fatalf("line scan cancellation=%v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked-parent")); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"../secret.txt":            CodePathOutsideWorkspace,
		"link.txt":                 CodeLinkNotAllowed,
		"linked-parent/secret.txt": CodeLinkNotAllowed,
		"directory":                CodeUnsupportedType,
	} {
		result := largeReadExecute(t, t.Context(), w, map[string]any{"path": path})
		encoded, err := json.Marshal(result.Value)
		if err != nil {
			t.Fatal(err)
		}
		code := resultCode(t, result)
		// O_DIRECTORY|O_NOFOLLOW can report ENOTDIR for a linked parent.
		// Both stable classifications reject traversal and expose no body.
		validCode := code == want || path == "linked-parent/secret.txt" && code == CodeNotDirectory
		if !validCode || resultObject(t, result)["content"] != nil || strings.Contains(string(encoded), outside) || strings.Contains(string(encoded), root) {
			t.Fatalf("entry boundary %q code=%q want=%q", path, code, want)
		}
	}
}

func TestLargeFileReadOversizeMetadataFailsWithinResultBudget(t *testing.T) {
	limits := DefaultLimits()
	limits.ResultBytes = 1024
	path := strings.Repeat(strings.Repeat("界", 70)+"/", 4) + "file.txt"
	w, _ := largeReadWorkspace(t, limits, path, "content\n")
	result := largeReadExecute(t, t.Context(), w, map[string]any{"path": path})
	value := resultObject(t, result)
	if resultCode(t, result) != CodeInvalidArguments || value["complete"] != false || value["content"] != nil || safeResultJSONSize(value) > limits.ResultBytes {
		t.Fatal("unrepresentable metadata produced oversized output or a stuck success page")
	}
}

func largeReadWorkspace(t *testing.T, limits Limits, path, raw string) (*Workspace, string) {
	t.Helper()
	root := t.TempDir()
	fullPath := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := OpenWithLimits(root, limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	return w, root
}

func largeReadHash(raw string) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(raw)))
}

func largeReadExecute(t *testing.T, ctx context.Context, w *Workspace, args map[string]any) Result {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return w.Execute(ctx, ToolRead, string(raw))
}

func largeReadPage(t *testing.T, w *Workspace, args map[string]any) map[string]any {
	t.Helper()
	result := largeReadExecute(t, t.Context(), w, args)
	if code := resultCode(t, result); code != "" {
		t.Fatalf("read failed: code=%q", code)
	}
	value := resultObject(t, result)
	if safeResultJSONSize(value) > w.limits.ResultBytes {
		t.Fatalf("read result exceeded budget: bytes=%d limit=%d", safeResultJSONSize(value), w.limits.ResultBytes)
	}
	return value
}

func largeReadReassemble(t *testing.T, w *Workspace, path string, offset, byteOffset int, hash, want string) {
	t.Helper()
	var rebuilt strings.Builder
	for page := 0; page < 128; page++ {
		value := largeReadPage(t, w, map[string]any{"path": path, "offset": offset, "byte_offset": byteOffset, "expected_hash": hash})
		content, ok := value["content"].(string)
		if !ok || content == "" || !utf8.ValidString(content) || value["content_hash"] != hash || value["hash_scope"] != "whole_file" {
			t.Fatalf("page %d has invalid/empty text or changed full hash", page)
		}
		if !strings.HasPrefix(want[rebuilt.Len():], content) {
			t.Fatalf("page %d repeated or skipped original bytes at byte %d", page, rebuilt.Len())
		}
		rebuilt.WriteString(content)
		if value["complete"] == true {
			if rebuilt.String() != want {
				t.Fatalf("reassembled bytes=%d want=%d", rebuilt.Len(), len(want))
			}
			return
		}
		newlines := strings.Count(content, "\n")
		wantOffset, wantByte := offset+newlines, byteOffset+len(content)
		if newlines > 0 {
			wantByte = len(content) - strings.LastIndexByte(content, '\n') - 1
		}
		nextOffset, nextByte := intValue(value["next_offset"]), intValue(value["next_byte_offset"])
		if nextOffset != wantOffset || nextByte != wantByte {
			t.Fatalf("page %d cursor=%d/%d want=%d/%d", page, nextOffset, nextByte, wantOffset, wantByte)
		}
		endLine := wantOffset
		if strings.HasSuffix(content, "\n") {
			endLine--
		}
		if intValue(value["end_line"]) != endLine || intValue(value["returned_lines"]) != endLine-offset+1 {
			t.Fatalf("page %d retained-line metadata is not global/accurate", page)
		}
		offset, byteOffset = nextOffset, nextByte
	}
	t.Fatalf("pagination did not finish within 128 pages: bytes=%d want=%d", rebuilt.Len(), len(want))
}
