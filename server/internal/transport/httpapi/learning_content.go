package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/go-chi/chi/v5"
)

func (a *API) mountLearningContent(r chi.Router) {
	owners := []privacy.OwnerKind{privacy.OwnerLearning, privacy.OwnerTutoring, privacy.OwnerKnowledge}
	r.With(a.requireScope("learning:read")).Get("/v1/learning/content/capabilities", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"protocol_version": 1, "available": a.learningContent.Available(), "blocks": []string{"markdown", "code", "math", "table", "citation", "callout", "question", "answer_input", "group"}, "interactions": []string{"none", "text", "single_choice"}})
	})
	if a.learningContent != nil {
		read := r.With(a.requireScope("learning:read"), a.contentProtocol, a.responseReadPermit("content_redacted", owners...))
		write := r.With(a.requireScope("learning:write"), a.contentProtocol, a.responseReadPermit("privacy_clear_in_progress", owners...))
		read.Get("/v1/learning/content/{artifactID}", a.contentGet)
		read.Get("/v1/learning/content", a.contentLibrary)
		read.Get("/v1/learning/content/{artifactID}/preferences", a.contentPreference)
		write.Put("/v1/learning/content/{artifactID}/preferences", a.contentPreference)
		read.Get("/v1/learning/content/{artifactID}/export", a.contentExport)
		read.Get("/v1/learning/content/{artifactID}/sources/{referenceID}", a.contentCitation)
		write.Post("/v1/learning/content/{artifactID}/restore", a.contentRestore)
		write.Post("/v1/learning/content/{artifactID}/reuse", a.contentReuse)
		read.Get("/v1/learning/content/{artifactID}/revisions", a.contentHistory)
		write.Post("/v1/learning/content/{artifactID}/revisions", a.contentCommit)
		write.Post("/v1/tutoring/sessions/{sessionID}/content", a.contentEnsure)
		if a.learning != nil {
			write.Post("/v1/learning/content/{artifactID}/answers", a.contentAnswer)
		}
	}
	if a.learning != nil {
		r.With(a.requireScope("learning:read"), a.responseReadPermit("content_redacted", owners...)).Get("/v1/tutoring/sessions/{sessionID}/operations/{operationID}", a.sessionOperation)
	}
}

func (a *API) contentProtocol(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if len(r.Header.Values("X-Learning-Content-Version")) != 1 || r.Header.Get("X-Learning-Content-Version") != "1" {
			contentFailure(w, r, learningcontent.ErrUnsupported)
			return
		}
		if len(r.Header.Values("X-Learning-Space-ID")) != 1 || !validLearningUUID(r.Header.Get("X-Learning-Space-ID")) {
			contentFailure(w, r, learningcontent.ErrInvalid)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func contentFailure(w http.ResponseWriter, r *http.Request, err error) bool {
	status := 0
	code := ""
	switch {
	case errors.Is(err, learningcontent.ErrInvalid):
		status, code = 400, learningcontent.ErrInvalid.Error()
	case errors.Is(err, learningcontent.ErrNotFound):
		status, code = 404, learningcontent.ErrNotFound.Error()
	case errors.Is(err, learningcontent.ErrConflict):
		status, code = 409, learningcontent.ErrConflict.Error()
	case errors.Is(err, learningcontent.ErrForbidden):
		status, code = 403, learningcontent.ErrForbidden.Error()
	case errors.Is(err, learningcontent.ErrUnavailable):
		status, code = 503, learningcontent.ErrUnavailable.Error()
	case errors.Is(err, learningcontent.ErrUnsupported):
		status, code = 422, learningcontent.ErrUnsupported.Error()
	}
	if status == 0 {
		return false
	}
	writeError(w, r, status, code, "学习内容请求未完成："+code)
	return true
}

func (a *API) contentError(w http.ResponseWriter, r *http.Request, err error) {
	if !contentFailure(w, r, err) {
		a.writeLearningFailure(w, r, "learning_content", err)
	}
}

func (a *API) contentEnsure(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	var input struct {
		ProtocolVersion int    `json:"protocol_version"`
		ActivityID      string `json:"activity_id"`
	}
	if !a.decodeLearning(w, r, &input) {
		return
	}
	if input.ProtocolVersion != 1 {
		a.contentError(w, r, learningcontent.ErrUnsupported)
		return
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.learningContent.Ensure(r.Context(), actor, chi.URLParam(r, "sessionID"), input.ActivityID)
	if err != nil {
		a.contentError(w, r, err)
		return
	}
	writeJSON(w, 200, result)
}

func (a *API) contentGet(w http.ResponseWriter, r *http.Request) {
	q, ok := strictLearningQuery(w, r, "version")
	if !ok {
		return
	}
	var version int64
	if q.Has("version") {
		var err error
		version, err = strconv.ParseInt(q.Get("version"), 10, 64)
		if err != nil || version < 1 {
			a.contentError(w, r, learningcontent.ErrInvalid)
			return
		}
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.learningContent.Get(r.Context(), actor, chi.URLParam(r, "artifactID"), version)
	if err != nil {
		a.contentError(w, r, err)
		return
	}
	writeJSON(w, 200, result)
}

func (a *API) contentHistory(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.learningContent.History(r.Context(), actor, chi.URLParam(r, "artifactID"))
	if err != nil {
		a.contentError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": result})
}

func (a *API) contentCommit(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	var input learningcontent.Commit
	if !a.decodeLearning(w, r, &input) {
		return
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.learningContent.Commit(r.Context(), actor, chi.URLParam(r, "artifactID"), input)
	if err != nil {
		a.contentError(w, r, err)
		return
	}
	writeJSON(w, 201, result)
}

func (a *API) contentAnswer(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	var input struct {
		actionAttemptInput
		ContentVersion int64 `json:"content_version"`
	}
	if !a.decodeLearning(w, r, &input) {
		return
	}
	if input.Action == nil || *input.Action != tutoring.ActionSubmitAttempt || input.Answer == nil || input.Help == nil || input.ContentVersion < 1 {
		writeLearningInvalid(w, r)
		return
	}
	actor, _ := credentialFromContext(r.Context())
	id := chi.URLParam(r, "artifactID")
	revision, err := a.learningContent.Get(r.Context(), actor, id, input.ContentVersion)
	if err != nil {
		a.contentError(w, r, err)
		return
	}
	op, err := input.operation(input, "session", revision.SessionID)
	if err != nil {
		writeLearningInvalid(w, r)
		return
	}
	ctx := a.learningContent.WithAnswer(r.Context(), actor, id, input.ContentVersion)
	result, err := a.learning.ApplyAction(ctx, actor.Device.ID, revision.SessionID, learning.ActionCommand{Operation: op, Action: tutoring.ActionSubmitAttempt, Answer: *input.Answer, Help: *input.Help})
	if err != nil {
		a.contentError(w, r, err)
		return
	}
	writeJSON(w, 200, result)
}

func (a *API) sessionOperation(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	session, operation := chi.URLParam(r, "sessionID"), chi.URLParam(r, "operationID")
	if !validLearningUUID(session) || !validLearningUUID(operation) {
		writeLearningInvalid(w, r)
		return
	}
	reader, ok := a.learning.(interface {
		SessionOperation(context.Context, string, string, string) (learning.SessionOperationReceipt, error)
	})
	if !ok {
		writeError(w, r, 501, "operation_lookup_unavailable", "当前服务不支持教学操作核对")
		return
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := reader.SessionOperation(r.Context(), actor.Device.ID, session, operation)
	if err != nil {
		a.writeLearningFailure(w, r, "session_operation", err)
		return
	}
	writeJSON(w, 200, result)
}
