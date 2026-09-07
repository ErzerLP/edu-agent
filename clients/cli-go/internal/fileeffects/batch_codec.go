package fileeffects

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type batchMarker struct {
	Version   int    `json:"version"`
	ID        string `json:"id"`
	OwnerHash string `json:"owner_hash"`
	CallHash  string `json:"call_hash"`
}
type batchSegment struct {
	Bytes int64  `json:"bytes"`
	Hash  string `json:"hash"`
}
type batchMetadata struct {
	Version   int            `json:"version"`
	ID        string         `json:"id"`
	OwnerHash string         `json:"owner_hash"`
	CallHash  string         `json:"call_hash"`
	PlanHash  string         `json:"plan_hash"`
	PlanBytes int64          `json:"plan_bytes"`
	Items     int            `json:"items"`
	Plan      []batchSegment `json:"plan_segments"`
	Events    []batchSegment `json:"event_segments"`
	Hash      string         `json:"hash"`
	Bytes     int64          `json:"bytes"`
	Sequence  int            `json:"sequence"`
	Started   int            `json:"started"`
	Completed int            `json:"completed"`
	Unchanged int            `json:"unchanged"`
	Unknown   int            `json:"unknown"`
	Finished  bool           `json:"finished"`
	Complete  bool           `json:"complete"`
	Code      string         `json:"code"`
}
type batchRootLine struct {
	Version int    `json:"version"`
	Type    string `json:"type"`
	Root    Effect `json:"root"`
}
type batchPlanLine struct {
	Version int       `json:"version"`
	Type    string    `json:"type"`
	Index   int       `json:"index"`
	Item    BatchItem `json:"item"`
}
type batchPendingLine struct {
	Version  int    `json:"version"`
	Type     string `json:"type"`
	Sequence int    `json:"sequence"`
	Index    int    `json:"index"`
}
type batchActualLine struct {
	Version  int         `json:"version"`
	Type     string      `json:"type"`
	Sequence int         `json:"sequence"`
	Index    int         `json:"index"`
	Actual   BatchActual `json:"actual"`
}
type batchEndLine struct {
	Version  int    `json:"version"`
	Type     string `json:"type"`
	Sequence int    `json:"sequence"`
	Complete bool   `json:"complete"`
	Code     string `json:"code"`
}

func batchValidCall(s string) bool { return s != "" && len(s) <= 4096 && utf8.ValidString(s) }
func batchValidDigest(s string) bool {
	if len(s) != 71 || !strings.HasPrefix(s, "sha256:") {
		return false
	}
	for _, r := range s[7:] {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
func batchValidID(s string) bool {
	return len(s) == 34 && strings.HasPrefix(s, "b_") && batchValidDigest("sha256:"+s[2:]+strings.Repeat("0", 32))
}
func batchOwnerHash(owner string) string { return batchHash([]byte("file-batch-owner-v1\x00" + owner)) }
func batchID(ownerHash, callHash string) string {
	return "b_" + batchHash([]byte("file-batch-id-v1\x00" + ownerHash + "\x00" + callHash))[7:39]
}
func batchMakeMarker(owner, call string) batchMarker {
	o := batchOwnerHash(owner)
	c := batchHash([]byte("file-batch-call-v1\x00" + o + "\x00" + call))
	return batchMarker{Version: 1, OwnerHash: o, CallHash: c, ID: batchID(o, c)}
}
func batchMarkerName(c string) string { return "result_bi_" + strings.TrimPrefix(c, "sha256:") }
func batchMetaName(id string) string  { return "result_bm_" + id }
func batchSegmentName(id string, plan bool, index int) string {
	prefix := "result_be_"
	if plan {
		prefix = "result_bp_"
	}
	return prefix + id + "_" + strconv.Itoa(index)
}
func batchCode(s string) bool {
	if len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	return true
}
func batchLine(v any) []byte { data, _ := json.Marshal(v); return append(data, '\n') }
func batchCloneMeta(v batchMetadata) batchMetadata {
	v.Plan = append([]batchSegment{}, v.Plan...)
	v.Events = append([]batchSegment{}, v.Events...)
	return v
}
func batchSameMeta(a, b batchMetadata) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// Scan tokens first, before interpreting a version, rejecting duplicate keys,
// null, malformed UTF-8 and excessive nesting even in future-version DTOs.
func batchScanJSON(d *json.Decoder, depth int) error {
	if depth > 16 {
		return batchError("corrupt")
	}
	t, err := d.Token()
	if err != nil || t == nil {
		return batchError("corrupt")
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for d.More() {
			k, e := d.Token()
			s, ok := k.(string)
			if e != nil || !ok || len(s) > 64 || seen[s] || len(seen) >= 64 {
				return batchError("corrupt")
			}
			seen[s] = true
			if e = batchScanJSON(d, depth+1); e != nil {
				return e
			}
		}
		t, err = d.Token()
		if err != nil || t != json.Delim('}') {
			return batchError("corrupt")
		}
	case '[':
		for d.More() {
			if err = batchScanJSON(d, depth+1); err != nil {
				return err
			}
		}
		t, err = d.Token()
		if err != nil || t != json.Delim(']') {
			return batchError("corrupt")
		}
	default:
		return batchError("corrupt")
	}
	return nil
}

// Exact field spelling and required fields are checked recursively. Go's
// case-insensitive JSON field matching is deliberately not a storage contract.
func batchShape(data []byte, t reflect.Type) error {
	switch t.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) != nil || fields == nil {
			return batchError("corrupt")
		}
		known := make(map[string]bool)
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag := strings.Split(f.Tag.Get("json"), ",")
			name := tag[0]
			if name == "" {
				name = f.Name
			}
			known[name] = true
			v, ok := fields[name]
			optional := len(tag) > 1 && tag[1] == "omitempty"
			if !ok {
				if !optional {
					return batchError("corrupt")
				}
				continue
			}
			if err := batchShape(v, f.Type); err != nil {
				return err
			}
		}
		for k := range fields {
			if !known[k] {
				return batchError("corrupt")
			}
		}
	case reflect.Slice:
		var entries []json.RawMessage
		if json.Unmarshal(data, &entries) != nil || entries == nil {
			return batchError("corrupt")
		}
		for _, v := range entries {
			if err := batchShape(v, t.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}
func batchDecode(data []byte, out any) error {
	if len(data) > batchMetadataBytes || !utf8.Valid(data) {
		return batchError("corrupt")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := batchScanJSON(d, 0); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return batchError("corrupt")
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(data, &envelope) != nil || envelope == nil {
		return batchError("corrupt")
	}
	var version int
	if json.Unmarshal(envelope["version"], &version) != nil {
		return batchError("corrupt")
	}
	if version > 1 {
		return batchError("version_unsupported")
	}
	if version != 1 {
		return batchError("corrupt")
	}
	if err := batchShape(data, reflect.TypeOf(out).Elem()); err != nil {
		return err
	}
	if json.Unmarshal(data, out) != nil {
		return batchError("corrupt")
	}
	return nil
}
func batchDecodeMarker(data []byte, owner, name string) (batchMarker, error) {
	var v batchMarker
	if len(data) > 4096 {
		return v, batchError("corrupt")
	}
	if err := batchDecode(data, &v); err != nil {
		return v, err
	}
	if v.OwnerHash != batchOwnerHash(owner) || !batchValidDigest(v.CallHash) || v.ID != batchID(v.OwnerHash, v.CallHash) || name != batchMarkerName(v.CallHash) {
		return v, batchError("corrupt")
	}
	return v, nil
}
func (m *BatchManager) decodeMeta(data []byte, owner, id string) (batchMetadata, error) {
	var v batchMetadata
	if err := batchDecode(data, &v); err != nil {
		return v, err
	}
	bad := func() (batchMetadata, error) { return batchMetadata{}, batchError("corrupt") }
	if v.ID != id || !batchValidID(id) || v.OwnerHash != batchOwnerHash(owner) || !batchValidDigest(v.CallHash) || id != batchID(v.OwnerHash, v.CallHash) || !batchValidDigest(v.Hash) || !batchValidDigest(v.PlanHash) || v.PlanBytes < 1 || v.Items < 1 || v.Items > 1000000 || v.PlanBytes > 1<<30 || v.Bytes < v.PlanBytes || v.Sequence < 0 || v.Sequence > 2*v.Items+1 || v.Started < 0 || v.Started > v.Items || v.Completed < 0 || v.Unchanged < 0 || v.Unchanged > 1 || v.Unknown < 0 || v.Unknown > 1 || v.Completed+v.Unchanged+v.Unknown != v.Started || !batchCode(v.Code) || (!v.Finished && (v.Complete || v.Code != "")) || (v.Complete && v.Completed != v.Items) {
		return bad()
	}
	if v.Items > m.options.Entries || v.PlanBytes > m.options.PlanBytes {
		return v, batchError("limit")
	}
	if len(v.Plan) < 1 || len(v.Plan) > int(v.PlanBytes/(batchSegmentBytes-batchLineBytes))+1 || len(v.Events) > int((int64(v.Items)*2+1)*batchEventBytes/(batchSegmentBytes-batchEventBytes))+1 {
		return bad()
	}
	var p, e int64
	for _, s := range v.Plan {
		if s.Bytes < 1 || s.Bytes > batchSegmentBytes || !batchValidDigest(s.Hash) {
			return bad()
		}
		p += s.Bytes
	}
	for _, s := range v.Events {
		if s.Bytes < 1 || s.Bytes > batchSegmentBytes || !batchValidDigest(s.Hash) {
			return bad()
		}
		e += s.Bytes
	}
	if p != v.PlanBytes || p+e != v.Bytes || e > int64(v.Sequence)*batchEventBytes || (v.Sequence == 0 && len(v.Events) != 0) {
		return bad()
	}
	return v, nil
}

// Case folding uses Unicode SimpleFold equivalence, not ToLower alone (which
// misses aliases such as Greek sigma). The same portable policy applies on all
// hosts. Parent spelling must also match the frozen spelling exactly.
func batchFold(s string) string {
	return strings.Map(func(r rune) rune {
		small := r
		for n := unicode.SimpleFold(r); n != r; n = unicode.SimpleFold(n) {
			if n < small {
				small = n
			}
		}
		return small
	}, s)
}

type batchPlanValidator struct {
	root    Effect
	paths   map[string]BatchItem
	targets map[string]bool
}

func newBatchPlanValidator(root Effect) (*batchPlanValidator, error) {
	if root.Validate() != nil || !root.IsDirectoryCopy() {
		return nil, batchError("invalid_plan")
	}
	return &batchPlanValidator{root: root, paths: make(map[string]BatchItem), targets: make(map[string]bool)}, nil
}
func (v *batchPlanValidator) add(index int, item BatchItem) error {
	bad := func() error { return batchError("invalid_plan") }
	s, t := item.Source, item.Target
	if !ValidPath(s.Path, false) || !ValidPath(t.Path, false) || Protected(s.Path) || Protected(t.Path) || s.Kind != t.Kind || (s.Kind != "file" && s.Kind != "directory") || !strings.HasPrefix(s.Version, "entry-v1:") || !ValidVersion(s.Version) || t.Version != "" || item.Bytes < 0 || (s.Kind == "directory" && item.Bytes != 0) {
		return bad()
	}
	sf, tf := batchFold(s.Path), batchFold(t.Path)
	if _, ok := v.paths[sf]; ok || v.targets[tf] || v.targets[sf] {
		return bad()
	}
	if _, ok := v.paths[tf]; ok {
		return bad()
	}
	if index == 0 {
		if s != v.root.Source || t != v.root.Target {
			return bad()
		}
	} else {
		suffix, ok := strings.CutPrefix(s.Path, v.root.Source.Path+"/")
		if !ok || t.Path != v.root.Target.Path+"/"+suffix {
			return bad()
		}
		parent := s.Path[:strings.LastIndexByte(s.Path, '/')]
		p, ok := v.paths[batchFold(parent)]
		if !ok || p.Source.Path != parent || p.Source.Kind != "directory" {
			return bad()
		}
	}
	v.paths[sf] = item
	v.targets[tf] = true
	return nil
}
func batchValidActual(entry batchEntry, a BatchActual) bool {
	if !batchCode(a.Code) || a.Bytes < 0 {
		return false
	}
	switch a.Outcome {
	case "completed":
		if entry.File {
			return batchValidDigest(a.ContentHash) && a.Bytes == entry.Bytes
		}
		return a.ContentHash == "" && a.Bytes == 0
	case "unchanged", "unknown":
		return a.ContentHash == "" && a.Bytes == 0
	}
	return false
}
