package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/mentorrun"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/go-chi/chi/v5"
)

// 快照读取在分页/搜索之前检查设备、学习区和隐私；发送保持同一响应屏障。
func (a *API) researchRead(w http.ResponseWriter, r *http.Request, send func([]research.Source) error) {
	actor, _ := credentialFromContext(r.Context())
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
	started := false
	err := a.mentorRuns.ReadSources(r.Context(), actor, learningspace.Scope(r.Context()), chi.URLParam(r, "runID"), func(sources []research.Source) error {
		if err := send(sources); err != nil {
			return err
		}
		started = true
		return http.NewResponseController(w).Flush()
	})
	if err != nil && !started {
		mentorFailure(w, r, err)
	}
}

func (a *API) researchSources(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := research.MaxSources
	offset := 0
	var err error
	for key, values := range q {
		if len(values) != 1 || key != "limit" && key != "cursor" && key != "q" {
			mentorFailure(w, r, mentorrun.ErrInvalid)
			return
		}
	}
	if q.Has("limit") {
		limit, err = strconv.Atoi(q.Get("limit"))
		if err != nil || limit < 1 || limit > research.MaxSources {
			mentorFailure(w, r, mentorrun.ErrInvalid)
			return
		}
	}
	if q.Has("cursor") {
		offset, err = strconv.Atoi(q.Get("cursor"))
		if err != nil || offset < 0 || offset > research.MaxSources {
			mentorFailure(w, r, mentorrun.ErrInvalid)
			return
		}
	}
	if len(q.Get("q")) > 300 {
		mentorFailure(w, r, mentorrun.ErrInvalid)
		return
	}
	a.researchRead(w, r, func(sources []research.Source) error {
		filtered := []research.Source{}
		for _, source := range sources {
			if strings.Contains(strings.ToLower(source.Title), strings.ToLower(q.Get("q"))) {
				source.Text = ""
				source.Fragments = []research.Fragment{}
				filtered = append(filtered, source)
			}
		}
		offset = min(offset, len(filtered))
		end := min(offset+limit, len(filtered))
		next := ""
		if end < len(filtered) {
			next = strconv.Itoa(end)
		}
		writeJSON(w, 200, map[string]any{"items": filtered[offset:end], "next_cursor": next})
		return nil
	})
}

func (a *API) researchSource(w http.ResponseWriter, r *http.Request) {
	a.researchRead(w, r, func(sources []research.Source) error {
		for _, source := range sources {
			if source.ID == chi.URLParam(r, "sourceID") {
				writeJSON(w, 200, source)
				return nil
			}
		}
		return mentorrun.ErrNotFound
	})
}

func (a *API) researchDecision(w http.ResponseWriter, r *http.Request) {
	var command mentorrun.SourceDecision
	if decodeJSON(w, r, 2048, &command) != nil {
		mentorFailure(w, r, mentorrun.ErrInvalid)
		return
	}
	actor, _ := credentialFromContext(r.Context())
	receipt, err := a.mentorRuns.DecideSource(r.Context(), actor, learningspace.Scope(r.Context()), chi.URLParam(r, "runID"), chi.URLParam(r, "sourceID"), command)
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	writeJSON(w, 202, receipt)
}
