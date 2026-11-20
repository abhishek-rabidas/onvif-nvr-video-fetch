package onvif

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const envHead = `<?xml version="1.0" encoding="UTF-8"?><env:Envelope xmlns:env="http://www.w3.org/2003/05/soap-envelope" xmlns:tds="http://www.onvif.org/ver10/device/wsdl" xmlns:trc="http://www.onvif.org/ver10/recording/wsdl" xmlns:trp="http://www.onvif.org/ver10/replay/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema"><env:Body>`
const envTail = `</env:Body></env:Envelope>`

func TestClientAgainstFakeNVR(t *testing.T) {
	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		reply := func(s string) { io.WriteString(w, envHead+s+envTail) }
		switch {
		case strings.Contains(body, "GetSystemDateAndTime"):
			reply(`<tds:GetSystemDateAndTimeResponse><tds:SystemDateAndTime><tt:TimeZone><tt:TZ>IST-5:30</tt:TZ></tt:TimeZone>
<tt:UTCDateTime><tt:Time><tt:Hour>4</tt:Hour><tt:Minute>0</tt:Minute><tt:Second>0</tt:Second></tt:Time><tt:Date><tt:Year>2026</tt:Year><tt:Month>11</tt:Month><tt:Day>18</tt:Day></tt:Date></tt:UTCDateTime>
<tt:LocalDateTime><tt:Time><tt:Hour>9</tt:Hour><tt:Minute>30</tt:Minute><tt:Second>1</tt:Second></tt:Time><tt:Date><tt:Year>2026</tt:Year><tt:Month>11</tt:Month><tt:Day>18</tt:Day></tt:Date></tt:LocalDateTime>
</tds:SystemDateAndTime></tds:GetSystemDateAndTimeResponse>`)
		case r.Header.Get("Authorization") == "":
			w.Header().Set("WWW-Authenticate", `Digest realm="nvr", nonce="n1", qop="auth"`)
			w.WriteHeader(http.StatusUnauthorized)
		case strings.Contains(body, "GetServices"):
			reply(`<tds:GetServicesResponse>
<tds:Service><tds:Namespace>http://www.onvif.org/ver10/recording/wsdl</tds:Namespace><tds:XAddr>http://192.168.1.250/onvif/Recording</tds:XAddr></tds:Service>
<tds:Service><tds:Namespace>http://www.onvif.org/ver10/replay/wsdl</tds:Namespace><tds:XAddr>http://192.168.1.250/onvif/Replay</tds:XAddr></tds:Service>
</tds:GetServicesResponse>`)
		case strings.Contains(body, "GetRecordings") && r.URL.Path == "/onvif/Recording":
			reply(`<trc:GetRecordingsResponse>
<trc:RecordingItem><tt:RecordingToken>rec1</tt:RecordingToken><tt:Configuration><tt:Source><tt:SourceId>vs1</tt:SourceId><tt:Name>Gate</tt:Name></tt:Source></tt:Configuration></trc:RecordingItem>
<trc:RecordingItem><tt:RecordingToken>rec2</tt:RecordingToken><tt:Configuration><tt:Source><tt:SourceId>vs2</tt:SourceId><tt:Name>Dock</tt:Name></tt:Source></tt:Configuration></trc:RecordingItem>
</trc:GetRecordingsResponse>`)
		case strings.Contains(body, "GetReplayUri") && strings.Contains(body, "<RecordingToken>rec2</RecordingToken>"):
			reply(`<trp:GetReplayUriResponse><trp:Uri>rtsp://192.168.1.250:554/onvif/replay/2?a=1&amp;b=2</trp:Uri></trp:GetReplayUriResponse>`)
		default:
			w.WriteHeader(http.StatusBadRequest)
			reply(`<env:Fault><env:Code><env:Value>env:Sender</env:Value></env:Code><env:Reason><env:Text xml:lang="en">unexpected</env:Text></env:Reason></env:Fault>`)
		}
	}))
	defer srv.Close()
	host, portS, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portS)
	base = srv.URL

	ctx := context.Background()
	c := New(host, port, "admin", "pw")
	off, _, ok, err := c.SyncTime(ctx)
	if err != nil || !ok || off != 330*time.Minute {
		t.Fatalf("SyncTime: off=%v ok=%v err=%v", off, ok, err)
	}
	if err := c.DiscoverServices(ctx); err != nil {
		t.Fatal(err)
	}
	if c.ReplayURL != base+"/onvif/Replay" {
		t.Fatalf("ReplayURL not retargeted: %s", c.ReplayURL)
	}
	recs, err := c.Recordings(ctx)
	if err != nil || len(recs) != 2 || recs[1].Token != "rec2" || recs[1].SourceName != "Dock" {
		t.Fatalf("Recordings: %+v %v", recs, err)
	}
	uri, err := c.ReplayURI(ctx, "rec2")
	if err != nil || uri != "rtsp://192.168.1.250:554/onvif/replay/2?a=1&b=2" {
		t.Fatalf("ReplayURI: %q %v", uri, err)
	}
	if _, err := c.ReplayURI(ctx, "nope"); err == nil || !strings.Contains(err.Error(), "unexpected") {
		t.Fatalf("expected SOAP fault, got %v", err)
	}
}
