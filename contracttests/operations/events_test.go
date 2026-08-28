package operations

import (
	"strings"
	"testing"
)

func TestExpectedGoTestEventsRequireRunAndPass(t *testing.T) {
	log := strings.Join([]string{
		`{"Action":"run","Package":"example/blackbox","Test":"TestTarget"}`,
		`{"Action":"pass","Package":"example/blackbox","Test":"TestTarget"}`,
	}, "\n")
	summary, err := ParseExpectedGoTestEvents(strings.NewReader(log), []string{"TestTarget"})
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Executed) != 1 || len(summary.Passed) != 1 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
}

func TestExpectedGoTestEventsRejectEmptySkipNoTestsAndMissingTarget(t *testing.T) {
	tests := map[string]struct {
		selected []string
		log      string
	}{
		"empty selection": {nil, ""},
		"all skipped": {
			selected: []string{"TestTarget"},
			log: strings.Join([]string{
				`{"Action":"run","Package":"example/blackbox","Test":"TestTarget"}`,
				`{"Action":"skip","Package":"example/blackbox","Test":"TestTarget"}`,
			}, "\n"),
		},
		"one pass and one skipped": {
			selected: []string{"TestPassed", "TestSkipped"},
			log: strings.Join([]string{
				`{"Action":"run","Package":"example/blackbox","Test":"TestPassed"}`,
				`{"Action":"pass","Package":"example/blackbox","Test":"TestPassed"}`,
				`{"Action":"run","Package":"example/blackbox","Test":"TestSkipped"}`,
				`{"Action":"skip","Package":"example/blackbox","Test":"TestSkipped"}`,
			}, "\n"),
		},
		"no tests to run": {
			selected: []string{"TestTarget"},
			log:      `{"Action":"output","Package":"example/blackbox","Output":"testing: warning: no tests to run\n"}`,
		},
		"missing target": {
			selected: []string{"TestTarget"},
			log: strings.Join([]string{
				`{"Action":"run","Package":"example/blackbox","Test":"TestOther"}`,
				`{"Action":"pass","Package":"example/blackbox","Test":"TestOther"}`,
			}, "\n"),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseExpectedGoTestEvents(strings.NewReader(test.log), test.selected); err == nil {
				t.Fatal("invalid Go test evidence was accepted")
			}
		})
	}
}

func TestAnyGoTestEventsRejectEmptyAndAllSkip(t *testing.T) {
	if _, err := ParseAnyGoTestEvents(strings.NewReader("")); err == nil {
		t.Fatal("empty Go event stream was accepted")
	}
	skipLog := "=== RUN   TestOnly\n--- SKIP: TestOnly (0.00s)\n"
	if _, err := ParseAnyGoTestEvents(strings.NewReader(skipLog)); err == nil {
		t.Fatal("all-skip Go event stream was accepted")
	}
}
