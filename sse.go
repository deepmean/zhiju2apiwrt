package main

import (
	"bufio"
	"bytes"
	"io"
)

// sseEvent is one server-sent event from the upstream turn stream.
type sseEvent struct {
	ID    string
	Event string
	Data  []byte
}

// readSSE feeds every event of an SSE body to fn until the body ends or fn errors.
// The upstream sends `id:`, `event:` and single-line `data:` frames, but the parser
// handles the full format (comments, multi-line data, CRLF) so a server tweak cannot
// silently truncate a reply.
func readSSE(r io.Reader, fn func(sseEvent) error) error {
	br := bufio.NewReaderSize(r, 32*1024)
	var (
		ev    sseEvent
		data  bytes.Buffer
		have  bool
	)
	flush := func() error {
		if !have {
			return nil
		}
		ev.Data = bytes.TrimRight(data.Bytes(), "\n")
		err := fn(ev)
		ev = sseEvent{}
		data.Reset()
		have = false
		return err
	}
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimRight(line, "\r\n")
			switch {
			case len(line) == 0:
				if e := flush(); e != nil {
					return e
				}
			case line[0] == ':':
				// comment / keep-alive
			default:
				field, value := splitField(line)
				switch field {
				case "data":
					if data.Len() > 0 {
						data.WriteByte('\n')
					}
					data.Write(value)
					have = true
				case "event":
					ev.Event = string(value)
					have = true
				case "id":
					ev.ID = string(value)
					have = true
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return flush()
			}
			return err
		}
	}
}

func splitField(line []byte) (string, []byte) {
	i := bytes.IndexByte(line, ':')
	if i < 0 {
		return string(line), nil
	}
	v := line[i+1:]
	if len(v) > 0 && v[0] == ' ' {
		v = v[1:]
	}
	return string(line[:i]), v
}
