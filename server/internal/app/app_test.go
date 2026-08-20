package app

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/integrations/llm"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/platform/config"
)

type compositionTreeReader struct{}

func (compositionTreeReader) Tree(context.Context, string) (knowledge.TreeResult, error) {
	return knowledge.TreeResult{}, nil
}

func TestComposeLearningInjectsPostgresKnowledgeAndOptionalModel(t *testing.T) {
	cfg := config.Config{Model: config.ModelConfig{Name: "test-model", ContextWindow: 8192}}
	withoutModel, err := composeLearning(nil, compositionTreeReader{}, nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if withoutModel.learningStore == nil || withoutModel.tutoringStore == nil || withoutModel.resolver == nil || withoutModel.service == nil || withoutModel.model != nil {
		t.Fatalf("nil-model composition=%+v", withoutModel)
	}

	baseURL, _ := url.Parse("http://127.0.0.1:1/v1")
	client, err := llm.New(llm.Options{BaseURL: baseURL, Model: "test-model", APIKey: "test-key", ContextWindow: 8192, MinimumContext: 4096, Timeout: time.Second, ProbeCacheTTL: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	withModel, err := composeLearning(nil, compositionTreeReader{}, client, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if withModel.learningStore == nil || withModel.tutoringStore == nil || withModel.resolver == nil || withModel.service == nil || withModel.model == nil {
		t.Fatalf("model-enabled composition=%+v", withModel)
	}
}
