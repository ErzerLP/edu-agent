package operations

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strings"
)

var (
	bearerPattern         = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*bearer\s+|bearer\s+)[A-Za-z0-9._~+/=-]{8,}`)
	sensitiveLabelPattern = regexp.MustCompile(`(?i)["']?(?:password|passphrase|token|api[_-]?key|secret|pairing[_-]?code|credentials|answer|knowledge(?:_body)?|content|payload|vault)["']?\s*[:=]\s*`)
	unsafeMarkerPattern   = regexp.MustCompile(`(?i)(OPERATIONS_SECRET_SENTINEL|SYNTHETIC_SECRET)`)
)

type RedactionResult struct {
	Text   string
	Unsafe bool
}

func RedactText(value string) RedactionResult {
	parts := strings.SplitAfter(value, "\n")
	var output strings.Builder
	unsafe := false
	for _, part := range parts {
		if part == "" {
			continue
		}
		hadNewline := strings.HasSuffix(part, "\n")
		line := strings.TrimSuffix(part, "\n")
		line = strings.TrimSuffix(line, "\r")
		redacted, lineUnsafe := redactPlainLine(line)
		output.WriteString(redacted)
		if hadNewline {
			output.WriteByte('\n')
		}
		unsafe = unsafe || lineUnsafe
	}
	if value == "" {
		return RedactionResult{}
	}
	return RedactionResult{Text: output.String(), Unsafe: unsafe}
}

func redactPlainLine(line string) (string, bool) {
	if unsafeMarkerPattern.MatchString(line) {
		return "[REDACTED unsafe log line]", true
	}
	line = bearerPattern.ReplaceAllString(line, `${1}[REDACTED]`)
	var output strings.Builder
	cursor := 0
	for cursor < len(line) {
		match := sensitiveLabelPattern.FindStringIndex(line[cursor:])
		if match == nil {
			output.WriteString(line[cursor:])
			break
		}
		labelStart := cursor + match[0]
		valueStart := cursor + match[1]
		output.WriteString(line[cursor:valueStart])
		if valueStart >= len(line) {
			output.WriteString("[REDACTED]")
			cursor = valueStart
			break
		}
		switch line[valueStart] {
		case '\'', '"':
			end, ok := quotedValueEnd(line, valueStart)
			if !ok {
				return "[REDACTED unsafe log line]", true
			}
			output.WriteString("[REDACTED]")
			cursor = end
		case '{', '[':
			return "[REDACTED unsafe log line]", true
		default:
			boundary := len(line)
			if relative := strings.IndexAny(line[valueStart:], ",;}"); relative >= 0 {
				boundary = valueStart + relative
			}
			output.WriteString("[REDACTED]")
			cursor = boundary
		}
		if labelStart == cursor {
			return "[REDACTED unsafe log line]", true
		}
	}
	return output.String(), false
}

func quotedValueEnd(line string, start int) (int, bool) {
	quote := line[start]
	escaped := false
	for position := start + 1; position < len(line); position++ {
		if escaped {
			escaped = false
			continue
		}
		if line[position] == '\\' {
			escaped = true
			continue
		}
		if line[position] == quote {
			return position + 1, true
		}
	}
	return 0, false
}

func RedactLogLine(line []byte) ([]byte, bool) {
	hadNewline := len(line) > 0 && line[len(line)-1] == '\n'
	trimmed := bytes.TrimSuffix(line, []byte{'\n'})
	trimmed = bytes.TrimSuffix(trimmed, []byte{'\r'})
	var value any
	if json.Unmarshal(trimmed, &value) == nil {
		redacted, unsafe := redactJSONValue(value, "")
		if unsafe {
			result := []byte("[REDACTED unsafe log line]")
			if hadNewline {
				result = append(result, '\n')
			}
			return result, true
		}
		encoded, err := json.Marshal(redacted)
		if err == nil {
			if hadNewline {
				encoded = append(encoded, '\n')
			}
			return encoded, false
		}
	}
	redacted := RedactText(string(trimmed))
	result := []byte(redacted.Text)
	if hadNewline {
		result = append(result, '\n')
	}
	return result, redacted.Unsafe
}

func redactJSONValue(value any, key string) (any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		unsafe := false
		for childKey, child := range typed {
			if sensitiveJSONKey(childKey) {
				typed[childKey] = "[REDACTED]"
				continue
			}
			redacted, childUnsafe := redactJSONValue(child, childKey)
			typed[childKey] = redacted
			unsafe = unsafe || childUnsafe
		}
		return typed, unsafe
	case []any:
		unsafe := false
		for position, child := range typed {
			redacted, childUnsafe := redactJSONValue(child, key)
			typed[position] = redacted
			unsafe = unsafe || childUnsafe
		}
		return typed, unsafe
	case string:
		redacted := RedactText(typed)
		return redacted.Text, redacted.Unsafe
	default:
		return value, false
	}
}

func sensitiveJSONKey(key string) bool {
	normalized := strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(key))
	switch normalized {
	case "password", "passphrase", "token", "apikey", "secret", "pairingcode", "credentials", "answer", "knowledge", "knowledgebody", "content", "payload", "vault", "authorization":
		return true
	default:
		return false
	}
}

func RedactStream(reader io.Reader, writer io.Writer) (bool, error) {
	buffered := bufio.NewReaderSize(reader, 64*1024)
	unsafe := false
	for {
		line, err := buffered.ReadBytes('\n')
		if len(line) > 0 {
			redacted, lineUnsafe := RedactLogLine(line)
			unsafe = unsafe || lineUnsafe
			if _, writeErr := writer.Write(redacted); writeErr != nil {
				return unsafe, writeErr
			}
		}
		if err == io.EOF {
			return unsafe, nil
		}
		if err != nil {
			return unsafe, err
		}
	}
}

func ContainsSensitiveLabel(value string) bool {
	lower := strings.ToLower(value)
	for _, label := range []string{"authorization: bearer", "pairing_code=", "pairing code=", "password=", "api_key=", "answer=", "knowledge_body="} {
		if strings.Contains(lower, label) {
			return true
		}
	}
	return unsafeMarkerPattern.MatchString(value)
}
