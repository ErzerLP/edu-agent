package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"strings"
	"time"
)

type ProtocolError struct {
	Category       string
	Method         string
	Path           string
	Status         int
	ContentType    string
	RequestID      string
	DecodeCategory string
	Field          string
}

func (e *ProtocolError) Error() string {
	return "api protocol error: " + e.Category + e.Diagnostics()
}

// Diagnostics 只包含本地请求路径及有界结构信息，不保留响应正文或底层错误文本。
func (e *ProtocolError) Diagnostics() string {
	text := ""
	if e.Method != "" {
		text = fmt.Sprintf(" method=%s path=%s status=%d content_type=%s", e.Method, e.Path, e.Status, e.ContentType)
	}
	if e.RequestID != "" {
		text += " request_id=" + e.RequestID
	}
	if e.DecodeCategory != "" {
		text += " decode=" + e.DecodeCategory
	}
	if e.Field != "" {
		text += " field=" + e.Field
	}
	return text
}

type decodeIssue struct{ category, field string }

func (e *decodeIssue) Error() string { return e.category + " " + e.field }

func classifyDecodeError(err error) (string, string) {
	var issue *decodeIssue
	if errors.As(err, &issue) {
		return issue.category, issue.field
	}
	var mismatch *json.UnmarshalTypeError
	if errors.As(err, &mismatch) {
		return "type_mismatch", ""
	}
	var timestamp *time.ParseError
	if errors.As(err, &timestamp) {
		return "invalid_timestamp", ""
	}
	if strings.HasPrefix(err.Error(), "json: unknown field ") {
		// 未知字段名也可能是学习正文，不能回显。
		return "unknown_field", ""
	}
	return "invalid_json", ""
}

func responseContentType(value string) string {
	if value == "" {
		return "missing"
	}
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return "invalid"
	}
	switch strings.ToLower(mediaType) {
	case "application/json", "text/html", "text/plain", "application/octet-stream":
		return strings.ToLower(mediaType)
	}
	return "other"
}

func safeRequestID(value string) string {
	if len(value) > 128 {
		return ""
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-._/", c)) {
			return ""
		}
	}
	return value
}
