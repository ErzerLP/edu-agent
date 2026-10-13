package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	space "github.com/edu-agent/edu-agent/server/internal/learningspace"
)

type directionSpaces struct{ LearningSpaceService }

func (directionSpaces) Get(context.Context, string) (space.Space, error) {
	return space.Space{ID: space.DefaultID, Name: "区", Description: "", Status: "active", Version: 1, Direction: "方向", DirectionVersion: 1}, nil
}
func (s directionSpaces) List(ctx context.Context, _ space.Query) (space.Page, error) {
	item, _ := s.Get(ctx, "")
	return space.Page{Items: []space.Space{item}}, nil
}

func TestDirectionResponseOptInPreservesStrictLegacyReaders(t *testing.T) {
	api := &API{learningSpaces: directionSpaces{}}
	for _, path := range []string{"/v1/learning-spaces", "/v1/learning-spaces?include_direction=true"} {
		response := httptest.NewRecorder()
		api.listLearningSpaces(response, httptest.NewRequest("GET", path, nil))
		if response.Code != 200 {
			t.Fatal(response.Code)
		}
		if path == "/v1/learning-spaces" {
			var legacy struct {
				Items []struct {
					ID, Name, Description, Status string
					Version                       int64
					CreatedAt                     any `json:"created_at"`
					UpdatedAt                     any `json:"updated_at"`
				}
			}
			decoder := json.NewDecoder(bytes.NewReader(response.Body.Bytes()))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&legacy); err != nil {
				t.Fatalf("legacy strict reader broke: %v", err)
			}
		} else if !bytes.Contains(response.Body.Bytes(), []byte(`"direction":"方向"`)) {
			t.Fatal("opt-in lost direction")
		}
	}
}
