// Package fileeffects describes bounded filesystem facts, not executable transactions.
package fileeffects

import (
	"encoding/hex"
	"errors"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"
)

const ArchiveDirectory = ".edu-agent-archive"

// Version is optional and tagged: entry-v1 is metadata; sha256 is raw bytes.
// Empty legacy versions stay empty. No identities, absolute roots or bodies.
type Endpoint struct {
	Path    string `json:"path"`
	Kind    string `json:"kind"`
	Version string `json:"version,omitempty"`
}

// DirectoryChain compactly describes every prefix after Anchor up to Target.
// Created is the known-created prefix length, NOT an inference from the plan.
// A crash with only WAL has Created=0: any planned prefix may have been created.
type DirectoryChain struct {
	Anchor  string `json:"anchor,omitempty"`
	Count   int    `json:"count"`
	Created int    `json:"created"`
}

type Effect struct {
	SchemaVersion int            `json:"schema_version"`
	Operation     string         `json:"operation"`
	Source        Endpoint       `json:"source"`
	Target        Endpoint       `json:"target"`
	Scope         string         `json:"scope"` // entry or subtree; locations derive from endpoints/chain
	Directories   DirectoryChain `json:"directories"`
}

func New(operation, source, target, kind string) Effect {
	e := Effect{SchemaVersion: 1, Operation: operation, Target: Endpoint{Path: target, Kind: kind}, Scope: "entry"}
	if source != "" {
		e.Source = Endpoint{Path: source, Kind: kind}
	}
	if kind == "directory" {
		e.Scope = "subtree"
	}
	return e
}

func (e Effect) ReferencePath() string {
	if e.Source.Path != "" && e.Operation != "copy" {
		return e.Source.Path
	}
	return e.Target.Path
}
func (e Effect) ReferenceKind() string {
	if e.Operation == "move" {
		return "move_" + e.Source.Kind
	}
	if e.Operation == "archive" {
		return "archive_" + e.Source.Kind
	}
	if e.Operation == "copy" {
		return "copy"
	}
	if e.Operation == "mkdir" {
		return "mkdir"
	}
	return "file"
}
func (e Effect) PlannedPaths() []string {
	if e.Directories.Count == 0 {
		return nil
	}
	parts := strings.Split(e.Target.Path, "/")
	start := len(parts) - e.Directories.Count
	if start < 0 || e.Directories.Count > 64 {
		return nil
	}
	result := make([]string, 0, e.Directories.Count)
	for n := start + 1; n <= len(parts); n++ {
		result = append(result, strings.Join(parts[:n], "/"))
	}
	return result
}
func (e Effect) CreatedPaths() []string {
	all := e.PlannedPaths()
	if e.Directories.Created < 0 || e.Directories.Created > len(all) {
		return nil
	}
	return all[:e.Directories.Created]
}
func (e Effect) SamePlan(other Effect) bool {
	e.Directories.Created = 0
	other.Directories.Created = 0
	// Published target versions may be learned only after an operation.
	e.Target.Version = ""
	other.Target.Version = ""
	return e == other
}

func ValidPath(s string, root bool) bool {
	if root && s == "." {
		return true
	}
	if s == "" || len(s) > 4096 || !utf8.ValidString(s) || strings.ContainsAny(s, "\\\n\t<>:\"|?*") {
		return false
	}
	parts := strings.Split(s, "/")
	if len(parts) > 64 {
		return false
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || strings.TrimRight(p, " .") != p {
			return false
		}
		for _, r := range p {
			if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
				return false
			}
		}
	}
	return true
}
func Protected(s string) bool {
	return strings.EqualFold(strings.SplitN(s, "/", 2)[0], ArchiveDirectory)
}
func ValidVersion(s string) bool {
	if s == "" {
		return true
	}
	value, ok := strings.CutPrefix(s, "entry-v1:")
	if !ok {
		value, ok = strings.CutPrefix(s, "sha256:")
	}
	if !ok || len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
func (e Effect) Validate() error {
	invalid := errors.New("invalid file effect")
	if e.SchemaVersion != 1 || !ValidPath(e.Target.Path, false) || !ValidVersion(e.Target.Version) || (e.Target.Kind != "file" && e.Target.Kind != "directory") {
		return invalid
	}
	if e.Scope != "entry" && e.Scope != "subtree" {
		return invalid
	}
	switch e.Operation {
	case "move":
		if !ValidPath(e.Source.Path, false) || Protected(e.Source.Path) || Protected(e.Target.Path) || strings.EqualFold(e.Source.Path, e.Target.Path) || e.Source.Kind != e.Target.Kind || !strings.HasPrefix(e.Source.Version, "entry-v1:") || !ValidVersion(e.Source.Version) || e.Target.Version != "" || e.Directories != (DirectoryChain{}) {
			return invalid
		}
		if e.Target.Kind == "directory" && (e.Scope != "subtree" || strings.HasPrefix(e.Target.Path, e.Source.Path+"/")) || e.Target.Kind == "file" && e.Scope != "entry" {
			return invalid
		}
	case "copy":
		if !ValidPath(e.Source.Path, false) || Protected(e.Source.Path) || Protected(e.Target.Path) || e.Source.Path == e.Target.Path || e.Source.Kind != "file" || e.Target.Kind != "file" || e.Scope != "entry" || !strings.HasPrefix(e.Source.Version, "entry-v1:") || !ValidVersion(e.Source.Version) || strings.HasPrefix(e.Target.Version, "entry-v1:") || e.Directories != (DirectoryChain{}) {
			return invalid
		}
	case "mkdir":
		if e.Source != (Endpoint{}) || e.Target.Kind != "directory" || e.Scope != "subtree" || Protected(e.Target.Path) || e.Target.Version != "" {
			return invalid
		}
		d := e.Directories
		if !ValidPath(d.Anchor, true) || d.Count < 1 || d.Count > 64 || d.Created < 0 || d.Created > d.Count {
			return invalid
		}
		parts := strings.Split(e.Target.Path, "/")
		depth := len(parts) - d.Count
		if depth < 0 {
			return invalid
		}
		anchor := "."
		if depth > 0 {
			anchor = strings.Join(parts[:depth], "/")
		}
		if anchor != d.Anchor {
			return invalid
		}
	case "archive":
		if !ValidPath(e.Source.Path, false) || Protected(e.Source.Path) || e.Source.Kind != e.Target.Kind || !ValidVersion(e.Source.Version) || strings.HasPrefix(e.Source.Version, "sha256:") || e.Target.Version != "" || e.Directories != (DirectoryChain{}) {
			return invalid
		}
		parts := strings.SplitN(e.Target.Path, "/", 3)
		if len(parts) != 3 || parts[0] != ArchiveDirectory || parts[2] != e.Source.Path {
			return invalid
		}
		stamp, id, ok := strings.Cut(parts[1], "-")
		if !ok || stamp == "" || id == "" {
			return invalid
		}
		for _, r := range parts[1] {
			if r != '-' && r != '_' && r != '.' && !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') {
				return invalid
			}
		}
		if e.Target.Kind == "directory" && e.Scope != "subtree" || e.Target.Kind == "file" && e.Scope != "entry" {
			return invalid
		}
	case "write_create", "write_replace", "edit":
		if e.Source != (Endpoint{}) || e.Target.Kind != "file" || e.Scope != "entry" || e.Directories != (DirectoryChain{}) || strings.HasPrefix(e.Target.Version, "entry-v1:") {
			return invalid
		}
	default:
		return invalid
	}
	return nil
}

// Affects invalidates observations, never historical operation receipts.
func (e Effect) Affects(p, kind string) bool {
	switch kind {
	case "file", "entry_metadata", "directory_listing", "find_result", "search_result":
	default:
		return false
	}
	locations := []string{e.Target.Path}
	if e.Source.Path != "" && e.Operation != "copy" {
		locations = append(locations, e.Source.Path)
	}
	if plan := e.PlannedPaths(); len(plan) > 0 {
		locations = []string{plan[0]}
	}
	for _, location := range locations {
		observed := p
		if runtime.GOOS == "windows" {
			location = strings.ToLower(location)
			observed = strings.ToLower(observed)
		}
		inside := observed == location || e.Scope == "subtree" && strings.HasPrefix(observed, location+"/")
		ancestor := observed == "." || strings.HasPrefix(location, observed+"/")
		switch kind {
		case "file":
			if inside {
				return true
			}
		case "directory_listing":
			if inside || ancestor {
				return true
			}
		default:
			if inside || ancestor {
				return true
			}
		}
	}
	return false
}
