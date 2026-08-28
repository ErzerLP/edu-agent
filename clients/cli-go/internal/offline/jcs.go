package offline

import (
	"bytes"
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

// CanonicalizeJCS implements the repository RFC 8785 profile. Duplicate keys,
// floating-point numbers, invalid UTF-8, and unpaired UTF-16 escapes are rejected.
func CanonicalizeJCS(input []byte) ([]byte, error) {
	if !utf8.Valid(input) {
		return nil, errorsJCS("input is not UTF-8")
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
			return nil, errorsJCS("input has trailing token %v", token)
		}
		return nil, errorsJCS("input has trailing content")
	}
	var output bytes.Buffer
	if err := appendJCSValue(&output, value); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func requireCanonicalObject(input []byte) ([]byte, error) {
	canonical, err := CanonicalizeJCS(input)
	if err != nil {
		return nil, err
	}
	if len(canonical) < 2 || canonical[0] != '{' || !bytes.Equal(canonical, input) {
		return nil, errorsJCS("value must be a canonical JSON object")
	}
	return canonical, nil
}

func marshalCanonical(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, errorsJCS("encode value")
	}
	return CanonicalizeJCS(encoded)
}

func decodeClosed(input []byte, target any) error {
	canonical, err := requireCanonicalObject(input)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errorsJCS("decode closed object")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errorsJCS("closed object has trailing content")
	}
	return nil
}

func errorsJCS(format string, args ...any) error {
	return fmt.Errorf("invalid closed JCS: "+format, args...)
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
					return errorsJCS("unpaired high surrogate")
				}
				low, ok := parseHex16(input, index+8)
				if !ok || low < 0xdc00 || low > 0xdfff {
					return errorsJCS("unpaired high surrogate")
				}
				index += 11
			case value >= 0xdc00 && value <= 0xdfff:
				return errorsJCS("unpaired low surrogate")
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

type jcsObjectEntry struct {
	key   string
	value any
}
type jcsObject []jcsObjectEntry

func decodeJCSValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, errorsJCS("read token")
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
					return nil, errorsJCS("read object key")
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, errorsJCS("object key is not a string")
				}
				if _, exists := seen[key]; exists {
					return nil, errorsJCS("duplicate key %q", key)
				}
				seen[key] = struct{}{}
				item, err := decodeJCSValue(decoder)
				if err != nil {
					return nil, err
				}
				object = append(object, jcsObjectEntry{key: key, value: item})
			}
			if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
				return nil, errorsJCS("object is not closed")
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
				return nil, errorsJCS("array is not closed")
			}
			return array, nil
		default:
			return nil, errorsJCS("unexpected delimiter")
		}
	case json.Number:
		text := value.String()
		if !canonicalInteger.MatchString(text) || text == "-0" {
			return nil, errorsJCS("floating-point or noncanonical number")
		}
		if _, err := strconv.ParseInt(text, 10, 64); err != nil {
			return nil, errorsJCS("integer is outside int64")
		}
		return json.Number(text), nil
	case string, bool, nil:
		return value, nil
	default:
		return nil, errorsJCS("unsupported token")
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
		return errorsJCS("unsupported value")
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
	l, r := utf16.Encode([]rune(left)), utf16.Encode([]rune(right))
	for index := 0; index < len(l) && index < len(r); index++ {
		if l[index] != r[index] {
			return l[index] < r[index]
		}
	}
	return len(l) < len(r)
}
