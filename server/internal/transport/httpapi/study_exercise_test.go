package httpapi

import "testing"

func TestStudyExerciseRouteClassification(t *testing.T) {
	for _, path := range []string{"/v1/learning/exercises/id", "/v1/learning/exercises/id/help", "/v1/learning/exercises/id/assessment/resume"} {
		if !mentorPath(path) {
			t.Fatalf("exercise route bypasses scope classification: %s", path)
		}
	}
}
