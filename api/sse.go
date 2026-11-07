package api

import (
	"bufio"
	"io"
	"strings"
)

// sseReader decodes a text/event-stream body following the SSE specification
// closely enough for API relay purposes: multiple `data:` lines in one event
// are joined with "\n" instead of being dropped, comment lines are skipped, and
// all other fields (event:, id:, retry:) are ignored.
type sseReader struct {
	br   *bufio.Reader
	data []string
	eof  bool
}

func newSSEReader(r io.Reader) *sseReader {
	return &sseReader{br: bufio.NewReaderSize(r, 4096)}
}

// Next returns the next event's data payload. It returns io.EOF once the body
// is exhausted; callers must decide whether an EOF without a terminal marker
// counts as a premature disconnect.
func (s *sseReader) Next() (string, error) {
	for {
		if s.eof {
			if len(s.data) > 0 {
				out := strings.Join(s.data, "\n")
				s.data = s.data[:0]
				return out, nil
			}
			return "", io.EOF
		}

		line, err := s.br.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				if len(s.data) > 0 {
					out := strings.Join(s.data, "\n")
					s.data = s.data[:0]
					return out, nil
				}
				return "", err
			}
			s.eof = true
			// A trailing line without a newline still belongs to this event.
			if line = strings.TrimRight(line, "\r\n"); line != "" {
				s.consume(line)
			}
			continue
		}

		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if len(s.data) > 0 {
				out := strings.Join(s.data, "\n")
				s.data = s.data[:0]
				return out, nil
			}
			continue
		}
		s.consume(line)
	}
}

func (s *sseReader) consume(line string) {
	if strings.HasPrefix(line, ":") {
		return // comment / keepalive
	}
	if !strings.HasPrefix(line, "data:") {
		return // event:, id:, retry:
	}
	v := strings.TrimPrefix(line, "data:")
	v = strings.TrimPrefix(v, " ") // single optional space after the colon
	s.data = append(s.data, v)
}
