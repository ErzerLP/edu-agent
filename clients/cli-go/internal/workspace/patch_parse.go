package workspace

import "strings"

type patchFile struct {
	action, path string
	added        []string
	hunks        []patchHunk
}

type patchHunk struct {
	lines []string // first byte is exactly one of space, minus, plus
	eof   bool
}

func invalidPatch(message string) error { return operationFailure(CodeInvalidPatch, message) }

func parsePatch(text string) ([]patchFile, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if strings.ContainsRune(text, '\r') {
		return nil, invalidPatch("补丁只支持 LF 或 CRLF 行分隔符")
	}
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(lines) < 3 || lines[0] != "*** Begin Patch" || lines[len(lines)-1] != "*** End Patch" {
		return nil, invalidPatch("补丁必须由 Begin Patch / End Patch 完整包围，不能附带其他指令")
	}
	var files []patchFile
	for at := 1; at < len(lines)-1; {
		var file patchFile
		for _, action := range []string{"Add", "Update", "Delete"} {
			if path, ok := strings.CutPrefix(lines[at], "*** "+action+" File: "); ok {
				file.action, file.path = action, path
				break
			}
		}
		if file.action == "" {
			return nil, invalidPatch("只支持 Add/Update/Delete File；不支持 Move to、二进制或 Git 指令")
		}
		at++
		switch file.action {
		case "Add":
			for at < len(lines)-1 && strings.HasPrefix(lines[at], "+") {
				file.added = append(file.added, lines[at][1:])
				at++
			}
		case "Update":
			for at < len(lines)-1 && strings.HasPrefix(lines[at], "@@") {
				if lines[at] != "@@" {
					return nil, invalidPatch("只支持裸 @@，不支持附加行号或模糊定位语法")
				}
				at++
				hunk := patchHunk{}
				changed := false
				for at < len(lines)-1 && len(lines[at]) > 0 && strings.ContainsRune(" +-", rune(lines[at][0])) {
					hunk.lines = append(hunk.lines, lines[at])
					changed = changed || lines[at][0] != ' '
					at++
				}
				if at < len(lines)-1 && lines[at] == "*** End of File" {
					hunk.eof = true
					at++
				}
				if !changed {
					return nil, invalidPatch("每个 hunk 必须包含新增或删除正文")
				}
				file.hunks = append(file.hunks, hunk)
				if hunk.eof && at < len(lines)-1 && strings.HasPrefix(lines[at], "@@") {
					return nil, invalidPatch("EOF hunk 必须是该文件最后一个 hunk")
				}
			}
			if len(file.hunks) == 0 {
				return nil, invalidPatch("Update File 至少需要一个裸 @@ hunk")
			}
		}
		if at < len(lines)-1 && lines[at] == "\\ No newline at end of file" {
			return nil, invalidPatch("输入不支持 No newline 标记；更新自动保留原文件的末尾换行约定")
		}
		files = append(files, file)
		if len(files) > MaxPatchFiles {
			return nil, invalidPatch("每个补丁最多包含 16 个文件")
		}
	}
	return files, nil
}

func validatePatchPaths(files []patchFile, hashes map[string]string) error {
	existing := 0
	for i := range files {
		path, err := normalizeModelPath(files[i].path, false)
		if err != nil {
			return err
		}
		files[i].path = path
		for j := 0; j < i; j++ {
			if patchPathsConflict(path, files[j].path) {
				return operationFailure(CodePatchPathConflict, "补丁路径重复、大小写别名或祖先/后代冲突")
			}
		}
		hash, present := hashes[path]
		if files[i].action == "Add" {
			if present {
				return argumentError("新增文件不得提供旧 hash")
			}
		} else {
			existing++
			if !present || !validContentHash(hash) {
				return argumentError("每个修改或删除文件都必须提供规范路径对应的完整 hash")
			}
		}
	}
	if len(hashes) != existing {
		return argumentError("expected_hashes 不得包含多余或非规范路径键")
	}
	return nil
}

func patchPathsConflict(a, b string) bool {
	left, right := strings.Split(a, "/"), strings.Split(b, "/")
	for i := 0; i < min(len(left), len(right)); i++ {
		if !strings.EqualFold(left[i], right[i]) {
			return false
		}
		if left[i] != right[i] {
			return true
		} // also reject differently cased parent aliases
	}
	return true // identical path or an ancestor/descendant pair
}
