package learning

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

var canonicalInteger = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

// ParseUint63Decimal validates the canonical decimal wire form used by signed payloads.
func ParseUint63Decimal(value string) (uint64, error) {
	if value == "" || value == "-0" || !regexp.MustCompile(`^(0|[1-9][0-9]*)$`).MatchString(value) {
		return 0, fmt.Errorf("invalid uint63 decimal")
	}
	parsed, err := strconv.ParseUint(value, 10, 63)
	if err != nil || parsed > MaxUint63 {
		return 0, fmt.Errorf("uint63 decimal out of range")
	}
	return parsed, nil
}

func FormatUint63Decimal(value uint64) (string, error) {
	if value > MaxUint63 {
		return "", fmt.Errorf("uint63 decimal out of range")
	}
	return strconv.FormatUint(value, 10), nil
}

// CanonicalizeJCS implements the repository's RFC 8785 profile: duplicate keys
// and floating-point numbers are rejected, while objects use UTF-16 key order.
func CanonicalizeJCS(input []byte) ([]byte, error) {
	if !utf8.Valid(input) {
		return nil, fmt.Errorf("JCS input is not UTF-8")
	}
	if err := validateJCSUnicodeEscapes(input); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	value, err := decodeJCSValue(decoder)
	if err != nil {
		return nil, err
	}
	if token, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("JCS input has trailing token %v", token)
		}
		return nil, fmt.Errorf("JCS input has trailing content: %w", err)
	}
	var output bytes.Buffer
	if err := appendJCSValue(&output, value); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func validateJCSUnicodeEscapes(input []byte) error {
	inString := false
	for index := 0; index < len(input); index++ {
		switch input[index] {
		case '"':
			inString = !inString
		case '\\':
			if !inString || index+1 >= len(input) {
				continue
			}
			if input[index+1] != 'u' {
				index++
				continue
			}
			value, ok := parseHex16(input, index+2)
			if !ok {
				continue
			}
			switch {
			case value >= 0xd800 && value <= 0xdbff:
				if index+12 > len(input) || input[index+6] != '\\' || input[index+7] != 'u' {
					return fmt.Errorf("JCS input contains an unpaired high surrogate")
				}
				low, ok := parseHex16(input, index+8)
				if !ok || low < 0xdc00 || low > 0xdfff {
					return fmt.Errorf("JCS input contains an unpaired high surrogate")
				}
				index += 11
			case value >= 0xdc00 && value <= 0xdfff:
				return fmt.Errorf("JCS input contains an unpaired low surrogate")
			default:
				index += 5
			}
		}
	}
	return nil
}

func parseHex16(input []byte, start int) (uint16, bool) {
	if start+4 > len(input) {
		return 0, false
	}
	var value uint16
	for _, current := range input[start : start+4] {
		value <<= 4
		switch {
		case current >= '0' && current <= '9':
			value += uint16(current - '0')
		case current >= 'a' && current <= 'f':
			value += uint16(current-'a') + 10
		case current >= 'A' && current <= 'F':
			value += uint16(current-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func JCSSHA256(input []byte) (string, error) {
	canonical, err := CanonicalizeJCS(input)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

type jcsObjectEntry struct {
	key   string
	value any
}

type jcsObject []jcsObjectEntry

func decodeJCSValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch value := token.(type) {
	case json.Delim:
		switch value {
		case '{':
			seen := map[string]struct{}{}
			object := jcsObject{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, fmt.Errorf("JCS object key is not a string")
				}
				if _, duplicate := seen[key]; duplicate {
					return nil, fmt.Errorf("JCS object contains duplicate key %q", key)
				}
				seen[key] = struct{}{}
				item, err := decodeJCSValue(decoder)
				if err != nil {
					return nil, err
				}
				object = append(object, jcsObjectEntry{key: key, value: item})
			}
			if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
				return nil, fmt.Errorf("JCS object is not closed")
			}
			return object, nil
		case '[':
			array := []any{}
			for decoder.More() {
				item, err := decodeJCSValue(decoder)
				if err != nil {
					return nil, err
				}
				array = append(array, item)
			}
			if end, err := decoder.Token(); err != nil || end != json.Delim(']') {
				return nil, fmt.Errorf("JCS array is not closed")
			}
			return array, nil
		default:
			return nil, fmt.Errorf("unexpected JCS delimiter")
		}
	case json.Number:
		text := value.String()
		if !canonicalInteger.MatchString(text) {
			return nil, fmt.Errorf("JCS profile rejects floating-point number %q", text)
		}
		if text == "-0" {
			text = "0"
		}
		if _, err := strconv.ParseInt(text, 10, 64); err != nil {
			return nil, fmt.Errorf("JCS integer is outside int64: %w", err)
		}
		return json.Number(text), nil
	case string, bool, nil:
		return value, nil
	default:
		return nil, fmt.Errorf("unsupported JCS token %T", token)
	}
}

func appendJCSValue(output *bytes.Buffer, value any) error {
	switch value := value.(type) {
	case nil:
		output.WriteString("null")
	case bool:
		if value {
			output.WriteString("true")
		} else {
			output.WriteString("false")
		}
	case string:
		appendJCSString(output, value)
	case json.Number:
		output.WriteString(value.String())
	case []any:
		output.WriteByte('[')
		for index, item := range value {
			if index > 0 {
				output.WriteByte(',')
			}
			if err := appendJCSValue(output, item); err != nil {
				return err
			}
		}
		output.WriteByte(']')
	case jcsObject:
		sort.Slice(value, func(i, j int) bool { return utf16Less(value[i].key, value[j].key) })
		output.WriteByte('{')
		for index, entry := range value {
			if index > 0 {
				output.WriteByte(',')
			}
			appendJCSString(output, entry.key)
			output.WriteByte(':')
			if err := appendJCSValue(output, entry.value); err != nil {
				return err
			}
		}
		output.WriteByte('}')
	default:
		return fmt.Errorf("unsupported JCS value %T", value)
	}
	return nil
}

func appendJCSString(output *bytes.Buffer, value string) {
	output.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"', '\\':
			output.WriteByte('\\')
			output.WriteRune(r)
		case '\b':
			output.WriteString(`\b`)
		case '\t':
			output.WriteString(`\t`)
		case '\n':
			output.WriteString(`\n`)
		case '\f':
			output.WriteString(`\f`)
		case '\r':
			output.WriteString(`\r`)
		default:
			if r < 0x20 {
				fmt.Fprintf(output, `\u%04x`, r)
			} else {
				output.WriteRune(r)
			}
		}
	}
	output.WriteByte('"')
}

func utf16Less(left, right string) bool {
	l := utf16.Encode([]rune(left))
	r := utf16.Encode([]rune(right))
	for index := 0; index < len(l) && index < len(r); index++ {
		if l[index] != r[index] {
			return l[index] < r[index]
		}
	}
	return len(l) < len(r)
}
