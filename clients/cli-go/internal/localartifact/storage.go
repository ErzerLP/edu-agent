package localartifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

const metadataPrefix = "result_m_"

// Segment lengths and names are derived, never provided by storage. The final
// segment is the only short segment; an empty artifact has an empty hash array.
// Owner is a one-way binding digest, not a session name or execution identity.
type metadata struct {
	Version       int      `json:"version"`
	ID            string   `json:"id"`
	Kind          string   `json:"kind"`
	OwnerHash     string   `json:"owner_hash"`
	Hash          string   `json:"hash"`
	Bytes         int64    `json:"bytes"`
	SegmentHashes []string `json:"segment_hashes"`
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validDigest(value string) bool {
	if len(value) != len("sha256:")+2*sha256.Size || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, c := range value[len("sha256:"):] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func validKind(kind string) bool { return kind == "diff" || kind == "receipt" }

func validID(id string) bool {
	if len(id) < 28 || len(id) > 100 || !strings.HasPrefix(id, "r_") {
		return false
	}
	for _, c := range id[2:] {
		if !(c >= 'A' && c <= 'Z' || c >= '2' && c <= '7') {
			return false
		}
	}
	return true
}

func metadataName(id string) string { return metadataPrefix + id }
func segmentName(id string, index int) string {
	return "result_d_" + id + "_" + strconv.Itoa(index)
}

func makeMetadata(owner, id, kind string, data []byte) metadata {
	meta := metadata{Version: 1, ID: id, Kind: kind, OwnerHash: digest([]byte(owner)),
		Hash: digest(data), Bytes: int64(len(data)), SegmentHashes: make([]string, 0, (len(data)+segmentBytes-1)/segmentBytes)}
	for start := 0; start < len(data); start += segmentBytes {
		meta.SegmentHashes = append(meta.SegmentHashes, digest(data[start:min(start+segmentBytes, len(data))]))
	}
	return meta
}

func (meta metadata) info(saved bool) Info {
	return Info{ID: meta.ID, Kind: meta.Kind, Hash: meta.Hash, Bytes: meta.Bytes, Saved: saved}
}

func sameMetadata(a, b metadata) bool {
	return a.Version == b.Version && a.ID == b.ID && a.Kind == b.Kind && a.OwnerHash == b.OwnerHash &&
		a.Hash == b.Hash && a.Bytes == b.Bytes && slices.Equal(a.SegmentHashes, b.SegmentHashes)
}

// decodeMetadata validates shape before allocating the segment slice. Input,
// key count, scalar values and segment count all have independent hard bounds.
// Future versions get an explicit error, including schemas with new fields;
// duplicate keys, nulls, malformed JSON and invalid UTF-8 remain corrupt.
func decodeMetadata(data []byte, owner, id string) (metadata, error) {
	var meta metadata
	bad := func() (metadata, error) { return metadata{}, failure("artifact_corrupt") }
	if len(data) > metadataBytes || !utf8.Valid(data) || !validID(id) {
		return bad()
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return bad()
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || len(key) > 64 || len(fields) >= 64 {
			return bad()
		}
		if _, exists := fields[key]; exists {
			return bad()
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return bad()
		}
		fields[key] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') || decoder.Decode(new(any)) != io.EOF {
		return bad()
	}
	if json.Unmarshal(fields["version"], &meta.Version) != nil {
		return bad()
	}
	if meta.Version > 1 {
		return metadata{}, failure("artifact_version_unsupported")
	}
	if meta.Version != 1 || len(fields) != 7 ||
		json.Unmarshal(fields["id"], &meta.ID) != nil ||
		json.Unmarshal(fields["kind"], &meta.Kind) != nil ||
		json.Unmarshal(fields["owner_hash"], &meta.OwnerHash) != nil ||
		json.Unmarshal(fields["hash"], &meta.Hash) != nil ||
		json.Unmarshal(fields["bytes"], &meta.Bytes) != nil {
		return bad()
	}
	if meta.ID != id || !validKind(meta.Kind) || !validDigest(meta.OwnerHash) || meta.OwnerHash != digest([]byte(owner)) ||
		!validDigest(meta.Hash) || meta.Bytes < 0 || meta.Bytes > maxConfiguredBytes {
		return bad()
	}
	expected := int((meta.Bytes + segmentBytes - 1) / segmentBytes)
	segments := json.NewDecoder(bytes.NewReader(fields["segment_hashes"]))
	if token, err := segments.Token(); err != nil || token != json.Delim('[') {
		return bad()
	}
	meta.SegmentHashes = make([]string, 0, expected)
	for segments.More() {
		if len(meta.SegmentHashes) >= expected {
			return bad()
		}
		var hash string
		if segments.Decode(&hash) != nil || !validDigest(hash) {
			return bad()
		}
		meta.SegmentHashes = append(meta.SegmentHashes, hash)
	}
	if token, err := segments.Token(); err != nil || token != json.Delim(']') ||
		len(meta.SegmentHashes) != expected || segments.Decode(new(any)) != io.EOF {
		return bad()
	}
	if meta.Bytes == 0 && meta.Hash != digest(nil) {
		return bad()
	}
	return meta, nil
}

func readBlob(ctx context.Context, backend Store, name string) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, failure("artifact_canceled")
	}
	callCtx, cancel := boundedContext(ctx, callTimeout)
	defer cancel()
	data, err := backend.ReadArtifact(callCtx, name)
	if err != nil {
		return nil, storeError(err, "artifact_unavailable")
	}
	if callCtx.Err() != nil {
		return nil, failure("artifact_canceled")
	}
	if len(data) > metadataBytes {
		return nil, failure("artifact_corrupt")
	}
	return data, nil
}

func writeBlob(ctx context.Context, backend Store, name string, data []byte) error {
	if ctx.Err() != nil {
		return failure("artifact_canceled")
	}
	callCtx, cancel := boundedContext(ctx, callTimeout)
	defer cancel()
	if err := backend.WriteArtifact(callCtx, name, data); err != nil {
		return storeError(err, "artifact_store_failed")
	}
	// A confirmed write stays confirmed even if its deadline expires just after
	// return. The next write checks cancellation before attempting publication.
	return nil
}

func save(ctx context.Context, backend Store, meta metadata, data []byte) error {
	encoded, err := json.Marshal(meta)
	if err != nil || len(encoded) > metadataBytes {
		return failure("artifact_store_failed")
	}
	for index, start := 0, 0; start < len(data); index, start = index+1, start+segmentBytes {
		// Each store call gets an independent buffer, never the caller's data.
		chunk := append([]byte(nil), data[start:min(start+segmentBytes, len(data))]...)
		if err := writeBlob(ctx, backend, segmentName(meta.ID, index), chunk); err != nil {
			return err
		}
	}
	return writeBlob(ctx, backend, metadataName(meta.ID), encoded)
}

func readSegment(ctx context.Context, backend Store, meta metadata, index int) ([]byte, error) {
	data, err := readBlob(ctx, backend, segmentName(meta.ID, index))
	if err != nil {
		return nil, err
	}
	want := min(int64(segmentBytes), meta.Bytes-int64(index)*segmentBytes)
	if int64(len(data)) != want || digest(data) != meta.SegmentHashes[index] {
		return nil, failure("artifact_corrupt")
	}
	return data, nil
}

// verifyBody streams only segments referenced by the authenticated metadata.
// No segment discovery, plaintext body cache, or execution recovery is possible.
func verifyBody(ctx context.Context, backend Store, meta metadata) error {
	hash := sha256.New()
	for index := range meta.SegmentHashes {
		data, err := readSegment(ctx, backend, meta, index)
		if err != nil {
			return err
		}
		_, _ = hash.Write(data)
	}
	if "sha256:"+hex.EncodeToString(hash.Sum(nil)) != meta.Hash {
		return failure("artifact_corrupt")
	}
	return nil
}
