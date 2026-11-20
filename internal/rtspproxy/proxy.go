// Package rtspproxy runs a local RTSP proxy in front of an NVR replay URI so a stock
// RTSP client (ffmpeg) can play ONVIF recordings: it injects the ONVIF replay headers
// (Range: clock=..., Require: onvif-replay, Rate-Control) into PLAY and handles NVR auth.
package rtspproxy

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"com.flipkart.nvrvms/internal/digest"
)

type Config struct {
	Upstream           string // rtsp://host[:port]/path?query (no userinfo)
	Username, Password string
	Start, End         time.Time
	RateControl        bool          // false => ask NVR to stream faster than real time
	IdleTimeout        time.Duration // close when NVR sends nothing for this long
	Logf               func(format string, args ...any)
}

type Proxy struct {
	cfg       Config
	ln        net.Listener
	dialAddr  string
	upBase    string // rtsp://host[:port] as in the upstream URI
	localBase string // rtsp://127.0.0.1:port
	path      string

	mu    sync.Mutex
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
}

func Start(cfg Config) (*Proxy, error) {
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 20 * time.Second
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if !strings.HasPrefix(strings.ToLower(cfg.Upstream), "rtsp://") {
		return nil, fmt.Errorf("unsupported replay URI %q", cfg.Upstream)
	}
	after := cfg.Upstream[len("rtsp://"):]
	hostPart, path := after, "/"
	if i := strings.IndexAny(after, "/?"); i >= 0 {
		hostPart, path = after[:i], after[i:]
	}
	dial := hostPart
	if _, _, err := net.SplitHostPort(hostPart); err != nil {
		dial = net.JoinHostPort(strings.Trim(hostPart, "[]"), "554")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &Proxy{
		cfg: cfg, ln: ln, dialAddr: dial, path: path,
		upBase:    "rtsp://" + hostPart,
		localBase: "rtsp://" + ln.Addr().String(),
		conns:     map[net.Conn]struct{}{},
	}
	p.wg.Add(1)
	go p.serve()
	return p, nil
}

// URL is the local URL to hand to the RTSP client.
func (p *Proxy) URL() string { return p.localBase + p.path }

func (p *Proxy) Close() error {
	err := p.ln.Close()
	p.mu.Lock()
	for c := range p.conns {
		c.Close()
	}
	p.mu.Unlock()
	p.wg.Wait()
	return err
}

func (p *Proxy) track(c net.Conn, add bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if add {
		p.conns[c] = struct{}{}
	} else {
		delete(p.conns, c)
	}
}

func (p *Proxy) serve() {
	defer p.wg.Done()
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.handle(c)
		}()
	}
}

type session struct {
	p          *Proxy
	client, up net.Conn
	cr, ur     *bufio.Reader
	auth       *digest.Auth
	offset     int // upstream CSeq = client CSeq + offset (grows with each auth retry)
}

func (p *Proxy) handle(client net.Conn) {
	p.track(client, true)
	defer func() { client.Close(); p.track(client, false) }()

	up, err := net.DialTimeout("tcp", p.dialAddr, 10*time.Second)
	if err != nil {
		p.cfg.Logf("rtsp: connect %s: %v", p.dialAddr, err)
		return
	}
	p.track(up, true)
	defer func() { up.Close(); p.track(up, false) }()

	s := &session{
		p: p, client: client, up: up,
		cr: bufio.NewReaderSize(client, 64<<10), ur: bufio.NewReaderSize(up, 256<<10),
		auth: &digest.Auth{User: p.cfg.Username, Pass: p.cfg.Password},
	}
	if err := s.negotiate(); err != nil {
		if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			p.cfg.Logf("rtsp: %v", err)
		}
		return
	}
	s.stream()
}

// negotiate relays the strictly sequential OPTIONS/DESCRIBE/SETUP/PLAY exchange.
func (s *session) negotiate() error {
	for {
		frame, req, err := readMessage(s.cr)
		if err != nil {
			return err
		}
		if frame != nil {
			if _, err := s.up.Write(frame); err != nil {
				return err
			}
			continue
		}
		resp, err := s.roundTrip(req)
		if err != nil {
			return err
		}
		if err := resp.writeTo(s.client); err != nil {
			return err
		}
		switch method, _, _ := strings.Cut(req.first, " "); method {
		case "TEARDOWN":
			return io.EOF
		case "PLAY":
			if st := resp.status(); st < 200 || st > 299 {
				return fmt.Errorf("NVR rejected PLAY: %s", resp.first)
			}
			return nil
		}
	}
}

func (s *session) roundTrip(req *message) (*message, error) {
	for attempt := 0; ; attempt++ {
		if err := s.rewriteRequest(req).writeTo(s.up); err != nil {
			return nil, err
		}
		resp, err := s.readResponse()
		if err != nil {
			return nil, err
		}
		if resp.status() == 401 && attempt < 2 {
			if ch, err := digest.ParseBest(resp.values("WWW-Authenticate")); err == nil {
				s.auth.SetChallenge(ch)
				s.offset++
				continue
			}
		}
		s.rewriteResponse(resp)
		return resp, nil
	}
}

// readResponse returns the next RTSP response, passing through any interleaved data or server requests.
func (s *session) readResponse() (*message, error) {
	for {
		s.up.SetReadDeadline(time.Now().Add(s.p.cfg.IdleTimeout))
		frame, m, err := readMessage(s.ur)
		if err != nil {
			return nil, fmt.Errorf("reading NVR response: %w", err)
		}
		if frame == nil && m.isResponse() {
			return m, nil
		}
		if frame != nil {
			_, err = s.client.Write(frame)
		} else {
			err = m.writeTo(s.client)
		}
		if err != nil {
			return nil, err
		}
	}
}

func (s *session) rewriteRequest(req *message) *message {
	m := req.clone()
	method, rest, _ := strings.Cut(m.first, " ")
	uri, proto, _ := strings.Cut(rest, " ")
	if strings.HasPrefix(uri, s.p.localBase) {
		uri = s.p.upBase + uri[len(s.p.localBase):]
	}
	m.first = method + " " + uri + " " + proto
	if n, err := strconv.Atoi(m.get("CSeq")); err == nil {
		m.set("CSeq", strconv.Itoa(n+s.offset))
	}
	m.del("Authorization")
	if h := s.auth.Authorization(method, uri); h != "" {
		m.set("Authorization", h)
	}
	if method == "PLAY" {
		const layout = "20060102T150405Z"
		m.set("Range", "clock="+s.p.cfg.Start.UTC().Format(layout)+"-"+s.p.cfg.End.UTC().Format(layout))
		m.set("Require", "onvif-replay")
		if !s.p.cfg.RateControl {
			m.set("Rate-Control", "no")
		}
	}
	return m
}

func (s *session) rewriteResponse(m *message) {
	if n, err := strconv.Atoi(m.get("CSeq")); err == nil {
		m.set("CSeq", strconv.Itoa(n-s.offset))
	}
}

// stream pipes media both ways after PLAY, still rewriting RTSP messages (keep-alives, TEARDOWN).
func (s *session) stream() {
	done := make(chan struct{}, 2)
	go func() { s.upToClient(); done <- struct{}{} }()
	go func() { s.clientToUp(); done <- struct{}{} }()
	<-done
	s.client.Close()
	s.up.Close()
	<-done
}

func (s *session) upToClient() {
	w := bufio.NewWriterSize(s.client, 256<<10)
	defer w.Flush()
	for {
		s.up.SetReadDeadline(time.Now().Add(s.p.cfg.IdleTimeout))
		frame, m, err := readMessage(s.ur)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				s.p.cfg.Logf("rtsp: no data from NVR for %s, closing stream", s.p.cfg.IdleTimeout)
			}
			return
		}
		if frame != nil {
			_, err = w.Write(frame)
		} else {
			if m.isResponse() {
				if m.status() == 401 { // e.g. stale nonce on a keep-alive
					if ch, err := digest.ParseBest(m.values("WWW-Authenticate")); err == nil {
						s.auth.SetChallenge(ch)
					}
				}
				s.rewriteResponse(m)
			}
			err = m.writeTo(w)
		}
		if err == nil && s.ur.Buffered() == 0 {
			err = w.Flush()
		}
		if err != nil {
			return
		}
	}
}

func (s *session) clientToUp() {
	for {
		frame, m, err := readMessage(s.cr)
		if err != nil {
			return
		}
		if frame != nil {
			_, err = s.up.Write(frame)
		} else {
			err = s.rewriteRequest(m).writeTo(s.up)
		}
		if err != nil {
			return
		}
	}
}
