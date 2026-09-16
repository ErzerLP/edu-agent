package knowledge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/google/uuid"
)

// SourceImport 只能创建新的原始来源，不能携带身份解析、覆盖、共享或发布指令。
type SourceImport struct {
	RunID                                                          string
	Metadata                                                       research.Source
	OperationID, SourceID, SourceRevisionID, GoalID, ActorDeviceID string
	Locator, Title, Fingerprint, Coverage, Text                    string
	FetchedAt                                                      time.Time
}

func PrepareSourceImport(c SourceImport) (PreparedCommit, error) {
	for _, id := range []string{c.RunID, c.OperationID, c.SourceID, c.SourceRevisionID, c.GoalID, c.ActorDeviceID} {
		if !validUUID(id) {
			return PreparedCommit{}, &Error{Code: CodeInvalidRequest}
		}
	}
	if c.Text == "" || len(c.Text) > 16000 || len(c.Locator) > 8192 || c.Metadata.ID != c.SourceID || c.Metadata.RevisionID != c.SourceRevisionID || c.Metadata.GoalID != c.GoalID {
		return PreparedCommit{}, &Error{Code: CodeInvalidRequest}
	}
	// 外部文本进入缩进代码块，不能注入知识身份标记或 Markdown 指令。
	metadata, _ := json.Marshal(struct {
		Provenance, SourceID, SourceRevisionID, GoalID, Locator, Title, Fingerprint, Coverage, FinalURL, Parser string
		FetchedAt                                                                                               time.Time
	}{"external_original", c.SourceID, c.SourceRevisionID, c.GoalID, c.Locator, c.Title, c.Fingerprint, c.Coverage, c.Metadata.FinalURL, c.Metadata.Parser, c.FetchedAt})
	markdown := "# 外部来源原文\n\n    " + string(metadata) + "\n\n    " + strings.ReplaceAll(c.Text, "\n", "\n    ") + "\n"
	canonicalizer := NewCanonicalizer()
	inspected, err := canonicalizer.Inspect(markdown)
	if err != nil {
		return PreparedCommit{}, err
	}
	nodes := make([]string, len(inspected.DraftNodes))
	for i := range nodes {
		nodes[i] = uuid.NewString()
	}
	document, err := canonicalizer.Materialize(inspected, uuid.NewString(), uuid.NewString(), nodes)
	if err != nil {
		return PreparedCommit{}, err
	}
	documents := []SnapshotDocument{{Path: "source.md", Revision: document}}
	raw, _ := json.Marshal(c)
	hash := sha256.Sum256(raw)
	revision := KnowledgeRevision{ID: uuid.NewString(), RevisionNo: 1, ManifestHash: hashManifest(documents), Source: "external_original:" + c.SourceRevisionID, CreatedByDeviceID: c.ActorDeviceID, CreatedAt: time.Now().UTC(), CanonicalizerVersion: CanonicalizerVersion, ParserVersion: ParserVersion, IndexerVersion: IndexerVersion, IdentityPolicyVersion: IdentityPolicyVersion, Documents: documents}
	return PreparedCommit{OperationID: c.OperationID, RequestHash: hex.EncodeToString(hash[:]), Revision: revision}, nil
}
