// Bounded CI log excerpts retain failing-step diagnostics and head/tail context while streaming downloads.

package forge

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// MaxLogExcerptBytes bounds each agent-facing CI log excerpt, including notices.
const MaxLogExcerptBytes = 64 << 10

const excerptContextBytes = 4 << 10

// LimitLogExcerpt returns a UTF-8-valid, independently owned excerpt. Oversized
// input keeps head and tail context with an explicit omission notice between.
// It does not retain the backing storage of a larger source string.
func LimitLogExcerpt(s string, limit int) string {
	const notice = "\n[Log content omitted to fit prompt; open the job link for the full log.]\n"
	if len(s) > limit {
		if limit <= len(notice) {
			return strings.Clone(notice[:max(limit, 0)])
		}
		n := limit - len(notice)
		head := min(excerptContextBytes, n/2)
		tail := n - head
		for head > 0 && !utf8.RuneStart(s[head]) {
			head--
		}
		start := len(s) - tail
		for start < len(s) && !utf8.RuneStart(s[start]) {
			start++
		}
		s = s[:head] + notice + s[start:]
	}
	s = strings.ToValidUTF8(s, "\uFFFD")
	if len(s) > limit {
		s = s[:limit]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return strings.Clone(s)
}

// ReadLogExcerpt streams a CI download through EOF, retaining bounded context.
// GitHub-style grouped logs select only error-containing steps when present;
// otherwise recognized plain diagnostics and head/tail context are retained.
// Errors, cancellation,
// lines of arbitrary length, and ANSI sequences spanning chunks are handled
// without whole-body buffers. Sources exceeding the existing 100 MiB download
// allowance fail explicitly instead of returning an apparently complete log.
func ReadLogExcerpt(ctx context.Context, r io.Reader, groups bool) (string, error) {
	br := bufio.NewReaderSize(io.LimitReader(r, MaxLogBytes+1), 64<<10)
	p := logExcerptParser{groups: groups}
	var ansi logANSI
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		data, err := br.ReadSlice('\n')
		n += int64(len(data))
		if n > MaxLogBytes {
			return "", fmt.Errorf("CI log exceeds %d-byte download limit", MaxLogBytes)
		}
		clean, cleanErr := ansi.strip(data)
		if cleanErr != nil {
			return "", cleanErr
		}
		last := len(data) > 0 && data[len(data)-1] == '\n' || !errors.Is(err, bufio.ErrBufferFull)
		p.consume(clean, last)
		if errors.Is(err, io.EOF) {
			p.consume(ansi.pending, true)
			p.flushGroup()
			if err := ctx.Err(); err != nil {
				return "", err
			}
			return p.excerpt(), nil
		}
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return "", err
		}
	}
}

type logTail struct {
	data  [MaxLogExcerptBytes]byte
	start int
	size  int
	total int64
}

func (t *logTail) write(p []byte) {
	t.total += int64(len(p))
	if len(p) >= len(t.data) {
		copy(t.data[:], p[len(p)-len(t.data):])
		t.start = 0
		t.size = len(t.data)
		return
	}
	end := (t.start + t.size) % len(t.data)
	n := copy(t.data[end:], p)
	copy(t.data[:], p[n:])
	overflow := max(t.size+len(p)-len(t.data), 0)
	t.start = (t.start + overflow) % len(t.data)
	t.size = min(t.size+len(p), len(t.data))
}
func (t *logTail) bytes() []byte {
	out := make([]byte, t.size)
	n := copy(out, t.data[t.start:min(t.start+t.size, len(t.data))])
	copy(out[n:], t.data[:t.size-n])
	return out
}
func (t *logTail) reset() { t.start = 0; t.size = 0; t.total = 0 }

type logANSI struct {
	pending []byte
	out     []byte
}

func (a *logANSI) strip(p []byte) ([]byte, error) {
	a.out = a.out[:0]
	for len(p) > 0 {
		if len(a.pending) == 0 {
			n := bytes.IndexByte(p, 0x1b)
			if n < 0 {
				a.out = append(a.out, p...)
				break
			}
			a.out = append(a.out, p[:n]...)
			p = p[n:]
		}
		b := p[0]
		p = p[1:]
		if len(a.pending) == 0 {
			if b == 0x1b {
				a.pending = append(a.pending, b)
			} else {
				a.out = append(a.out, b)
			}
			continue
		}
		if len(a.pending) == 1 {
			if b == '[' {
				a.pending = append(a.pending, b)
				continue
			}
		} else {
			if b >= '0' && b <= '9' || b == ';' {
				if len(a.pending) >= 64 {
					return nil, errors.New("CI log ANSI sequence exceeds 64 bytes")
				}
				a.pending = append(a.pending, b)
				continue
			}
			if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' {
				a.pending = a.pending[:0]
				continue
			}
		}
		a.out = append(a.out, a.pending...)
		a.pending = a.pending[:0]
		if b == 0x1b {
			a.pending = append(a.pending, b)
		} else {
			a.out = append(a.out, b)
		}
	}
	return a.out, nil
}

type logLine uint8

const (
	logLineBody logLine = iota
	logLineEnd
	logLineGroup
)

type logExcerptParser struct {
	groups         bool
	raw            logTail
	body           logTail
	selected       logTail
	rawHead        []byte
	bodyHead       []byte
	selectedHead   []byte
	anchor         []byte
	selectedAnchor []byte
	prefix         []byte
	kind           logLine
	classified     bool
	group          bool
	hasError       bool
	header         []byte
}

func (p *logExcerptParser) consume(data []byte, last bool) {
	p.raw.write(data)
	p.rawHead = append(p.rawHead, data[:min(len(data), max(excerptContextBytes-len(p.rawHead), 0))]...)
	if !p.classified {
		n := min(len(data), 64-len(p.prefix))
		p.prefix = append(p.prefix, data[:n]...)
		data = data[n:]
		if len(p.prefix) < 64 && !last {
			return
		}
		p.startLine()
		p.part(p.prefix)
		p.prefix = p.prefix[:0]
	}
	p.part(data)
	if last {
		p.classified = false
	}
}
func (p *logExcerptParser) startLine() {
	p.classified = true
	p.kind = logLineBody
	line := string(p.prefix)
	if len(line) > 29 && line[4] == '-' && line[10] == 'T' && line[27] == 'Z' && line[28] == ' ' {
		line = line[29:]
	}
	switch {
	case p.groups && strings.HasPrefix(line, "##[group]"):
		p.flushGroup()
		p.group = true
		p.hasError = false
		p.body.reset()
		p.bodyHead = p.bodyHead[:0]
		p.header = append(p.header[:0], strings.TrimPrefix(line, "##[group]")...)
		p.kind = logLineGroup
	case p.groups && strings.HasPrefix(line, "##[endgroup]"):
		p.flushGroup()
		p.kind = logLineEnd
	case strings.HasPrefix(line, "##[error]") || plainLogDiagnostic(line):
		if p.anchor == nil {
			p.anchor = make([]byte, 0, excerptContextBytes)
		} else {
			p.anchor = p.anchor[:0]
		}
		if p.group {
			p.hasError = true
		}
	}
}
func (p *logExcerptParser) part(data []byte) {
	switch p.kind {
	case logLineGroup:
		// Add an explicit notice when the bounded header prefix ends mid-line.
		if len(data) > 0 && data[len(data)-1] != '\n' && !bytes.Contains(p.header, []byte("[step name abbreviated]")) {
			p.header = append(p.header, " [step name abbreviated]"...)
		}
	case logLineEnd:
	default:
		if p.group {
			p.body.write(data)
			p.bodyHead = append(p.bodyHead, data[:min(len(data), max(excerptContextBytes-len(p.bodyHead), 0))]...)
		}
		if p.anchor != nil {
			p.anchor = append(p.anchor, data[:min(len(data), max(excerptContextBytes-len(p.anchor), 0))]...)
		}
	}
}
func (p *logExcerptParser) flushGroup() {
	if p.group && p.hasError {
		header := []byte("Step: " + strings.TrimSpace(string(p.header)) + "\n")
		p.selected.write(header)
		body := p.body.bytes()
		p.selected.write(body)
		p.selected.total += p.body.total - int64(len(body))
		p.selectedHead = append(p.selectedHead, header[:min(len(header), max(excerptContextBytes-len(p.selectedHead), 0))]...)
		p.selectedHead = append(p.selectedHead, p.bodyHead[:min(len(p.bodyHead), max(excerptContextBytes-len(p.selectedHead), 0))]...)
		p.selectedAnchor = append(p.selectedAnchor[:0], p.anchor...)
	}
	p.group = false
}
func (p *logExcerptParser) excerpt() string {
	tail := &p.raw
	head := p.rawHead
	anchor := p.anchor
	prefix := ""
	if p.selected.total > 0 {
		tail = &p.selected
		head = p.selectedHead
		anchor = p.selectedAnchor
		prefix = "[Only failing steps included; open job link for the full log.]\n"
	}
	text := string(tail.bytes())
	if tail.total > int64(len(text)) {
		text = "[Earlier log content omitted; preserving error context and log tail.]\n" + string(anchor) + "\n" + string(head) + "\n" + text
	}
	return LimitLogExcerpt(prefix+strings.TrimRight(text, "\n"), MaxLogExcerptBytes)
}

// plainLogDiagnostic recognizes common compiler, test and runtime failures in a
// bounded line prefix. This is a selection heuristic, not a log-format parser:
// full logs remain linked and omissions are explicit. Subsequent lines provide
// bounded context for multiline diagnostics such as tracebacks.
func plainLogDiagnostic(line string) bool {
	line = strings.ToLower(strings.TrimSpace(line))
	if line == "fail" {
		return true
	}
	for _, prefix := range []string{"fail:", "fail ", "fail\t", "failed:", "--- fail:", "error:", "error[", "fatal:", "fatal error:", "panic:", "traceback (most recent call last):", "assertionerror:", "exception:", "[error]"} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	for _, marker := range []string{": error", ": fatal", " error:", " error[", " undefined:", " assertionerror:", " exception:"} {
		if strings.Contains(line, marker) {
			return true
		}
	}
	// Source locations are useful even when the diagnostic text occurs beyond
	// the bounded prefix (file:line: or file:line:column:).
	for i := range len(line) {
		if line[i] != ':' || !strings.ContainsAny(line[:i], "./") {
			continue
		}
		j := i + 1
		for j < len(line) && line[j] >= '0' && line[j] <= '9' {
			j++
		}
		if j > i+1 && j < len(line) && line[j] == ':' {
			return true
		}
	}
	return false
}
