package mentorrun

import (
	"context"
	"strconv"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
)

func (s *Service) StudioUnits(ctx context.Context, actor identity.Credential, id, cursor string, revision int64, limit int) (learningcontent.StudioUnitPage, error) {
	after := 0
	var err error
	if cursor != "" {
		after, err = strconv.Atoi(cursor)
	}
	if !validID(id) || err != nil || after < 0 || limit < 1 || limit > 32 || revision < 0 {
		return learningcontent.StudioUnitPage{}, ErrInvalid
	}
	tx, g, err := s.studioJobBegin(ctx, actor, false)
	if err != nil {
		return learningcontent.StudioUnitPage{}, err
	}
	defer tx.Rollback(context.Background())
	j, err := s.studio.StudioJobTx(ctx, tx, id, g, false)
	if err != nil {
		return learningcontent.StudioUnitPage{}, err
	}
	if err = s.studioJobSourcesTx(ctx, tx, j); err != nil {
		return learningcontent.StudioUnitPage{}, err
	}
	if revision > 0 {
		if revision > j.ActivePlanRevision {
			return learningcontent.StudioUnitPage{}, ErrInvalid
		}
		j.ConfirmedPlanRevision = revision
	}
	page, err := s.studio.StudioUnitsTx(ctx, tx, j, after, limit)
	if err != nil {
		return page, err
	}
	for i := range page.Items {
		u := &page.Items[i]
		if u.Body != nil {
			if err = s.studio.VerifyStudioUnitTx(ctx, tx, actor.TokenID, *u); err != nil {
				return learningcontent.StudioUnitPage{}, err
			}
		}
	}
	return page, tx.Commit(ctx)
}
