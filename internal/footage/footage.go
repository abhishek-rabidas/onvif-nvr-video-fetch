// Package footage fetches NVR recordings over ONVIF Profile G and saves them as MP4 via ffmpeg.
package footage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"com.flipkart.nvrvms/internal/onvif"
	"com.flipkart.nvrvms/internal/rtspproxy"
)

// Request describes one footage fetch. List uses only the connection fields.
type Request struct {
	Host     string
	Port     int
	Username string
	Password string

	Channel        int    // 1-based; maps to the N-th recording reported by the NVR
	RecordingToken string // optional; overrides Channel

	Start, End string // local wall-clock time, e.g. "2026-11-18 10:00:00"
	TZ         string // "" = NVR's own timezone, "Local" = this machine, or IANA name

	Output string
	Fast   bool   // ask NVR to stream faster than real time (Rate-Control: no)
	FFmpeg string // ffmpeg binary, default "ffmpeg"
	Log    *log.Logger

	MaxDuration time.Duration // 0 = unlimited
	Stderr      io.Writer     // live ffmpeg output (with progress); nil = discard
}

// ErrInvalid marks errors caused by bad request parameters (vs. NVR/network failures).
var ErrInvalid = errors.New("invalid request")

// timeLayouts are the accepted Start/End formats, tried in order.
var timeLayouts = []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02 15:04"}

// parseLocal parses a wall-clock time string as a time in loc.
func parseLocal(s string, loc *time.Location) (time.Time, error) {
	for _, l := range timeLayouts {
		if t, err := time.ParseInLocation(l, strings.TrimSpace(s), loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%w: time %q (use YYYY-MM-DD HH:MM:SS)", ErrInvalid, s)
}

// connect opens an ONVIF session: syncs the NVR clock (needed for WS-Security),
// picks the timezone for Start/End, and discovers the Profile G service endpoints.
func connect(ctx context.Context, r *Request) (*onvif.Client, *time.Location, error) {
	c := onvif.New(r.Host, r.Port, r.Username, r.Password)
	offset, tz, ok, err := c.SyncTime(ctx)
	if err != nil {
		return nil, nil, err
	}
	// Timezone precedence: explicit TZ > NVR's reported UTC offset > this machine's zone.
	var loc *time.Location
	switch {
	case r.TZ != "":
		if loc, err = time.LoadLocation(r.TZ); err != nil {
			return nil, nil, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
	case ok:
		loc = time.FixedZone("NVR", int(offset.Seconds()))
	default:
		r.Log.Printf("warning: NVR did not report local time (TZ=%q); interpreting timestamps in this machine's timezone", tz)
		loc = time.Local
	}
	if err := c.DiscoverServices(ctx); err != nil {
		return nil, nil, err
	}
	return c, loc, nil
}

// List returns the NVR's recordings; index+1 is the channel number used by Fetch.
func List(ctx context.Context, r Request) ([]onvif.Recording, error) {
	if r.Log == nil {
		r.Log = log.Default()
	}
	c, _, err := connect(ctx, &r)
	if err != nil {
		return nil, err
	}
	return c.Recordings(ctx)
}

// Fetch downloads r.Start..r.End of one recording into r.Output (MP4):
// ONVIF discovery → resolve channel to recording token → GetReplayUri →
// local RTSP proxy (adds ONVIF replay headers) → ffmpeg remux.
// Errors caused by bad parameters wrap ErrInvalid.
func Fetch(ctx context.Context, r Request) error {
	if r.Log == nil {
		r.Log = log.Default()
	}
	if r.FFmpeg == "" {
		r.FFmpeg = "ffmpeg"
	}
	ffmpeg, err := exec.LookPath(r.FFmpeg)
	if err != nil {
		return fmt.Errorf("ffmpeg not found: %w", err)
	}

	c, loc, err := connect(ctx, &r)
	if err != nil {
		return err
	}
	start, err := parseLocal(r.Start, loc)
	if err != nil {
		return err
	}
	end, err := parseLocal(r.End, loc)
	if err != nil {
		return err
	}
	if !end.After(start) {
		return fmt.Errorf("%w: end must be after start", ErrInvalid)
	}
	if r.MaxDuration > 0 && end.Sub(start) > r.MaxDuration {
		return fmt.Errorf("%w: range %s exceeds max %s", ErrInvalid, end.Sub(start), r.MaxDuration)
	}
	r.Log.Printf("range %s → %s (UTC %s → %s)", start.Format(time.RFC3339), end.Format(time.RFC3339),
		start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339))

	token := r.RecordingToken
	if token == "" {
		recs, err := c.Recordings(ctx)
		if err != nil {
			return err
		}
		if r.Channel < 1 || r.Channel > len(recs) {
			return fmt.Errorf("%w: channel %d out of range, NVR has %d recordings", ErrInvalid, r.Channel, len(recs))
		}
		token = recs[r.Channel-1].Token
		r.Log.Printf("channel %d → recording %q (source %q)", r.Channel, token, recs[r.Channel-1].SourceName)
	}

	// Sanity check only: warn (don't fail) since NVRs don't all report ranges reliably.
	if earliest, latest, err := c.RecordingRange(ctx, token); err == nil && !earliest.IsZero() {
		if end.Before(earliest) || (!latest.IsZero() && start.After(latest)) {
			r.Log.Printf("warning: requested range is outside available footage (%s → %s)", earliest.In(loc).Format(time.RFC3339), latest.In(loc).Format(time.RFC3339))
		}
	}

	uri, err := c.ReplayURI(ctx, token)
	if err != nil {
		return err
	}
	uri, err = retarget(uri, r.Host)
	if err != nil {
		return err
	}
	r.Log.Printf("replay URI %s", uri)

	proxy, err := rtspproxy.Start(rtspproxy.Config{
		Upstream: uri, Username: r.Username, Password: r.Password,
		Start: start, End: end, RateControl: !r.Fast, Logf: r.Log.Printf,
	})
	if err != nil {
		return err
	}
	defer proxy.Close()

	dur := end.Sub(start)
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "warning", "-y"}
	if r.Stderr != nil {
		args = append(args, "-stats")
	}
	cmd := exec.CommandContext(ctx, ffmpeg, append(args,
		// TCP interleaved transport (the proxy only relays the RTSP connection, not UDP).
		// -timeout is the socket I/O timeout in µs (30s) in ffmpeg ≥5.
		"-rtsp_transport", "tcp", "-timeout", "30000000",
		"-i", proxy.URL(),
		// Cap output length: some NVRs keep streaming past the requested end.
		"-t", fmt.Sprintf("%.3f", dur.Seconds()),
		// First video stream; first audio stream only if present ("?").
		"-map", "0:v:0", "-map", "0:a:0?",
		"-c:v", "copy", "-c:a", "aac", // NVR audio is usually G.711, which MP4 cannot carry
		"-avoid_negative_ts", "make_zero", "-movflags", "+faststart",
		r.Output)...)
	// Keep the end of ffmpeg's stderr so failures carry its actual error message.
	tail := &tailBuffer{max: 4 << 10}
	cmd.Stderr = tail
	if r.Stderr != nil {
		cmd.Stderr = io.MultiWriter(tail, r.Stderr)
	}
	// On cancel, end the stream instead of signalling (os.Interrupt is unsupported on Windows):
	// ffmpeg sees EOF and finalizes the MP4. WaitDelay kills it if it doesn't exit.
	cmd.Cancel = func() error { proxy.Close(); return nil }
	cmd.WaitDelay = 15 * time.Second

	mode := "real time"
	if r.Fast {
		mode = "fast replay"
	}
	r.Log.Printf("recording %s of footage (%s) → %s", dur, mode, r.Output)
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("ffmpeg: %w: %s", err, tail.lastLines(3))
	}
	// ffmpeg can exit 0 without writing anything if the NVR sent no media.
	if fi, err := os.Stat(r.Output); err != nil || fi.Size() == 0 {
		return errors.New("ffmpeg produced no output (no footage for this range?)")
	}
	return nil
}

// retarget points the replay URI at the user-supplied NVR host (keeping the RTSP port)
// and strips any embedded credentials.
func retarget(uri, host string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("bad replay URI %q: %w", uri, err)
	}
	u.User = nil
	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else {
		u.Host = host
	}
	return u.String(), nil
}

// tailBuffer keeps the last max bytes written (ffmpeg stderr, for error messages).
type tailBuffer struct {
	b   []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}

// lastLines returns the last n non-empty lines joined by " | " (ffmpeg progress uses \r).
func (t *tailBuffer) lastLines(n int) string {
	lines := strings.FieldsFunc(string(t.b), func(r rune) bool { return r == '\n' || r == '\r' })
	return strings.Join(lines[max(0, len(lines)-n):], " | ")
}
