package workspace

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode"
	"unicode/utf8"
)

var utf8BOM = []byte{0xef, 0xbb, 0xbf}

type decodedText struct {
	Text string
	Hash string
}

func decodeText(data []byte) (decodedText, error) {
	hash := sha256.Sum256(data)
	textBytes := bytes.TrimPrefix(data, utf8BOM)
	if !utf8.Valid(textBytes) {
		return decodedText{}, operationFailure(CodeInvalidUTF8, "workspace file is not valid UTF-8")
	}
	if looksBinary(textBytes) {
		return decodedText{}, operationFailure(CodeBinaryFile, "workspace file appears to be binary")
	}
	return decodedText{Text: string(textBytes), Hash: "sha256:" + hex.EncodeToString(hash[:])}, nil
}

func looksBinary(data []byte) bool {
	controls := 0
	for _, current := range string(data) {
		if current == 0 {
			return true
		}
		if unicode.IsControl(current) && current != '\n' && current != '\r' && current != '\t' && current != '\f' {
			controls++
			if controls > 4 {
				return true
			}
		}
	}
	return false
}

func truncateUTF8Bytes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func splitTextLines(value string) []string {
	if value == "" {
		return []string{}
	}
	lines := strings.SplitAfter(value, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func trimLineEnding(value string) string {
	value = strings.TrimSuffix(value, "\n")
	return strings.TrimSuffix(value, "\r")
}

func hasUpper(value string) bool {
	for _, current := range value {
		if unicode.IsUpper(current) {
			return true
		}
	}
	return false
}
