package mcp

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestPinnedOfficialSDKVersion(t *testing.T) {
	data, err := os.ReadFile("../../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "github.com/modelcontextprotocol/go-sdk v1.7.0") {
		t.Fatalf("official MCP SDK v1.7.0 is not pinned:\n%s", data)
	}
}

func TestCatalogIsUniqueExactAndMatchesOpenAPIScopes(t *testing.T) {
	data, err := os.ReadFile("../../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	paths, ok := document["paths"].(map[string]any)
	if !ok {
		t.Fatal("OpenAPI paths are missing")
	}
	scopesByOperation := map[string]string{}
	for _, rawPath := range paths {
		path, ok := rawPath.(map[string]any)
		if !ok {
			continue
		}
		for _, method := range []string{"get", "post", "put", "patch", "delete"} {
			operation, ok := path[method].(map[string]any)
			if !ok {
				continue
			}
			operationID, _ := operation["operationId"].(string)
			scope, _ := operation["x-required-scope"].(string)
			if operationID != "" {
				scopesByOperation[operationID] = scope
			}
		}
	}

	seenPublic := map[string]bool{}
	seenAudit := map[string]bool{}
	toolCount, resourceCount, templateCount := 0, 0, 0
	for _, descriptor := range Catalog() {
		public := descriptor.Name
		switch descriptor.Kind {
		case DescriptorTool:
			toolCount++
		case DescriptorResource:
			resourceCount++
			public = descriptor.URI
		case DescriptorResourceTemplate:
			templateCount++
			public = descriptor.URITemplate
		default:
			t.Fatalf("descriptor %s has invalid kind %q", descriptor.Name, descriptor.Kind)
		}
		if seenPublic[public] || seenAudit[descriptor.AuditName] {
			t.Fatalf("duplicate descriptor public=%q audit=%q", public, descriptor.AuditName)
		}
		seenPublic[public], seenAudit[descriptor.AuditName] = true, true
		if descriptor.RequiredScope == "" || len(descriptor.PrivacyOwners) == 0 || descriptor.OutputLimit <= 0 || descriptor.HTTPOperationID == "" {
			t.Fatalf("incomplete descriptor: %+v", descriptor)
		}
		if descriptor.Kind == DescriptorTool && (descriptor.InputLimit <= 0 || descriptor.InputSchema == nil) {
			t.Fatalf("tool limits/schema missing: %+v", descriptor)
		}
		if descriptor.Kind == DescriptorTool && (strings.HasPrefix(descriptor.Name, "knowledge.maintenance.") || strings.HasPrefix(descriptor.Name, "learning.evidence_carryover.")) && descriptor.OutputSchema == nil {
			t.Fatalf("maintenance output schema missing: %+v", descriptor)
		}
		if scopesByOperation[descriptor.HTTPOperationID] != descriptor.RequiredScope {
			t.Fatalf("descriptor %s scope=%q OpenAPI %s scope=%q", descriptor.Name, descriptor.RequiredScope, descriptor.HTTPOperationID, scopesByOperation[descriptor.HTTPOperationID])
		}
	}
	if toolCount != 15 || resourceCount != 4 || templateCount != 5 {
		t.Fatalf("catalog counts tools=%d resources=%d templates=%d", toolCount, resourceCount, templateCount)
	}

	for _, forbidden := range []string{
		"knowledge.import", "knowledge.maintenance.rollback", "knowledge.maintenance.approve", "knowledge.maintenance.reject",
		"knowledge.maintenance.finalize", "knowledge.maintenance.adjudicate",
		"learning.evidence_carryover.approve", "learning.evidence_carryover.reject", "learning.evidence_carryover.decision",
		"learning.evidence_carryover.finalize", "learning.evidence_carryover.rollback",
		"assessment.confirm", "assessment.override", "assessment.invalidate",
		"memory.create_candidate", "memory.admit", "memory.delete", "memory.replay",
		"privacy.erase", "device.revoke", "notesync", "offline", "nocturne",
	} {
		if seenPublic[forbidden] {
			t.Fatalf("forbidden descriptor %q is registered", forbidden)
		}
	}
}

func TestMCPProductionPackageDoesNotImportAdaptersOrRegisterOutsideCatalogLoop(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	registrations := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(".", entry.Name())
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imported := range file.Imports {
			value, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(value, "postgresstore") || strings.Contains(value, "integrations/nocturne") || strings.Contains(value, "pgx") {
				t.Fatalf("MCP transport imports adapter %q in %s", value, entry.Name())
			}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		registrations += strings.Count(string(data), ".AddTool(")
		registrations += strings.Count(string(data), ".AddResource(")
		registrations += strings.Count(string(data), ".AddResourceTemplate(")
	}
	if registrations != 3 {
		t.Fatalf("SDK registrations=%d; registration must remain in the three catalog loop branches", registrations)
	}
}
