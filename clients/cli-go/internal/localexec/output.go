package localexec

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"sort"
)

type outputChunk struct {
	offset int64
	data   []byte
}

type outputStream struct {
	chunks    []outputChunk
	received  int64
	retained  int64
	truncated bool
}

// OutputPage always uses original byte positions, not runes or re-encoded text.
// Retention is a prefix [0, Retained); [Retained, Received) is a known gap.
// A read in that gap returns no Data and advances NextOffset to Received. A page
// ending at the retained boundary does not skip the gap implicitly. More refers
// to the current Received waterline, not to possible future output. Incomplete
// means pipe EOF could not be established; Received then is only an observed
// lower bound. Data is a copy owned by the caller.
type OutputPage struct {
	Data       []byte
	Offset     int64
	NextOffset int64
	Received   int64
	Retained   int64
	More       bool
	Truncated  bool
	Incomplete bool
}

func (m *Manager) Read(owner, taskID, stream string, offset int64, limit int) (OutputPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, err := m.taskLocked(owner, taskID)
	if err != nil {
		return OutputPage{}, err
	}
	var output *outputStream
	switch stream {
	case "stdout":
		output = &t.stdout
	case "stderr":
		output = &t.stderr
	default:
		return OutputPage{}, failure("invalid_stream")
	}
	if offset < 0 || offset > output.received {
		return OutputPage{}, failure("invalid_offset")
	}
	if limit <= 0 {
		return OutputPage{}, failure("invalid_limit")
	}
	if limit > MaxReadBytes {
		limit = MaxReadBytes
	}
	page := OutputPage{Offset: offset, NextOffset: offset, Received: output.received, Retained: output.retained, Truncated: output.truncated, Incomplete: t.outputIncomplete}
	if offset >= output.retained {
		page.NextOffset = output.received
		return page, nil
	}
	count := min(int64(limit), output.retained-offset)
	page.Data = make([]byte, 0, int(count))
	i := sort.Search(len(output.chunks), func(i int) bool { return output.chunks[i].offset+int64(len(output.chunks[i].data)) > offset })
	for count > 0 && i < len(output.chunks) {
		chunk := output.chunks[i]
		start := page.NextOffset - chunk.offset
		n := min(count, int64(len(chunk.data))-start)
		page.Data = append(page.Data, chunk.data[start:start+n]...)
		page.NextOffset += n
		count -= n
		i++
	}
	page.More = page.NextOffset < output.received
	return page, nil
}

func (m *Manager) capture(t *task, stream *outputStream, reader *os.File, done chan<- struct{}) {
	defer close(done)
	defer reader.Close()
	buffer := make([]byte, 32<<10)
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			m.mu.Lock()
			stream.received += int64(n)
			room := min(int64(m.options.OutputBytesPerTask)-t.stdout.retained-t.stderr.retained, int64(m.options.OutputBytesTotal)-m.retained)
			keep := min(int64(n), room)
			if stream.truncated {
				keep = 0
			} // Never create a later retained island.
			if keep > 0 {
				data := make([]byte, int(keep))
				copy(data, buffer[:keep])
				stream.chunks = append(stream.chunks, outputChunk{offset: stream.retained, data: data})
				stream.retained += keep
				m.retained += keep
			}
			if keep < int64(n) {
				stream.truncated = true
			}
			m.mu.Unlock()
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				m.mu.Lock()
				t.outputIncomplete = true
				m.mu.Unlock()
			}
			return
		}
	}
}

// Passing files (rather than exec's copying writers / StdoutPipe) separates
// process reaping from pipe EOF. Descendants may inherit those descriptors.
type processPipes struct {
	stdoutReader, stdoutWriter *os.File
	stderrReader, stderrWriter *os.File
	stdinReader, stdinWriter   *os.File
}

func openPipes(cmd *exec.Cmd, stdin bool) (*processPipes, error) {
	p := &processPipes{}
	var err error
	p.stdoutReader, p.stdoutWriter, err = os.Pipe()
	if err == nil {
		p.stderrReader, p.stderrWriter, err = os.Pipe()
	}
	if err == nil && stdin {
		p.stdinReader, p.stdinWriter, err = os.Pipe()
	}
	if err != nil {
		p.closeAll()
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = p.stdoutWriter, p.stderrWriter
	if stdin {
		cmd.Stdin = p.stdinReader
	}
	// nil Stdin is os.DevNull in exec: EOF by default, never the host terminal.
	return p, nil
}

func closeFile(file *os.File) {
	if file != nil {
		_ = file.Close()
	}
}
func (p *processPipes) closeChildEnds() {
	closeFile(p.stdoutWriter)
	closeFile(p.stderrWriter)
	closeFile(p.stdinReader)
}
func (p *processPipes) closeAll() {
	p.closeChildEnds()
	closeFile(p.stdoutReader)
	closeFile(p.stderrReader)
	closeFile(p.stdinWriter)
}
