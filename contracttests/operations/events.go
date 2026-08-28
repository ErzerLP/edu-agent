package operations

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

type GoEventSummary struct {
	Executed []string
	Passed   []string
	Failed   []string
	Skipped  []string
}

type goTestEvent struct {
	Action  string `json:"Action"`
	Package string `json:"Package"`
	Test    string `json:"Test"`
	Output  string `json:"Output"`
}

var (
	textRunPattern      = regexp.MustCompile(`^=== RUN\s+([^\s]+)`)
	textTerminalPattern = regexp.MustCompile(`^--- (PASS|FAIL|SKIP):\s+([^\s]+)`)
)

func ParseExpectedGoTestEvents(reader io.Reader, selected []string) (GoEventSummary, error) {
	selected = sortedUnique(selected)
	if len(selected) == 0 {
		return GoEventSummary{}, errors.New("Go test selection is empty")
	}
	summary, noTests, err := parseGoTestEvents(reader)
	if err != nil {
		return GoEventSummary{}, err
	}
	if noTests {
		return GoEventSummary{}, errors.New("Go test reported [no tests to run]")
	}

	executed := make(map[string]struct{})
	passed := make(map[string]struct{})
	failed := make(map[string]struct{})
	skipped := make(map[string]struct{})
	for _, expected := range selected {
		for _, actual := range summary.Executed {
			if goTargetMatches(expected, actual) {
				executed[expected] = struct{}{}
			}
		}
		for _, actual := range summary.Passed {
			if goTargetMatches(expected, actual) {
				passed[expected] = struct{}{}
			}
		}
		for _, actual := range summary.Failed {
			if goTargetMatches(expected, actual) {
				failed[expected] = struct{}{}
			}
		}
		for _, actual := range summary.Skipped {
			if goTargetMatches(expected, actual) {
				skipped[expected] = struct{}{}
			}
		}
	}
	for _, expected := range selected {
		if _, ok := executed[expected]; !ok {
			return GoEventSummary{}, fmt.Errorf("selected Go test did not execute: %s", expected)
		}
		if _, failedOK := failed[expected]; failedOK {
			return GoEventSummary{}, fmt.Errorf("selected Go test failed: %s", expected)
		}
		if _, skipOK := skipped[expected]; skipOK {
			return GoEventSummary{}, fmt.Errorf("selected Go test was skipped: %s", expected)
		}
		if _, ok := passed[expected]; !ok {
			return GoEventSummary{}, fmt.Errorf("selected Go test has no terminal event: %s", expected)
		}
	}
	if len(passed) == 0 {
		return GoEventSummary{}, errors.New("all selected Go tests were skipped")
	}
	return GoEventSummary{
		Executed: sortedSet(executed),
		Passed:   sortedSet(passed),
		Failed:   sortedSet(failed),
		Skipped:  sortedSet(skipped),
	}, nil
}

func ParseAnyGoTestEvents(reader io.Reader) (GoEventSummary, error) {
	summary, _, err := parseGoTestEvents(reader)
	if err != nil {
		return GoEventSummary{}, err
	}
	if len(summary.Executed) == 0 {
		return GoEventSummary{}, errors.New("Go test log contains no executed tests")
	}
	if len(summary.Failed) > 0 {
		return GoEventSummary{}, errors.New("Go test log contains failed tests")
	}
	if len(summary.Passed) == 0 {
		return GoEventSummary{}, errors.New("all executed Go tests were skipped")
	}
	return summary, nil
}

func parseGoTestEvents(reader io.Reader) (GoEventSummary, bool, error) {
	executed := map[string]struct{}{}
	passed := map[string]struct{}{}
	failed := map[string]struct{}{}
	skipped := map[string]struct{}{}
	noTests := false
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "[no tests to run]") {
			noTests = true
		}
		var event goTestEvent
		if json.Unmarshal([]byte(line), &event) == nil && event.Action != "" {
			if strings.Contains(event.Output, "[no tests to run]") {
				noTests = true
			}
			if event.Test == "" {
				continue
			}
			target := event.Test
			if event.Package != "" {
				target = event.Package + "::" + event.Test
			}
			recordGoAction(event.Action, target, executed, passed, failed, skipped)
			continue
		}
		if match := textRunPattern.FindStringSubmatch(line); len(match) == 2 {
			executed[match[1]] = struct{}{}
			continue
		}
		if match := textTerminalPattern.FindStringSubmatch(line); len(match) == 3 {
			recordGoAction(strings.ToLower(match[1]), match[2], executed, passed, failed, skipped)
		}
	}
	if err := scanner.Err(); err != nil {
		return GoEventSummary{}, false, err
	}
	return GoEventSummary{
		Executed: sortedSet(executed),
		Passed:   sortedSet(passed),
		Failed:   sortedSet(failed),
		Skipped:  sortedSet(skipped),
	}, noTests, nil
}

func recordGoAction(action, target string, executed, passed, failed, skipped map[string]struct{}) {
	switch action {
	case "run":
		executed[target] = struct{}{}
	case "pass":
		executed[target] = struct{}{}
		passed[target] = struct{}{}
	case "fail":
		executed[target] = struct{}{}
		failed[target] = struct{}{}
	case "skip":
		executed[target] = struct{}{}
		skipped[target] = struct{}{}
	}
}

func goTargetMatches(expected, actual string) bool {
	if expected == actual {
		return true
	}
	if strings.Contains(expected, "::") {
		return false
	}
	separator := strings.LastIndex(actual, "::")
	return separator >= 0 && actual[separator+2:] == expected
}

func sortedSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
