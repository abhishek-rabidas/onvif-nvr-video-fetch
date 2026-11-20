package digest

import (
	"strings"
	"testing"
)

// RFC 2617 §3.5 example.
func TestDigestRFC2617(t *testing.T) {
	ch, err := ParseBest([]string{
		`Basic realm="x"`,
		`Digest realm="testrealm@host.com", qop="auth,auth-int", nonce="dcd98b7102dd2f0e8b11d0f600bfb0c093", opaque="5ccc069c403ebaf9f0171e9517f40e41"`,
	})
	if err != nil {
		t.Fatal(err)
	}
	a := &Auth{User: "Mufasa", Pass: "Circle Of Life", cnonce: func() string { return "0a4f113b" }}
	a.SetChallenge(ch)
	got := a.Authorization("GET", "/dir/index.html")
	for _, want := range []string{`response="6629fae49393a05397450978507c4ef1"`, `nc=00000001`, `opaque="5ccc069c403ebaf9f0171e9517f40e41"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
}
