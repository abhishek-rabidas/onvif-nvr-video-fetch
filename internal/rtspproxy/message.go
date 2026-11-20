package rtspproxy

import (
	"bufio"
	"encoding/binary"
	"io"
	"strconv"
	"strings"
)

// header is one RTSP header line; names are compared case-insensitively.
type header struct{ k, v string }

// message is an RTSP request or response; header order is preserved.
type message struct {
	first   string // request line or status line
	headers []header
	body    []byte
}

// readMessage reads either an interleaved binary frame ("$" ch len16 payload) or an RTSP message.
func readMessage(r *bufio.Reader) (frame []byte, m *message, err error) {
	b, err := r.Peek(1)
	if err != nil {
		return nil, nil, err
	}
	if b[0] == '$' {
		var hdr [4]byte
		if _, err = io.ReadFull(r, hdr[:]); err != nil {
			return nil, nil, err
		}
		frame = make([]byte, 4+int(binary.BigEndian.Uint16(hdr[2:])))
		copy(frame, hdr[:])
		_, err = io.ReadFull(r, frame[4:])
		return frame, nil, err
	}
	m = &message{}
	for m.first == "" { // skip stray blank lines between messages
		if m.first, err = readLine(r); err != nil {
			return nil, nil, err
		}
	}
	for {
		line, err := readLine(r)
		if err != nil {
			return nil, nil, err
		}
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			m.headers = append(m.headers, header{strings.TrimSpace(k), strings.TrimSpace(v)})
		}
	}
	if n, _ := strconv.Atoi(m.get("Content-Length")); n > 0 {
		m.body = make([]byte, n)
		if _, err = io.ReadFull(r, m.body); err != nil {
			return nil, nil, err
		}
	}
	return nil, m, nil
}

// readLine reads one line without its CRLF/LF terminator.
func readLine(r *bufio.Reader) (string, error) {
	s, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(s, "\r\n"), nil
}

// get returns the first value of header k, or "".
func (m *message) get(k string) string {
	for _, h := range m.headers {
		if strings.EqualFold(h.k, k) {
			return h.v
		}
	}
	return ""
}

// values returns all values of header k (e.g. multiple WWW-Authenticate).
func (m *message) values(k string) []string {
	var out []string
	for _, h := range m.headers {
		if strings.EqualFold(h.k, k) {
			out = append(out, h.v)
		}
	}
	return out
}

// set replaces the first header k in place, or appends it.
func (m *message) set(k, v string) {
	for i, h := range m.headers {
		if strings.EqualFold(h.k, k) {
			m.headers[i].v = v
			return
		}
	}
	m.headers = append(m.headers, header{k, v})
}

// del removes every header k.
func (m *message) del(k string) {
	out := m.headers[:0]
	for _, h := range m.headers {
		if !strings.EqualFold(h.k, k) {
			out = append(out, h)
		}
	}
	m.headers = out
}

// clone copies the header slice so edits don't affect the original (body is shared).
func (m *message) clone() *message {
	c := *m
	c.headers = append([]header(nil), m.headers...)
	return &c
}

func (m *message) isResponse() bool { return strings.HasPrefix(m.first, "RTSP/") }

// status returns the response status code, or 0 for requests/malformed lines.
func (m *message) status() int {
	f := strings.Fields(m.first)
	if len(f) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(f[1])
	return n
}

// writeTo serializes the message in a single Write.
func (m *message) writeTo(w io.Writer) error {
	var b strings.Builder
	b.WriteString(m.first + "\r\n")
	for _, h := range m.headers {
		b.WriteString(h.k + ": " + h.v + "\r\n")
	}
	b.WriteString("\r\n")
	b.Write(m.body)
	_, err := io.WriteString(w, b.String())
	return err
}
