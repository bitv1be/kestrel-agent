package provider

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"time"
)

// SSEEvent is one server-sent event.
type SSEEvent struct {
	Event string
	Data  string
	ID    string
	Retry time.Duration
}

// SSEDecoder incrementally parses text/event-stream without buffering the
// whole response. It is safe for a single goroutine and allocation-conscious
// on the hot path.
type SSEDecoder struct {
	br      *bufio.Reader
	maxSize int
	data    []byte
	event   string
	id      string
	retry   time.Duration
	eof     bool
}

// NewSSEDecoder wraps r.
func NewSSEDecoder(r io.Reader) *SSEDecoder {
	return &SSEDecoder{br: bufio.NewReaderSize(r, 32*1024), maxSize: 4 << 20}
}

// Next returns the next event or io.EOF. Blank-line-delimited events are
// dispatched; comment-only lines (": keep-alive") are ignored.
func (d *SSEDecoder) Next() (SSEEvent, error) {
	for {
		line, err := d.readLine()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(d.data) > 0 || d.event != "" {
					return d.dispatch(), nil
				}
			}
			return SSEEvent{}, err
		}
		if len(line) == 0 {
			if len(d.data) > 0 || d.event != "" {
				return d.dispatch(), nil
			}
			continue
		}
		if line[0] == ':' {
			continue
		}
		field, value := string(line), ""
		if i := bytes.IndexByte(line, ':'); i >= 0 {
			field, value = string(line[:i]), strings.TrimPrefix(string(line[i+1:]), " ")
		}
		switch field {
		case "data":
			if len(d.data) > 0 {
				d.data = append(d.data, '\n')
			}
			if len(d.data)+len(value) > d.maxSize {
				return SSEEvent{}, errors.New("SSE event exceeds size limit")
			}
			d.data = append(d.data, value...)
		case "event":
			d.event = string(value)
		case "id":
			d.id = string(value)
		case "retry":
			if ms, perr := time.ParseDuration(string(value) + "ms"); perr == nil {
				d.retry = ms
			}
		}
	}
}

func (d *SSEDecoder) readLine() ([]byte, error) {
	line, err := d.br.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return nil, err
	}
	if len(line) > d.maxSize {
		return nil, errors.New("SSE line exceeds size limit")
	}
	line = bytes.TrimSuffix(line, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
	return line, nil
}

func (d *SSEDecoder) dispatch() SSEEvent {
	ev := SSEEvent{Event: d.event, Data: string(d.data), ID: d.id, Retry: d.retry}
	d.data = d.data[:0]
	d.event = ""
	d.retry = 0
	return ev
}
