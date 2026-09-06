package workspace

import (
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type patchArguments struct {
	Patch          string            `json:"patch"`
	ExpectedHashes map[string]string `json:"expected_hashes"`
}

func decodePatchArguments(raw string) (patchArguments, error) {
	var args patchArguments
	if !utf8.ValidString(raw) || !validPatchJSONUnicode(raw) {
		return args, argumentError("patch arguments must be valid UTF-8 without unpaired surrogates")
	}
	fields, err := patchJSONObject(raw)
	if err != nil || len(fields) != 2 || fields["patch"] == nil || fields["expected_hashes"] == nil {
		return args, argumentError("patch requires exactly patch and expected_hashes")
	}
	if string(fields["patch"]) == "null" || json.Unmarshal(fields["patch"], &args.Patch) != nil || !validPatchText(args.Patch) {
		return args, argumentError("patch must be a text string without unsafe controls")
	}
	hashes, err := patchJSONObject(string(fields["expected_hashes"]))
	if err != nil {
		return args, err
	}
	args.ExpectedHashes = make(map[string]string, len(hashes))
	for path, rawHash := range hashes {
		var hash string
		if string(rawHash) == "null" || json.Unmarshal(rawHash, &hash) != nil || !validContentHash(hash) {
			return args, argumentError("expected_hashes values must be sha256 content versions")
		}
		args.ExpectedHashes[path] = hash
	}
	return args, nil
}

// Unlike ordinary struct decoding, this rejects duplicate keys (including
// escaped aliases), null objects, unknown trailing values and non-objects.
func patchJSONObject(raw string) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(strings.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, argumentError("patch field must be an object")
	}
	fields := make(map[string]json.RawMessage)
	for d.More() {
		token, err := d.Token()
		key, ok := token.(string)
		if err != nil || !ok || fields[key] != nil {
			return nil, argumentError("patch object contains duplicate or invalid keys")
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return nil, argumentError("invalid patch JSON value")
		}
		fields[key] = value
	}
	if token, err = d.Token(); err != nil || token != json.Delim('}') {
		return nil, argumentError("invalid patch JSON object")
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, argumentError("patch arguments contain trailing JSON")
	}
	return fields, nil
}

// encoding/json substitutes U+FFFD for unpaired JSON surrogates. Reject them
// before decoding so exact patch text can never be silently repaired.
func validPatchJSONUnicode(raw string) bool {
	inString := false
	for i := 0; i < len(raw); i++ {
		if raw[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) {
			return false
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return false
		}
		n, err := strconv.ParseUint(raw[i+1:i+5], 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return false
		}
		if n < 0xd800 || n > 0xdbff {
			continue
		}
		if i+6 >= len(raw) || raw[i+1:i+3] != "\\u" {
			return false
		}
		low, err := strconv.ParseUint(raw[i+3:i+7], 16, 16)
		if err != nil || low < 0xdc00 || low > 0xdfff {
			return false
		}
		i += 6
	}
	return true
}

func validPatchText(text string) bool {
	if !utf8.ValidString(text) {
		return false
	}
	for _, r := range text {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return false
		}
	}
	return true
}
