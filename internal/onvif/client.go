// Package onvif is a minimal ONVIF SOAP client covering Profile G replay
// (device, recording, search and replay services).
package onvif

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"com.flipkart.nvrvms/internal/digest"
)

const (
	nsDevice    = "http://www.onvif.org/ver10/device/wsdl"
	nsRecording = "http://www.onvif.org/ver10/recording/wsdl"
	nsSearch    = "http://www.onvif.org/ver10/search/wsdl"
	nsReplay    = "http://www.onvif.org/ver10/replay/wsdl"
	nsSchema    = "http://www.onvif.org/ver10/schema"
)

// Client talks to one NVR. Call SyncTime, then DiscoverServices, before other methods.
type Client struct {
	host string
	port int
	user string
	pass string

	httpc    *http.Client
	httpAuth *digest.Auth
	skew     time.Duration // device UTC clock - local clock, used for WS-Security Created

	// Service endpoints. DeviceURL is the standard ONVIF path; the rest come from DiscoverServices.
	DeviceURL, RecordingURL, SearchURL, ReplayURL string
}

// New returns a client for the NVR's ONVIF HTTP endpoint at host:port.
func New(host string, port int, user, pass string) *Client {
	return &Client{
		host: host, port: port, user: user, pass: pass,
		httpc: &http.Client{
			Timeout: 20 * time.Second,
			// NVRs almost always serve self-signed certs on HTTPS XAddrs.
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		},
		httpAuth:  &digest.Auth{User: user, Pass: pass},
		DeviceURL: fmt.Sprintf("http://%s/onvif/device_service", net.JoinHostPort(host, strconv.Itoa(port))),
	}
}

// Recording is one NVR recording (normally one per camera channel).
type Recording struct {
	Token       string
	SourceID    string
	SourceName  string
	Description string
}

// ---------------------------------------------------------------- SOAP plumbing

// envelope wraps body in a SOAP 1.2 envelope. If secure, adds a WS-Security UsernameToken
// with PasswordDigest = Base64(SHA1(nonce + created + password)); created uses the device
// clock (local time + skew) because NVRs reject tokens outside a small time window.
func (c *Client) envelope(body string, secure bool) string {
	var hdr string
	if secure {
		nonce := make([]byte, 16)
		_, _ = rand.Read(nonce)
		created := time.Now().Add(c.skew).UTC().Format("2006-01-02T15:04:05.000Z")
		sum := sha1.Sum(append(append(nonce, created...), c.pass...))
		hdr = `<s:Header><Security s:mustUnderstand="1" xmlns="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd">` +
			`<UsernameToken><Username>` + esc(c.user) + `</Username>` +
			`<Password Type="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordDigest">` + base64.StdEncoding.EncodeToString(sum[:]) + `</Password>` +
			`<Nonce EncodingType="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-soap-message-security-1.0#Base64Binary">` + base64.StdEncoding.EncodeToString(nonce) + `</Nonce>` +
			`<Created xmlns="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd">` + created + `</Created>` +
			`</UsernameToken></Security></s:Header>`
	}
	return `<?xml version="1.0" encoding="UTF-8"?><s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">` +
		hdr + `<s:Body>` + body + `</s:Body></s:Envelope>`
}

// call posts a SOAP request; WS-Security is always sent when secure, HTTP Digest/Basic is added on 401.
func (c *Client) call(ctx context.Context, endpoint, body string, secure bool, out any) error {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(c.envelope(body, secure)))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/soap+xml; charset=utf-8")
		if h := c.httpAuth.Authorization(http.MethodPost, req.URL.RequestURI()); h != "" {
			req.Header.Set("Authorization", h)
		}
		resp, err := c.httpc.Do(req)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if err != nil {
			return err
		}
		// On 401: retry (at most twice) with HTTP auth, and WS-Security too in case the
		// call was unauthenticated. The challenge is kept for later calls.
		if resp.StatusCode == http.StatusUnauthorized && attempt < 2 {
			if ch, err := digest.ParseBest(resp.Header.Values("WWW-Authenticate")); err == nil {
				c.httpAuth.SetChallenge(ch)
				secure = true
				continue
			}
		}
		// Decode even on non-200: SOAP faults usually come with HTTP 400/500.
		fault, err := decode(data, out)
		switch {
		case fault != nil:
			return fault
		case err != nil:
			return fmt.Errorf("HTTP %d from %s: %w", resp.StatusCode, endpoint, err)
		case resp.StatusCode != http.StatusOK:
			return fmt.Errorf("HTTP %d from %s", resp.StatusCode, endpoint)
		}
		return nil
	}
}

// soapFault covers both SOAP 1.2 (Code/Reason) and SOAP 1.1 (faultstring) faults.
type soapFault struct {
	Code struct {
		Value   string `xml:"Value"`
		Subcode struct {
			Value string `xml:"Value"`
		} `xml:"Subcode"`
	} `xml:"Code"`
	Reason      string `xml:"Reason>Text"`
	FaultString string `xml:"faultstring"` // SOAP 1.1
}

func (f *soapFault) Error() string {
	msg := strings.TrimSpace(f.Reason + f.FaultString)
	return fmt.Sprintf("SOAP fault %s %s: %s", f.Code.Value, f.Code.Subcode.Value, msg)
}

// decode decodes the first element inside soap:Body into out (or returns it as a fault).
// Elements are matched by local name only, since vendors use arbitrary namespace prefixes.
func decode(data []byte, out any) (*soapFault, error) {
	d := xml.NewDecoder(bytes.NewReader(data))
	// encoding/xml rejects non-UTF-8 charset declarations; treat the bytes as-is.
	d.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	inBody := false
	for {
		tok, err := d.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, errors.New("no SOAP body in response")
			}
			return nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if !inBody {
			inBody = se.Name.Local == "Body"
			continue
		}
		if se.Name.Local == "Fault" {
			var f soapFault
			if err := d.DecodeElement(&f, &se); err != nil {
				return nil, err
			}
			return &f, nil
		}
		if out == nil {
			return nil, nil
		}
		return nil, d.DecodeElement(out, &se)
	}
}

// esc XML-escapes s for embedding in a request body.
func esc(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// ---------------------------------------------------------------- device service

// onvifDateTime is the tt:DateTime structure (separate Date and Time elements).
type onvifDateTime struct {
	Date struct{ Year, Month, Day int }
	Time struct{ Hour, Minute, Second int }
}

// utc returns the fields as-is in a UTC time.Time (no zone conversion), or zero if absent.
func (d onvifDateTime) utc() time.Time {
	if d.Date.Year == 0 {
		return time.Time{}
	}
	return time.Date(d.Date.Year, time.Month(d.Date.Month), d.Date.Day, d.Time.Hour, d.Time.Minute, d.Time.Second, 0, time.UTC)
}

// SyncTime reads the device clock (unauthenticated) to fix WS-Security clock skew and
// returns the NVR's UTC offset (ok=false if the NVR does not report local time).
func (c *Client) SyncTime(ctx context.Context) (offset time.Duration, tz string, ok bool, err error) {
	var r struct {
		SystemDateAndTime struct {
			TimeZone      struct{ TZ string }
			UTCDateTime   onvifDateTime
			LocalDateTime onvifDateTime
		}
	}
	if err = c.call(ctx, c.DeviceURL, `<GetSystemDateAndTime xmlns="`+nsDevice+`"/>`, false, &r); err != nil {
		return 0, "", false, fmt.Errorf("GetSystemDateAndTime: %w", err)
	}
	sdt := r.SystemDateAndTime
	utc, local := sdt.UTCDateTime.utc(), sdt.LocalDateTime.utc()
	if utc.IsZero() {
		return 0, sdt.TimeZone.TZ, false, nil
	}
	c.skew = time.Until(utc)
	if local.IsZero() {
		return 0, sdt.TimeZone.TZ, false, nil
	}
	// Offset = local − UTC wall clock. The POSIX TZ string isn't parsed (formats vary by
	// vendor); rounding to 15 min absorbs seconds ticking between the two readings.
	return local.Sub(utc).Round(15 * time.Minute), sdt.TimeZone.TZ, true, nil
}

// DiscoverServices fills the recording/search/replay endpoints (GetServices, falling back to GetCapabilities).
func (c *Client) DiscoverServices(ctx context.Context) error {
	var svc struct {
		Service []struct{ Namespace, XAddr string }
	}
	err := c.call(ctx, c.DeviceURL, `<GetServices xmlns="`+nsDevice+`"><IncludeCapability>false</IncludeCapability></GetServices>`, true, &svc)
	if err == nil {
		for _, s := range svc.Service {
			switch strings.TrimSpace(s.Namespace) {
			case nsRecording:
				c.RecordingURL = c.fixXAddr(s.XAddr)
			case nsSearch:
				c.SearchURL = c.fixXAddr(s.XAddr)
			case nsReplay:
				c.ReplayURL = c.fixXAddr(s.XAddr)
			}
		}
	}
	// GetCapabilities: older (pre-2.x) devices, or GetServices failed/omitted Replay.
	if c.ReplayURL == "" {
		var capab struct {
			Ext struct {
				Recording, Search, Replay struct{ XAddr string }
			} `xml:"Capabilities>Extension"`
		}
		if err2 := c.call(ctx, c.DeviceURL, `<GetCapabilities xmlns="`+nsDevice+`"><Category>All</Category></GetCapabilities>`, true, &capab); err2 != nil {
			return fmt.Errorf("service discovery failed: GetServices: %v; GetCapabilities: %w", err, err2)
		}
		c.RecordingURL = firstNonEmpty(c.RecordingURL, c.fixXAddr(capab.Ext.Recording.XAddr))
		c.SearchURL = firstNonEmpty(c.SearchURL, c.fixXAddr(capab.Ext.Search.XAddr))
		c.ReplayURL = c.fixXAddr(capab.Ext.Replay.XAddr)
	}
	if c.ReplayURL == "" {
		return errors.New("device does not expose the ONVIF Replay service (Profile G not supported)")
	}
	if c.RecordingURL == "" && c.SearchURL == "" {
		return errors.New("device exposes neither ONVIF Recording nor Search service")
	}
	return nil
}

// fixXAddr points service URLs at the address the user reached the NVR on
// (NVRs often advertise an internal IP/port when accessed through NAT).
// HTTPS XAddrs are left unchanged: the user-supplied port is the HTTP one.
func (c *Client) fixXAddr(xaddr string) string {
	xaddr = strings.TrimSpace(xaddr)
	u, err := url.Parse(xaddr)
	if err != nil || xaddr == "" {
		return ""
	}
	if u.Scheme == "http" {
		u.Host = net.JoinHostPort(c.host, strconv.Itoa(c.port))
	}
	return u.String()
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ---------------------------------------------------------------- recordings

// Recordings lists recordings in device order (Recording service, falling back to Search service).
func (c *Client) Recordings(ctx context.Context) ([]Recording, error) {
	var errRec error
	if c.RecordingURL != "" {
		var r struct {
			Items []struct {
				RecordingToken string
				Source         struct{ SourceId, Name, Description string } `xml:"Configuration>Source"`
			} `xml:"RecordingItem"`
		}
		if errRec = c.call(ctx, c.RecordingURL, `<GetRecordings xmlns="`+nsRecording+`"/>`, true, &r); errRec == nil && len(r.Items) > 0 {
			recs := make([]Recording, 0, len(r.Items))
			for _, it := range r.Items {
				recs = append(recs, Recording{Token: strings.TrimSpace(it.RecordingToken), SourceID: it.Source.SourceId, SourceName: it.Source.Name, Description: it.Source.Description})
			}
			return recs, nil
		}
	}
	if c.SearchURL == "" {
		return nil, fmt.Errorf("GetRecordings: %v", errRec)
	}
	return c.findRecordings(ctx)
}

// recordingInformation is tt:RecordingInformation from the Search service.
type recordingInformation struct {
	RecordingToken    string
	Source            struct{ SourceId, Name, Description string }
	EarliestRecording string
	LatestRecording   string
}

// findRecordings lists recordings via an asynchronous Search session:
// FindRecordings → poll GetRecordingSearchResults until Completed → EndSearch.
func (c *Client) findRecordings(ctx context.Context) ([]Recording, error) {
	var fr struct{ SearchToken string }
	if err := c.call(ctx, c.SearchURL, `<FindRecordings xmlns="`+nsSearch+`"><Scope/><KeepAliveTime>PT30S</KeepAliveTime></FindRecordings>`, true, &fr); err != nil {
		return nil, fmt.Errorf("FindRecordings: %w", err)
	}
	tok := esc(strings.TrimSpace(fr.SearchToken))
	// Release the search session even if ctx was cancelled (NVRs allow only a few at once).
	defer c.call(context.WithoutCancel(ctx), c.SearchURL, `<EndSearch xmlns="`+nsSearch+`"><SearchToken>`+tok+`</SearchToken></EndSearch>`, true, nil)

	var recs []Recording
	seen := map[string]bool{} // some NVRs repeat results across polls
	for i := 0; i < 20; i++ { // bounded: some NVRs never report Completed
		var r struct {
			State string                 `xml:"ResultList>SearchState"`
			Info  []recordingInformation `xml:"ResultList>RecordingInformation"`
		}
		body := `<GetRecordingSearchResults xmlns="` + nsSearch + `"><SearchToken>` + tok + `</SearchToken><MaxResults>256</MaxResults><WaitTime>PT5S</WaitTime></GetRecordingSearchResults>`
		if err := c.call(ctx, c.SearchURL, body, true, &r); err != nil {
			return nil, fmt.Errorf("GetRecordingSearchResults: %w", err)
		}
		for _, in := range r.Info {
			t := strings.TrimSpace(in.RecordingToken)
			if !seen[t] {
				seen[t] = true
				recs = append(recs, Recording{Token: t, SourceID: in.Source.SourceId, SourceName: in.Source.Name, Description: in.Source.Description})
			}
		}
		if strings.TrimSpace(r.State) == "Completed" {
			break
		}
	}
	if len(recs) == 0 {
		return nil, errors.New("NVR reported no recordings")
	}
	return recs, nil
}

// RecordingRange returns the earliest/latest footage time of a recording (Search service).
func (c *Client) RecordingRange(ctx context.Context, token string) (earliest, latest time.Time, err error) {
	if c.SearchURL == "" {
		return time.Time{}, time.Time{}, errors.New("no search service")
	}
	var r struct {
		Info recordingInformation `xml:"RecordingInformation"`
	}
	if err = c.call(ctx, c.SearchURL, `<GetRecordingInformation xmlns="`+nsSearch+`"><RecordingToken>`+esc(token)+`</RecordingToken></GetRecordingInformation>`, true, &r); err != nil {
		return
	}
	return parseXSDateTime(r.Info.EarliestRecording), parseXSDateTime(r.Info.LatestRecording), nil
}

// parseXSDateTime parses an xs:dateTime; values without a zone are taken as UTC.
// Returns zero on failure.
func parseXSDateTime(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// ---------------------------------------------------------------- replay

// ReplayURI returns the RTSP playback URI for a recording.
func (c *Client) ReplayURI(ctx context.Context, token string) (string, error) {
	var r struct{ Uri string }
	body := `<GetReplayUri xmlns="` + nsReplay + `"><StreamSetup>` +
		`<Stream xmlns="` + nsSchema + `">RTP-Unicast</Stream>` +
		`<Transport xmlns="` + nsSchema + `"><Protocol>RTSP</Protocol></Transport>` +
		`</StreamSetup><RecordingToken>` + esc(token) + `</RecordingToken></GetReplayUri>`
	if err := c.call(ctx, c.ReplayURL, body, true, &r); err != nil {
		return "", fmt.Errorf("GetReplayUri: %w", err)
	}
	if strings.TrimSpace(r.Uri) == "" {
		return "", errors.New("GetReplayUri returned an empty URI")
	}
	return strings.TrimSpace(r.Uri), nil
}
