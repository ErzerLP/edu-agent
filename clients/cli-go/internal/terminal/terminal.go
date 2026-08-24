package terminal

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"

	xterm "golang.org/x/term"
)

const ControlL = byte(0x0c)

var ErrNotTerminal = errors.New("output is not a terminal")

type IO struct {
	in        io.Reader
	out       io.Writer
	errOut    io.Writer
	reader    *bufio.Reader
	inputTTY  bool
	outputTTY bool
	inputFD   int
}

func New(in io.Reader, out, errOut io.Writer) *IO {
	value := &IO{in: in, out: out, errOut: errOut, reader: bufio.NewReader(in), inputFD: -1}
	if file, ok := in.(*os.File); ok {
		value.inputFD = int(file.Fd())
		value.inputTTY = xterm.IsTerminal(value.inputFD)
	}
	if file, ok := out.(*os.File); ok {
		value.outputTTY = xterm.IsTerminal(int(file.Fd()))
	}
	return value
}

func (t *IO) ReadSecret(prompt string) (string, error) {
	if prompt != "" {
		_, _ = fmt.Fprint(t.errOut, prompt)
	}
	if t.inputTTY && t.inputFD >= 0 {
		data, err := xterm.ReadPassword(t.inputFD)
		_, _ = fmt.Fprintln(t.errOut)
		if err != nil {
			return "", err
		}
		defer clear(data)
		return strings.TrimSpace(string(data)), nil
	}
	return t.readLineRaw()
}

func (t *IO) ReadLine(prompt string) (string, error) {
	if prompt != "" {
		_, _ = fmt.Fprint(t.errOut, prompt)
	}
	return t.readLineRaw()
}

func (t *IO) Confirm(prompt string) (bool, error) {
	answer, err := t.ReadLine(prompt + " [y/N] ")
	if err != nil {
		return false, err
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}

func (t *IO) Clear() error {
	if !t.outputTTY {
		return ErrNotTerminal
	}
	return clearScreen(t.out)
}

func (t *IO) OutputIsTTY() bool { return t.outputTTY }

func IsControlL(input []byte) bool { return len(input) == 1 && input[0] == ControlL }

func ColorEnabled(mode string, outputTTY bool, termName string, noColor bool) bool {
	if !outputTTY || noColor || termName == "dumb" {
		return false
	}
	return mode == "auto" || mode == "always"
}

func EscapeText(value string) string {
	var escaped strings.Builder
	for _, current := range value {
		switch current {
		case '\n':
			escaped.WriteString(`\n`)
		case '\r':
			escaped.WriteString(`\r`)
		case '\t':
			escaped.WriteString(`\t`)
		default:
			if unicode.IsControl(current) {
				if current <= 0xffff {
					_, _ = fmt.Fprintf(&escaped, `\u%04x`, current)
				} else {
					_, _ = fmt.Fprintf(&escaped, `\U%08x`, current)
				}
				continue
			}
			escaped.WriteRune(current)
		}
	}
	return escaped.String()
}

func (t *IO) readLineRaw() (string, error) {
	line, err := t.reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if errors.Is(err, io.EOF) && len(line) == 0 {
		return "", io.EOF
	}
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), nil
}
