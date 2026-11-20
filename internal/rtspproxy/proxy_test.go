package rtspproxy

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// Scripted NVR: 401 on first request, then checks URI/CSeq/auth rewriting and ONVIF PLAY headers.
func TestProxyAuthAndReplayHeaders(t *testing.T) {
	nvr, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer nvr.Close()
	upBase := "rtsp://" + nvr.Addr().String()

	start := time.Date(2026, 11, 18, 4, 30, 0, 0, time.UTC)
	p, err := Start(Config{Upstream: upBase + "/onvif/replay/1?x=1", Username: "admin", Password: "pw",
		Start: start, End: start.Add(time.Hour), IdleTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	nvrErr := make(chan string, 1)
	go func() {
		c, err := nvr.Accept()
		if err != nil {
			nvrErr <- err.Error()
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		expect := func(cseq string, check func(*message) string) bool {
			_, m, err := readMessage(r)
			if err != nil {
				nvrErr <- err.Error()
				return false
			}
			if got := m.get("CSeq"); got != cseq {
				nvrErr <- "upstream CSeq " + got + ", want " + cseq
				return false
			}
			if msg := check(m); msg != "" {
				nvrErr <- msg
				return false
			}
			return true
		}
		if !expect("1", func(m *message) string {
			if m.first != "OPTIONS "+upBase+"/onvif/replay/1?x=1 RTSP/1.0" {
				return "bad request line: " + m.first
			}
			return ""
		}) {
			return
		}
		io.WriteString(c, "RTSP/1.0 401 Unauthorized\r\nCSeq: 1\r\nWWW-Authenticate: Digest realm=\"nvr\", nonce=\"abc\"\r\n\r\n")
		if !expect("2", func(m *message) string {
			if a := m.get("Authorization"); !strings.HasPrefix(a, "Digest ") || !strings.Contains(a, `uri="`+upBase+`/onvif/replay/1?x=1"`) {
				return "bad Authorization: " + a
			}
			return ""
		}) {
			return
		}
		io.WriteString(c, "RTSP/1.0 200 OK\r\nCSeq: 2\r\n\r\n")
		if !expect("3", func(m *message) string {
			if m.get("Range") != "clock=20261118T043000Z-20261118T053000Z" || m.get("Require") != "onvif-replay" || m.get("Rate-Control") != "no" {
				return "bad PLAY headers: " + m.get("Range") + " | " + m.get("Require") + " | " + m.get("Rate-Control")
			}
			return ""
		}) {
			return
		}
		io.WriteString(c, "RTSP/1.0 200 OK\r\nCSeq: 3\r\n\r\n$\x00\x00\x03abc")
		nvrErr <- ""
		io.Copy(io.Discard, r)
	}()

	cl, err := net.Dial("tcp", strings.TrimPrefix(p.localBase, "rtsp://"))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	cr := bufio.NewReader(cl)

	io.WriteString(cl, "OPTIONS "+p.URL()+" RTSP/1.0\r\nCSeq: 1\r\n\r\n")
	if _, m, err := readMessage(cr); err != nil || m.status() != 200 || m.get("CSeq") != "1" {
		t.Fatalf("OPTIONS reply: %v %+v", err, m)
	}
	io.WriteString(cl, "PLAY "+p.URL()+" RTSP/1.0\r\nCSeq: 2\r\nRange: npt=0.000-\r\n\r\n")
	if _, m, err := readMessage(cr); err != nil || m.status() != 200 || m.get("CSeq") != "2" {
		t.Fatalf("PLAY reply: %v %+v", err, m)
	}
	if frame, _, err := readMessage(cr); err != nil || string(frame) != "$\x00\x00\x03abc" {
		t.Fatalf("frame: %v %q", err, frame)
	}
	if msg := <-nvrErr; msg != "" {
		t.Fatal(msg)
	}
}
