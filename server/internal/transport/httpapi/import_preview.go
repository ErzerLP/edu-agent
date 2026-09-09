package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/go-chi/chi/v5"
)

type importPreviewService interface {
	PreviewImport(context.Context, knowledge.ImportCommand) (knowledge.ImportPreview, error)
	ConfirmImport(context.Context, knowledge.ConfirmImportCommand) (knowledge.ImportResult, error)
	ImportOperation(context.Context, string, string) (knowledge.ImportResult, error)
}

func (a *API) mountImportPreview(r chi.Router) {
	a.mountImportJobs(r)
	s, ok := a.knowledge.(importPreviewService)
	if !ok {
		return
	}
	r.With(a.requireScope("knowledge:write"), a.requireScope("knowledge:read"), a.responseReadPermit("content_redacted", privacy.OwnerKnowledge, privacy.OwnerLearning)).Post("/v1/knowledge/imports/previews", func(w http.ResponseWriter, r *http.Request) {
		var c knowledge.ImportCommand
		if err := decodeJSON(w, r, a.maxKnowledgeRequestBody, &c); err != nil {
			importInputFailure(w, r, err)
			return
		}
		if !c.ExpectedParentProvided {
			writeError(w, r, 400, knowledge.CodeInvalidRequest, "必须明确提供父版本")
			return
		}
		actor, _ := credentialFromContext(r.Context())
		c.ActorDeviceID = actor.Device.ID
		result, err := s.PreviewImport(r.Context(), c)
		if err != nil {
			a.writeKnowledgeFailure(w, r, "import_preview", err)
			return
		}
		writeJSON(w, 200, result)
	})
	r.With(a.requireScope("knowledge:write"), a.requireScope("knowledge:approve"), a.responseReadPermit("content_redacted", privacy.OwnerKnowledge)).Post("/v1/knowledge/imports/confirm", func(w http.ResponseWriter, r *http.Request) {
		var c knowledge.ConfirmImportCommand
		if err := decodeJSON(w, r, a.maxKnowledgeRequestBody, &c); err != nil {
			importInputFailure(w, r, err)
			return
		}
		if !c.Request.ExpectedParentProvided {
			writeError(w, r, 400, knowledge.CodeInvalidRequest, "必须明确提供父版本")
			return
		}
		actor, _ := credentialFromContext(r.Context())
		c.Request.ActorDeviceID = actor.Device.ID
		result, err := s.ConfirmImport(r.Context(), c)
		if err != nil {
			a.writeKnowledgeFailure(w, r, "import_confirm", err)
			return
		}
		writeJSON(w, 200, minimalImportResult(result))
	})
	r.With(a.requireScope("knowledge:read"), a.responseReadPermit("content_redacted", privacy.OwnerKnowledge)).Get("/v1/knowledge/imports/operations/{operationID}", func(w http.ResponseWriter, r *http.Request) {
		actor, _ := credentialFromContext(r.Context())
		result, err := s.ImportOperation(r.Context(), chi.URLParam(r, "operationID"), actor.Device.ID)
		if err != nil {
			a.writeKnowledgeFailure(w, r, "import_operation", err)
			return
		}
		writeJSON(w, 200, minimalImportResult(result))
	})
}

func importInputFailure(w http.ResponseWriter, r *http.Request, err error) {
	var maxBytes *http.MaxBytesError
	if errors.As(err, &maxBytes) {
		writeError(w, r, 413, knowledge.CodePayloadTooLarge, "请求超出单批预算")
		return
	}
	writeError(w, r, 400, knowledge.CodeInvalidRequest, "导入请求无效")
}

func minimalImportResult(result knowledge.ImportResult) knowledge.ImportResult {
	result.Revision.Documents = nil
	result.Revision.Lineages = nil
	return result
}
