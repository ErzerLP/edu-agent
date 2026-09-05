package localexec

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sort"
	"time"
)

type outputChunk struct {
	offset int64
	data   []byte
}

type outputStream struct {
	chunks    []outputChunk
	received  int64
	retained  int64
	saved     int64
	drainUsed time.Duration
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
	Data             []byte
	Offset           int64
	NextOffset       int64
	Received         int64
	Retained         int64
	More             bool
	Truncated        bool
	Incomplete       bool
	Saved            int64
	Availability     string
	PersistenceError string
	Historical       bool
}

func (m *Manager) Read(owner, taskID, stream string, offset int64, limit int) (OutputPage, error) {
	if limit > MaxReadBytes {
		limit = MaxReadBytes
	}
	return m.readOutput(context.Background(), owner, taskID, stream, offset, limit)
}

func taskStream(t *task, name string) (*outputStream, error) {
	switch name {
	case "stdout":
		return &t.stdout, nil
	case "stderr":
		return &t.stderr, nil
	default:
		return nil, failure("invalid_stream")
	}
}

func (m *Manager) outputPageLocked(t *task, output *outputStream, offset int64) OutputPage {
	retained := m.readableLocked(t, output)
	page := OutputPage{Offset: offset, NextOffset: offset, Received: output.received,
		Retained: retained, Saved: output.saved, Availability: "memory",
		Truncated: retained < output.received, Incomplete: t.outputIncomplete,
		PersistenceError: m.persistenceErrorLocked(t), Historical: t.snapshot.Restored}
	if t.snapshot.Restored && t.binding.available && !t.archiveUnreadable {
		page.Availability = "saved"
	}
	if output.saved > retained || t.snapshot.Restored && (!t.binding.available || t.archiveUnreadable) {
		page.Availability = "unavailable"
	}
	return page
}

func memoryPage(output *outputStream, page OutputPage, count int64) OutputPage {
	page.Availability = "memory"
	page.Data = make([]byte, 0, int(count))
	i := sort.Search(len(output.chunks), func(i int) bool { return output.chunks[i].offset+int64(len(output.chunks[i].data)) > page.Offset })
	for count > 0 && i < len(output.chunks) {
		chunk := output.chunks[i]
		start := page.NextOffset - chunk.offset
		n := min(count, int64(len(chunk.data))-start)
		page.Data = append(page.Data, chunk.data[start:start+n]...)
		page.NextOffset += n
		count -= n
		i++
	}
	page.More = page.NextOffset < page.Received
	return page
}

func (m *Manager) readOutput(ctx context.Context, owner, taskID, name string, offset int64, limit int) (OutputPage, error) {
	m.mu.Lock()
	t, err := m.taskLocked(owner, taskID)
	if err != nil {
		m.mu.Unlock()
		return OutputPage{}, err
	}
	stream, err := taskStream(t, name)
	if err != nil {
		m.mu.Unlock()
		return OutputPage{}, err
	}
	if offset < 0 || offset > stream.received {
		m.mu.Unlock()
		return OutputPage{}, failure("invalid_offset")
	}
	if limit <= 0 {
		m.mu.Unlock()
		return OutputPage{}, failure("invalid_limit")
	}
	page := m.outputPageLocked(t, stream, offset)
	count := min(int64(limit), max(int64(0), page.Retained-offset))
	if offset+count <= stream.retained && count > 0 {
		page = memoryPage(stream, page, count)
		m.mu.Unlock()
		return page, nil
	}
	if count == 0 {
		page.NextOffset = page.Received
		m.mu.Unlock()
		return page, nil
	}
	m.mu.Unlock()

	// Bind cannot retire this handle during I/O. Recheck the waterline after
	// acquiring the binding gate; no global manager lock spans a backend call.
	t.binding.mu.RLock()
	defer t.binding.mu.RUnlock()
	m.mu.Lock()
	page = m.outputPageLocked(t, stream, offset)
	count = min(int64(limit), max(int64(0), page.Retained-offset))
	if offset+count <= stream.retained && count > 0 {
		page = memoryPage(stream, page, count)
		m.mu.Unlock()
		return page, nil
	}
	if count == 0 {
		page.NextOffset = page.Received
		m.mu.Unlock()
		return page, nil
	}
	saved := stream.saved
	m.mu.Unlock()
	page.Availability = "saved"
	page.Data = make([]byte, 0, int(count))
	for count > 0 {
		index := page.NextOffset / archiveSegmentBytes
		callCtx, cancel := context.WithTimeout(ctx, archiveCallLimit)
		data, readErr := t.binding.backend.ReadArtifact(callCtx, segmentName(taskID, name, index))
		cancel()
		expected := min(int64(archiveSegmentBytes), saved-index*archiveSegmentBytes)
		if readErr == nil && (int64(len(data)) < expected || len(data) > archiveSegmentBytes) {
			readErr = failure("output_corrupt")
		}
		if readErr != nil {
			if ctx.Err() != nil {
				return OutputPage{}, failure("search_canceled")
			}
			code := archiveError(readErr)
			m.mu.Lock()
			t.archiveUnreadable = true
			m.archiveFailedLocked(t, code)
			page = m.outputPageLocked(t, stream, offset)
			available := min(int64(limit), max(int64(0), stream.retained-offset))
			if available > 0 {
				page = memoryPage(stream, page, available)
			} else {
				page.NextOffset = page.Received
			}
			m.mu.Unlock()
			return page, failure(code)
		}
		start := page.NextOffset % archiveSegmentBytes
		n := min(count, expected-start)
		page.Data = append(page.Data, data[start:start+n]...)
		page.NextOffset += n
		count -= n
	}
	page.More = page.NextOffset < page.Received
	return page, nil
}

func (m *Manager) capture(t *task, stream *outputStream, name string, reader *os.File, done chan<- struct{}) {
	defer close(done)
	defer reader.Close()
	defer func() {
		t.archiveMu.Lock()
		if name == "stdout" {
			t.stdoutTail = nil
		} else {
			t.stderrTail = nil
		}
		t.archiveMu.Unlock()
	}()
	buffer := make([]byte, 32<<10)
	for {
		m.mu.Lock()
		if !t.drainStarted.IsZero() {
			// Only time spent reading the pipe consumes the EOF budget. Slow
			// persistence is settled separately, never called a leaked child.
			_ = reader.SetReadDeadline(time.Now().Add(max(time.Duration(0), outputDrainLimit-stream.drainUsed)))
		}
		m.mu.Unlock()
		readStarted := time.Now()
		n, err := reader.Read(buffer)
		readFinished := time.Now()
		m.mu.Lock()
		if !t.drainStarted.IsZero() {
			start := readStarted
			if start.Before(t.drainStarted) {
				start = t.drainStarted
			}
			if readFinished.After(start) {
				stream.drainUsed += readFinished.Sub(start)
			}
		}
		offset := stream.received
		if n > 0 {
			stream.received += int64(n)
			room := min(int64(m.options.OutputBytesPerTask)-t.stdout.retained-t.stderr.retained, int64(m.options.OutputBytesTotal)-m.retained)
			keep := min(int64(n), room)
			if stream.truncated {
				keep = 0
			} // Never create a later memory island.
			if keep > 0 {
				data := append([]byte(nil), buffer[:keep]...)
				stream.chunks = append(stream.chunks, outputChunk{offset: stream.retained, data: data})
				stream.retained += keep
				m.retained += keep
			}
			if keep < int64(n) {
				stream.truncated = true
			}
		}
		m.mu.Unlock()
		if n > 0 {
			m.saveOutput(t, stream, name, offset, buffer[:n])
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
