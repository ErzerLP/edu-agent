package blackbox

import (
	"strings"
	"testing"
)

type learnInput struct {
	lines []string
}

func TestStandardTeachingInputOrder(t *testing.T) {
	got := standardTeachingInput().
		answer("accepted response").
		defaultHelp().
		acknowledgeFeedback().
		String()
	want := "y\n\ny\ny\n\naccepted response\n\n\n"
	if got != want {
		t.Fatalf("standard teaching input order changed")
	}
}

func standardTeachingInput() *learnInput {
	return newLearnInput().
		confirmRouteRetrieval().
		continueFromRoute().
		confirmExplanationRetrieval().
		confirmActivityRetrieval().
		presentActivity()
}

func dueReviewInput() *learnInput {
	return newLearnInput().
		confirmRouteRetrieval().
		continueFromRoute().
		confirmDueReview().
		confirmReviewActivityRetrieval()
}

func newLearnInput() *learnInput {
	return &learnInput{}
}

func (input *learnInput) confirmRouteRetrieval() *learnInput {
	return input.line("y")
}

func (input *learnInput) continueFromRoute() *learnInput {
	return input.line("")
}

func (input *learnInput) confirmExplanationRetrieval() *learnInput {
	return input.line("y")
}

func (input *learnInput) confirmActivityRetrieval() *learnInput {
	return input.line("y")
}

func (input *learnInput) confirmFreeAnswerRetrieval() *learnInput {
	return input.line("y")
}

func (input *learnInput) confirmAttachedQuizRetrieval() *learnInput {
	return input.line("y")
}

func (input *learnInput) confirmDueReview() *learnInput {
	return input.line("y")
}

func (input *learnInput) confirmReviewActivityRetrieval() *learnInput {
	return input.line("y")
}

func (input *learnInput) presentActivity() *learnInput {
	return input.line("")
}

func (input *learnInput) answer(value string) *learnInput {
	return input.line(value)
}

func (input *learnInput) multilineAnswer(lines ...string) *learnInput {
	input.line(":answer")
	for _, line := range lines {
		input.line(line)
	}
	return input.line(".")
}

func (input *learnInput) defaultHelp() *learnInput {
	return input.line("")
}

func (input *learnInput) selectHelp(value string) *learnInput {
	return input.line(value)
}

func (input *learnInput) acknowledgeFeedback() *learnInput {
	return input.line("")
}

func (input *learnInput) ask(question string) *learnInput {
	return input.line(":ask " + question)
}

func (input *learnInput) convertToQuiz() *learnInput {
	return input.line(":quiz")
}

func (input *learnInput) resumeFocus() *learnInput {
	return input.line(":resume")
}

func (input *learnInput) quit() *learnInput {
	return input.line(":quit")
}

func (input *learnInput) String() string {
	return strings.Join(input.lines, "\n") + "\n"
}

func (input *learnInput) line(value string) *learnInput {
	input.lines = append(input.lines, value)
	return input
}
