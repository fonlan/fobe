package aiprotocol

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Server-Sent Events reading, shared by all three dialects: all of them stream
// `event:`/`data:` frames (OpenAI completions uses bare `data:` lines, which is
// the same shape with an empty event name).

// errStreamDone is returned by a parser to stop reading after it has seen the
// protocol's terminal event. It never escapes Stream.
var errStreamDone = errors.New("aiprotocol: stream complete")

// maxSSELineBytes caps one line. Tool arguments arrive as fragments, but a
// single fragment can still be large (a base64 image in a tool call), so the
// bufio.Scanner default of 64 KiB is not enough. A line beyond this is a broken
// upstream, not a reason to grow without bound.
const maxSSELineBytes = 4 << 20

// forEachSSE dispatches every complete event in r to fn. It returns fn's error
// (including errStreamDone) unchanged, and a final event that arrives without a
// trailing blank line still counts. SSE comments (`: keep-alive`) are ignored.
func forEachSSE(r io.Reader, fn func(name, data string) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxSSELineBytes)

	var (
		name string
		data []string
	)
	dispatch := func() error {
		if len(data) == 0 {
			name = ""
			return nil
		}
		evName := name
		payload := strings.Join(data, "\n")
		name = ""
		data = data[:0]
		return fn(evName, payload)
	}

	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case line == "":
			if err := dispatch(); err != nil {
				return err
			}
		case strings.HasPrefix(line, ":"):
			// Comment / keep-alive.
		default:
			field, value, _ := strings.Cut(line, ":")
			switch field {
			case "event":
				name = strings.TrimPrefix(value, " ")
			case "data":
				// A single leading space after the colon is part of the framing,
				// not the payload.
				data = append(data, strings.TrimPrefix(value, " "))
			}
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read event stream: %w", err)
	}
	return dispatch()
}
