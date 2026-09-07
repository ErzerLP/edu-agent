package fileeffects

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// Build authenticated but not necessarily valid plan evidence, without using
// Begin. Recovery must independently prove the same complete-plan contract.
func fileBatchPurgeForgedPlan(t *testing.T, plan BatchPlan, version int) (*fileBatchTestStore, string) {
	t.Helper()
	store := newFileBatchTestStore()
	marker := batchMakeMarker("owner", "call")
	store.blobs[batchMarkerName(marker.CallHash)] = fileBatchTestJSON(t, marker)
	body := fileBatchTestLine(t, batchRootLine{Version: version, Type: "root", Root: plan.Root})
	for i, item := range plan.Items {
		body = append(body, fileBatchTestLine(t, batchPlanLine{Version: version, Type: "plan", Index: i, Item: item})...)
	}
	store.blobs[batchSegmentName(marker.ID, true, 0)] = body
	meta := batchMetadata{Version: version, ID: marker.ID, OwnerHash: marker.OwnerHash, CallHash: marker.CallHash, PlanHash: fileBatchTestHash(body), Hash: fileBatchTestHash(body), PlanBytes: int64(len(body)), Bytes: int64(len(body)), Items: len(plan.Items), Plan: []batchSegment{{Bytes: int64(len(body)), Hash: fileBatchTestHash(body)}}, Events: []batchSegment{}}
	store.blobs[batchMetaName(marker.ID)] = fileBatchTestJSON(t, meta)
	return store, marker.ID
}

func fileBatchPurgeRehash(t *testing.T, store *fileBatchTestStore, id string) {
	t.Helper()
	var meta batchMetadata
	fileBatchTestOK(t, json.Unmarshal(store.blobs[batchMetaName(id)], &meta))
	var body []byte
	for i := range meta.Plan {
		data := store.blobs[batchSegmentName(id, true, i)]
		meta.Plan[i] = batchSegment{Bytes: int64(len(data)), Hash: fileBatchTestHash(data)}
		body = append(body, data...)
	}
	meta.PlanBytes, meta.PlanHash = int64(len(body)), fileBatchTestHash(body)
	for i := range meta.Events {
		data := store.blobs[batchSegmentName(id, false, i)]
		meta.Events[i] = batchSegment{Bytes: int64(len(data)), Hash: fileBatchTestHash(data)}
		body = append(body, data...)
	}
	meta.Bytes, meta.Hash = int64(len(body)), fileBatchTestHash(body)
	store.blobs[batchMetaName(id)] = fileBatchTestJSON(t, meta)
}

func TestFileBatchPurgeRejectsInvalidPlansBeforeIOAndOnBind(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name   string
		mutate func(*BatchPlan)
	}{
		{"root-first", func(p *BatchPlan) { p.Items[0], p.Items[2] = p.Items[2], p.Items[0] }},
		{"missing-root", func(p *BatchPlan) { p.Items = p.Items[:2] }},
		{"missing-parent", func(p *BatchPlan) { p.Items = append(p.Items[:1], p.Items[2:]...) }},
		{"duplicate", func(p *BatchPlan) { p.Items = append(p.Items[:1:1], p.Items...) }},
		{"ascending-siblings", func(p *BatchPlan) {
			root := p.Root.Source.Path
			p.Items = []BatchItem{fileBatchPurgeItem(root+"/a", "file", 0), fileBatchPurgeItem(root+"/z", "file", 0), p.Items[2]}
		}},
		{"out-of-range", func(p *BatchPlan) {
			p.Items[0].Source.Path = p.Root.Source.Path + "-other/dir/file"
			p.Items[0].Target.Path = p.Items[0].Source.Path
		}},
		{"outside-archive", func(p *BatchPlan) { p.Items[0].Source.Path = "outside/file"; p.Items[0].Target.Path = "outside/file" }},
		{"root-alias", func(p *BatchPlan) {
			p.Items[0].Source.Path = ArchiveDirectory + "/Selected/dir/file"
			p.Items[0].Target.Path = p.Items[0].Source.Path
		}},
		{"parent-spelling", func(p *BatchPlan) {
			p.Items[1].Source.Path = p.Root.Source.Path + "/Dir"
			p.Items[1].Target.Path = p.Items[1].Source.Path
		}},
		{"case-alias", func(p *BatchPlan) {
			p.Items = append(p.Items, fileBatchPurgeItem(p.Root.Source.Path+"/Dir", "directory", 0))
			sort.Slice(p.Items, func(i, j int) bool { return p.Items[i].Source.Path > p.Items[j].Source.Path })
		}},
		{"unicode-fold-alias", func(p *BatchPlan) {
			root := p.Root.Source.Path
			p.Items = []BatchItem{fileBatchPurgeItem(root+"/σ", "file", 0), fileBatchPurgeItem(root+"/ς", "file", 0), p.Items[2]}
		}},
		{"file-ancestor", func(p *BatchPlan) { p.Items[1].Source.Kind = "file" }},
		{"link-ancestor", func(p *BatchPlan) { p.Items[1].Source.Kind = "link" }},
		{"file-root-children", func(p *BatchPlan) {
			p.Root.Source.Kind = "file"
			p.Root.Scope = "entry"
			p.Items[2].Source = p.Root.Source
		}},
		{"link-root-children", func(p *BatchPlan) {
			p.Root.Source.Kind = "link"
			p.Root.Scope = "entry"
			p.Items[2].Source = p.Root.Source
		}},
		{"root-item-version", func(p *BatchPlan) { p.Items[2].Source.Version = "entry-v1:" + strings.Repeat("b", 64) }},
		{"root-item-kind", func(p *BatchPlan) { p.Items[2].Source.Kind = "link" }},
		{"root-version", func(p *BatchPlan) { p.Root.Source.Version = "sha256:" + strings.Repeat("a", 64) }},
		{"root-effect-version", func(p *BatchPlan) { p.Root.SchemaVersion = 3 }},
		{"root-scope", func(p *BatchPlan) { p.Root.Scope = "entry" }},
		{"root-chain", func(p *BatchPlan) { p.Root.Directories.Count = 1 }},
		{"root-target-kind", func(p *BatchPlan) { p.Root.Target.Kind = "directory" }},
		{"root-target-path", func(p *BatchPlan) { p.Root.Target.Path += "/other" }},
		{"root-target-version", func(p *BatchPlan) { p.Root.Target.Version = p.Root.Source.Version }},
		{"archive-root", func(p *BatchPlan) { p.Root.Source.Path = ArchiveDirectory; p.Root.Target.Path = ArchiveDirectory }},
		{"archive-alias", func(p *BatchPlan) {
			p.Root.Source.Path = ".EDU-AGENT-ARCHIVE/selected"
			p.Root.Target.Path = p.Root.Source.Path
		}},
		{"item-missing-version", func(p *BatchPlan) { p.Items[0].Source.Version = "" }},
		{"item-hash-version", func(p *BatchPlan) { p.Items[0].Source.Version = "sha256:" + strings.Repeat("a", 64) }},
		{"item-short-version", func(p *BatchPlan) { p.Items[0].Source.Version = "entry-v1:aa" }},
		{"item-uppercase-version", func(p *BatchPlan) { p.Items[0].Source.Version = "entry-v1:" + strings.Repeat("A", 64) }},
		{"item-target-kind", func(p *BatchPlan) { p.Items[0].Target.Kind = "file" }},
		{"item-target-path", func(p *BatchPlan) { p.Items[0].Target.Path += "-other" }},
		{"item-target-version", func(p *BatchPlan) { p.Items[0].Target.Version = p.Items[0].Source.Version }},
		{"item-special", func(p *BatchPlan) { p.Items[0].Source.Kind = "fifo" }},
		{"item-path-cleaning", func(p *BatchPlan) {
			p.Items[0].Source.Path = p.Root.Source.Path + "/dir/../file"
			p.Items[0].Target.Path = p.Items[0].Source.Path
		}},
		{"negative-file-bytes", func(p *BatchPlan) { p.Items[0].Bytes = -1 }},
		{"directory-bytes", func(p *BatchPlan) { p.Items[1].Bytes = 1 }},
		{"link-bytes", func(p *BatchPlan) { p.Items[0].Source.Kind = "link"; p.Items[0].Bytes = 1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan := fileBatchPurgeSingle("directory")
			root := plan.Root.Source.Path
			plan.Items = []BatchItem{fileBatchPurgeItem(root+"/dir/file", "file", 5), fileBatchPurgeItem(root+"/dir", "directory", 0), plan.Items[0]}
			test.mutate(&plan)
			store := newFileBatchTestStore()
			m := NewBatchManager(BatchOptions{})
			fileBatchTestOK(t, m.Bind(ctx, "owner", store))
			io, memory := store.ioCounts(), m.memory
			id, err := m.Begin(ctx, "owner", "call", plan)
			fileBatchTestCode(t, err, "invalid_plan")
			if id != "" || m.HasCall("owner", "call") || store.ioCounts() != io || m.memory != memory || m.count != 0 {
				t.Fatal("invalid plan consumed identity, memory, or backend I/O")
			}
			forged, _ := fileBatchPurgeForgedPlan(t, plan, 2)
			fileBatchTestCode(t, m.Bind(ctx, "owner", forged), "corrupt")
			if m.memory != memory || m.count != 0 || forged.ioCounts()[1] != 0 {
				t.Fatal("invalid restoration retained state or wrote evidence")
			}
		})
	}
}

func TestFileBatchPurgeStrictVersionsAndShapes(t *testing.T) {
	ctx := context.Background()
	store := newFileBatchTestStore()
	m := NewBatchManager(BatchOptions{})
	fileBatchTestOK(t, m.Bind(ctx, "owner", store))
	plan := fileBatchPurgeSingle("file")
	id := fileBatchTestBegin(t, m, "owner", "call", plan)
	fileBatchPurgeFinish(t, m, "owner", id, plan)
	fileBatchTestOK(t, m.Bind(ctx, "owner", store))
	body, info := fileBatchTestReadAll(t, m, "owner", id)
	status := fileBatchTestStatus(t, m, "owner", id)
	memory := m.memory
	marker := batchMakeMarker("owner", "call")
	mutateLine := func(s *fileBatchTestStore, plan bool, index int, f func([]byte) []byte) {
		name := batchSegmentName(id, plan, 0)
		lines := bytes.Split(bytes.TrimSuffix(s.blobs[name], []byte{'\n'}), []byte{'\n'})
		lines[index] = f(lines[index])
		s.blobs[name] = append(bytes.Join(lines, []byte{'\n'}), '\n')
		fileBatchPurgeRehash(t, s, id)
	}
	setVersion := func(version string) func([]byte) []byte {
		return func(line []byte) []byte {
			return bytes.Replace(line, []byte(`"version":2`), []byte(`"version":`+version), 1)
		}
	}
	type mutation struct {
		name, code string
		mutate     func(*fileBatchTestStore)
	}
	tests := []mutation{
		{"marker-v2", "version_unsupported", func(s *fileBatchTestStore) {
			v := marker
			v.Version = 2
			s.blobs[batchMarkerName(marker.CallHash)] = fileBatchTestJSON(t, v)
		}},
		{"meta-v3", "version_unsupported", func(s *fileBatchTestStore) { s.blobs[batchMetaName(id)] = setVersion("3")(s.blobs[batchMetaName(id)]) }},
		{"meta-v1-mixed", "corrupt", func(s *fileBatchTestStore) { s.blobs[batchMetaName(id)] = setVersion("1")(s.blobs[batchMetaName(id)]) }},
		{"v1-purge-root", "corrupt", func(s *fileBatchTestStore) {
			for name, data := range s.blobs {
				if name != batchMarkerName(marker.CallHash) {
					s.blobs[name] = bytes.ReplaceAll(data, []byte(`"version":2`), []byte(`"version":1`))
				}
			}
			fileBatchPurgeRehash(t, s, id)
		}},
		{"v2-copy-root", "corrupt", func(s *fileBatchTestStore) {
			mutateLine(s, true, 0, func(_ []byte) []byte {
				return fileBatchTestJSON(t, batchRootLine{Version: 2, Type: "root", Root: fileBatchTestPlan(0).Root})
			})
		}},
		{"effect-future", "corrupt", func(s *fileBatchTestStore) {
			mutateLine(s, true, 0, func(line []byte) []byte {
				return bytes.Replace(line, []byte(`"schema_version":4`), []byte(`"schema_version":5`), 1)
			})
		}},
		{"false-actual-hash", "corrupt", func(s *fileBatchTestStore) {
			mutateLine(s, false, 1, func(line []byte) []byte {
				return bytes.Replace(line, []byte(`"content_hash":""`), []byte(`"content_hash":"`+fileBatchTestHash([]byte("fake"))+`"`), 1)
			})
		}},
		{"false-actual-size", "corrupt", func(s *fileBatchTestStore) {
			mutateLine(s, false, 1, func(line []byte) []byte {
				return bytes.Replace(line, []byte(`"bytes":1099511627776`), []byte(`"bytes":0`), 1)
			})
		}},
		{"future-type", "version_unsupported", func(s *fileBatchTestStore) {
			mutateLine(s, false, 0, func(line []byte) []byte {
				return bytes.Replace(setVersion("3")(line), []byte(`"type":"pending"`), []byte(`"type":"future"`), 1)
			})
		}},
		{"unknown-key", "corrupt", func(s *fileBatchTestStore) {
			mutateLine(s, true, 1, func(line []byte) []byte { return append([]byte(`{"command":"never executable",`), line[1:]...) })
		}},
		{"duplicate-key", "corrupt", func(s *fileBatchTestStore) {
			mutateLine(s, true, 1, func(line []byte) []byte { return append([]byte(`{"version":2,`), line[1:]...) })
		}},
		{"null-item", "corrupt", func(s *fileBatchTestStore) {
			mutateLine(s, true, 1, func(_ []byte) []byte { return []byte(`{"version":2,"type":"plan","index":0,"item":null}`) })
		}},
		{"missing-size", "corrupt", func(s *fileBatchTestStore) {
			mutateLine(s, true, 1, func(line []byte) []byte { return bytes.Replace(line, []byte(`,"bytes":1099511627776`), nil, 1) })
		}},
		{"field-case", "corrupt", func(s *fileBatchTestStore) {
			mutateLine(s, true, 1, func(line []byte) []byte { return bytes.Replace(line, []byte(`"source"`), []byte(`"Source"`), 1) })
		}},
		{"missing-plan", "unavailable", func(s *fileBatchTestStore) { delete(s.blobs, batchSegmentName(id, true, 0)) }},
	}
	for _, line := range []struct {
		name  string
		plan  bool
		index int
	}{{"root", true, 0}, {"plan", true, 1}, {"pending", false, 0}, {"actual", false, 1}, {"end", false, 2}} {
		for _, version := range []struct{ value, code string }{{"0", "corrupt"}, {"1", "corrupt"}, {"3", "version_unsupported"}} {
			tests = append(tests, mutation{line.name + "-v" + version.value, version.code, func(s *fileBatchTestStore) { mutateLine(s, line.plan, line.index, setVersion(version.value)) }})
		}
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bad := store.clone()
			test.mutate(bad)
			fileBatchTestCode(t, m.Bind(ctx, "owner", bad), test.code)
			got, gotInfo := fileBatchTestReadAll(t, m, "owner", id)
			if !bytes.Equal(got, body) || gotInfo != info || fileBatchTestStatus(t, m, "owner", id) != status || m.memory != memory || m.count != 1 || bad.ioCounts()[1] != 0 || !m.HasCall("owner", "call") {
				t.Fatal("failed Bind changed old binding, state, budgets or backend")
			}
		})
	}
	// Both versions retain the exact DTO shape contract, including future-token
	// validation. Copy cannot opt into v2 even when every line agrees with meta.
	copyV2, _ := fileBatchPurgeForgedPlan(t, fileBatchTestPlan(1), 2)
	fileBatchTestCode(t, NewBatchManager(BatchOptions{}).Bind(ctx, "owner", copyV2), "corrupt")
	for _, version := range []int{1, 2} {
		line := fileBatchTestJSON(t, batchActualLine{Version: version, Type: "actual", Sequence: 2, Index: 0, Actual: BatchActual{Outcome: "unknown"}})
		for _, malformed := range [][]byte{append([]byte(`{"extra":1,`), line[1:]...), append([]byte(`{"type":"actual",`), line[1:]...), bytes.Replace(line, []byte(`"actual":{`), []byte(`"Actual":{`), 1), bytes.Replace(line, []byte(`"content_hash":""`), []byte(`"content_hash":null`), 1)} {
			var actual batchActualLine
			fileBatchTestCode(t, batchDecode(malformed, &actual), "corrupt")
		}
	}
}
