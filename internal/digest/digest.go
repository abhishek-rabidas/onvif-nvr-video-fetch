// Package digest implements HTTP/RTSP Basic and Digest (RFC 2617/7616) client auth.
package digest

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"strings"
	"sync"
)

// Challenge is a parsed WWW-Authenticate header.
type Challenge struct {
	Scheme string
	Params map[string]string
}

// ParseBest picks Digest over Basic from a list of WWW-Authenticate values.
func ParseBest(values []string) (*Challenge, error) {
	var basic *Challenge
	for _, v := range values {
		scheme, rest, _ := strings.Cut(strings.TrimSpace(v), " ")
		c := &Challenge{Scheme: scheme, Params: parseParams(rest)}
		switch {
		case strings.EqualFold(scheme, "Digest"):
			return c, nil
		case strings.EqualFold(scheme, "Basic") && basic == nil:
			basic = c
		}
	}
	if basic == nil {
		return nil, errors.New("no supported WWW-Authenticate challenge")
	}
	return basic, nil
}

func parseParams(s string) map[string]string {
	m := map[string]string{}
	for {
		s = strings.TrimLeft(s, " ,\t")
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			return m
		}
		key := strings.ToLower(strings.TrimSpace(s[:eq]))
		s = strings.TrimLeft(s[eq+1:], " \t")
		var val string
		if strings.HasPrefix(s, `"`) {
			var b strings.Builder
			i := 1
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				b.WriteByte(s[i])
			}
			val = b.String()
			s = s[min(i+1, len(s)):]
		} else {
			end := strings.IndexByte(s, ',')
			if end < 0 {
				end = len(s)
			}
			val, s = strings.TrimSpace(s[:end]), s[end:]
		}
		m[key] = val
	}
}

// Auth produces Authorization headers once a challenge has been set. Safe for concurrent use.
type Auth struct {
	User, Pass string

	mu     sync.Mutex
	ch     *Challenge
	nc     uint32
	cnonce func() string // test hook
}

func (a *Auth) SetChallenge(c *Challenge) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ch, a.nc = c, 0
}

// Authorization returns the header value for a request, or "" if no challenge was received yet.
func (a *Auth) Authorization(method, uri string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ch == nil {
		return ""
	}
	if strings.EqualFold(a.ch.Scheme, "Basic") {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(a.User+":"+a.Pass))
	}
	p := a.ch.Params
	algo := p["algorithm"]
	upper := strings.ToUpper(algo)
	newHash := md5.New
	if strings.HasPrefix(upper, "SHA-256") {
		newHash = sha256.New
	}
	h := func(s string) string { return hexHash(newHash, s) }

	cnonce := randomHex()
	if a.cnonce != nil {
		cnonce = a.cnonce()
	}
	realm, nonce := p["realm"], p["nonce"]
	ha1 := h(a.User + ":" + realm + ":" + a.Pass)
	if strings.HasSuffix(upper, "-SESS") {
		ha1 = h(ha1 + ":" + nonce + ":" + cnonce)
	}
	ha2 := h(method + ":" + uri)

	qop := ""
	for _, q := range strings.Split(p["qop"], ",") {
		if strings.TrimSpace(q) == "auth" {
			qop = "auth"
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, `Digest username="%s", realm="%s", nonce="%s", uri="%s"`, quote(a.User), quote(realm), quote(nonce), quote(uri))
	if qop != "" {
		a.nc++
		nc := fmt.Sprintf("%08x", a.nc)
		fmt.Fprintf(&b, `, response="%s", qop=auth, nc=%s, cnonce="%s"`, h(ha1+":"+nonce+":"+nc+":"+cnonce+":"+qop+":"+ha2), nc, cnonce)
	} else {
		fmt.Fprintf(&b, `, response="%s"`, h(ha1+":"+nonce+":"+ha2))
	}
	if algo != "" {
		b.WriteString(", algorithm=" + algo)
	}
	if op, ok := p["opaque"]; ok {
		fmt.Fprintf(&b, `, opaque="%s"`, quote(op))
	}
	return b.String()
}

func hexHash(newHash func() hash.Hash, s string) string {
	h := newHash()
	h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))
}

func randomHex() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func quote(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
}
