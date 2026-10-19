package mentorrun

import (
	"context"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgepg "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
)

func (h *executionHost) studioLegacyCoverage(ctx context.Context, sources []knowledge.SourceDetail) (bool, error) {
	ctx, err := learningspace.WithScope(ctx, h.owned.SpaceID)
	if err != nil {
		return false, err
	}
	tx, err := h.service.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.Background())
	ranges := map[string]knowledge.SourceRange{}
	for _, source := range sources {
		for _, fragment := range source.Fragments {
			ranges[fragment.NodeRevisionID] = fragment.Range
		}
	}
	cursor := ""
	for {
		page, e := knowledgepg.New(h.service.pool).ReadSourceDirectoryTx(ctx, tx, *h.body.SourceManifest, cursor, 64)
		if e != nil {
			return false, e
		}
		for _, node := range page.Items {
			if r, ok := ranges[node.NodeRevisionID]; !ok || r != node.Range {
				return false, nil
			}
		}
		if page.NextCursor == "" {
			return true, nil
		}
		cursor = page.NextCursor
	}
}
