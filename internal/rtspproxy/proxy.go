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

// Config configures a Proxy for one replay session.
type Config struct {
	Upstream           string // rtsp://host[:port]/path?query (no userinfo)
	Username, Password string
	Start, End         time.Time     // replay range; sent to the NVR as UTC
	RateControl        bool          // false => ask NVR to stream faster than real time
	IdleTimeout        time.Duration // close when NVR sends nothing for this long
	Logf               func(format string, args ...any)
}

// Proxy is a local RTSP listener on 127.0.0.1; each client connection gets its own upstream connection.
type Proxy struct {
	cfg       Config
	ln        net.Listener
	dialAddr  string // host:port to dial (default RTSP port 554 if the URI has none)
	upBase    string // rtsp://host[:port] as in the upstream URI
	localBase string // rtsp://127.0.0.1:port
	path      string // path+query of the upstream URI, shared by local and upstream URLs

	mu    sync.Mutex
	conns map[net.Conn]struct{} // open client+upstream conns, closed by Close
	wg    sync.WaitGroup        // accept loop + per-connection handlers
}

// Start listens on a random loopback port and begins proxying to cfg.Upstream.
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
	// Split manually rather than url.Parse so the path/query is preserved byte-for-byte.
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

// Close stops accepting, closes all open connections (the client sees EOF and finishes
// cleanly) and waits for handlers to exit. Safe to call more than once.
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

// track registers (add) or forgets a connection so Close can interrupt it.
func (p *Proxy) track(c net.Conn, add bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if add {
		p.conns[c] = struct{}{}
	} else {
		delete(p.conns, c)
	}
}

// serve accepts client connections until the listener is closed.
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

// session is one client connection paired with its upstream NVR connection.
type session struct {
	p          *Proxy
	client, up net.Conn
	cr, ur     *bufio.Reader // buffered readers for client and upstream
	auth       *digest.Auth  // NVR credentials; the client's own Authorization is dropped
	offset     int           // upstream CSeq = client CSeq + offset (grows with each auth retry)
}

// handle proxies one client connection: negotiate (phase 1) then stream (phase 2).
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
// Returns nil once PLAY succeeds, io.EOF on TEARDOWN.
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

// roundTrip sends req upstream and returns the (CSeq-mapped) response. On 401 it sets the
// challenge and resends with the next CSeq (servers reject a reused CSeq), bumping offset
// so later client CSeqs stay ahead of upstream ones.
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

// rewriteRequest returns a copy of a client request adapted for the NVR: local → upstream URI,
// shifted CSeq, NVR auth, and on PLAY the ONVIF replay headers.
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
		// ONVIF Streaming spec: absolute UTC range, replay mode required, optional
		// Rate-Control: no for "as fast as possible".
		const layout = "20060102T150405Z"
		m.set("Range", "clock="+s.p.cfg.Start.UTC().Format(layout)+"-"+s.p.cfg.End.UTC().Format(layout))
		m.set("Require", "onvif-replay")
		if !s.p.cfg.RateControl {
			m.set("Rate-Control", "no")
		}
	}
	return m
}

// rewriteResponse maps an upstream CSeq back to the client's numbering.
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
	<-done // either direction ending ends the session
	s.client.Close()
	s.up.Close()
	<-done
}

// upToClient forwards NVR → client: media frames, responses (CSeq mapped back) and server
// requests. Ends on error or after IdleTimeout without data (NVRs often just stop sending
// at the end of the range instead of closing).
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
		// Batch writes: flush only when no more upstream data is already buffered.
		if err == nil && s.ur.Buffered() == 0 {
			err = w.Flush()
		}
		if err != nil {
			return
		}
	}
}

// clientToUp forwards client → NVR: RTCP frames as-is, requests (keep-alive, TEARDOWN) rewritten.
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
