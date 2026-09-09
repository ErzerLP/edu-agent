package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/go-chi/chi/v5"
)

type importJobService interface {
	ImportJobs(context.Context, string, string) (knowledge.ImportJobPage, error)
	RunImportJob(context.Context, string, knowledge.ImportJobCommand) (knowledge.ImportJob, error)
}

// 列表与详情只传状态和计数，正文差异按单批读取，避免任务大小突破响应预算。
func importJobView(j knowledge.ImportJob) knowledge.ImportJob {
	if len(j.Batches) > 0 {
		j.BatchCount = len(j.Batches)
	}
	j.Batches = append([]knowledge.ImportJobBatch{}, j.Batches...)
	for i := range j.Batches {
		if p := j.Batches[i].Preview; p != nil {
			q := *p
			q.Diff = []knowledge.DocumentDiff{}
			q.Before = nil
			q.Review = nil
			q.Receipt = ""
			j.Batches[i].Preview = &q
		}
	}
	return j
}
func (a *API) mountImportJobs(r chi.Router) {
	s, ok := a.knowledge.(importJobService)
	if !ok {
		return
	}
	failure := func(w http.ResponseWriter, r *http.Request, err error) {
		var domain *knowledge.Error
		if errors.As(err, &domain) && strings.HasPrefix(domain.Code, "import_job") {
			status := 409
			if domain.Code == "import_job_quota" {
				status = 413
			}
			if domain.Code == "import_jobs_unavailable" || domain.Code == "import_job_storage_unavailable" {
				status = 503
			}
			writeError(w, r, status, domain.Code, "导入任务未完成，请查询原任务状态")
			return
		}
		a.writeKnowledgeFailure(w, r, "import_job", err)
	}
	read := r.With(a.requireScope("knowledge:read"), a.responseReadPermit("content_redacted", privacy.OwnerKnowledge))
	read.Get("/v1/knowledge/import-jobs", func(w http.ResponseWriter, r *http.Request) {
		actor, _ := credentialFromContext(r.Context())
		page, err := s.ImportJobs(r.Context(), actor.Device.ID, r.URL.Query().Get("cursor"))
		if err != nil {
			failure(w, r, err)
			return
		}
		for i := range page.Items {
			page.Items[i] = importJobView(page.Items[i])
			page.Items[i].Batches = []knowledge.ImportJobBatch{}
		}
		writeJSON(w, 200, page)
	})
	read.Get("/v1/knowledge/import-jobs/{jobID}", func(w http.ResponseWriter, r *http.Request) {
		actor, _ := credentialFromContext(r.Context())
		j, err := s.RunImportJob(r.Context(), actor.Device.ID, knowledge.ImportJobCommand{ID: chi.URLParam(r, "jobID"), Action: "get"})
		if err != nil {
			failure(w, r, err)
			return
		}
		if raw := r.URL.Query().Get("batch"); raw != "" {
			index, e := strconv.Atoi(raw)
			if e != nil || index < 0 || index >= len(j.Batches) {
				importInputFailure(w, r, errors.New("批次无效"))
				return
			}
			writeJSON(w, 200, j.Batches[index])
			return
		}
		writeJSON(w, 200, importJobView(j))
	})
	r.With(a.requireScope("knowledge:write"), a.requireScope("knowledge:read"), a.responseReadPermit("content_redacted", privacy.OwnerKnowledge, privacy.OwnerLearning)).Post("/v1/knowledge/import-jobs", func(w http.ResponseWriter, r *http.Request) {
		var c knowledge.ImportJobCommand
		if err := decodeJSON(w, r, a.maxKnowledgeRequestBody, &c); err != nil {
			importInputFailure(w, r, err)
			return
		}
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			actor, _ := credentialFromContext(r.Context())
			j, err := s.RunImportJob(r.Context(), actor.Device.ID, c)
			if err != nil {
				failure(w, r, err)
				return
			}
			writeJSON(w, 200, importJobView(j))
		})
		if c.Action == "confirm" || c.Action == "continue" {
			a.requireScope("knowledge:approve")(handler).ServeHTTP(w, r)
		} else {
			handler.ServeHTTP(w, r)
		}
	})
	r.With(a.requireScope("knowledge:write"), a.responseReadPermit("content_redacted", privacy.OwnerKnowledge)).Delete("/v1/knowledge/import-jobs/{jobID}", func(w http.ResponseWriter, r *http.Request) {
		actor, _ := credentialFromContext(r.Context())
		j, err := s.RunImportJob(r.Context(), actor.Device.ID, knowledge.ImportJobCommand{ID: chi.URLParam(r, "jobID"), Action: "cancel"})
		if err != nil {
			failure(w, r, err)
			return
		}
		writeJSON(w, 200, importJobView(j))
	})
}
