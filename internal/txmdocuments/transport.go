package txmdocuments

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// These two configured origins are trusted internal endpoints. This dedicated
// transport does not alter the application's SSRF whitelist, follow redirects,
// accept cookies or consult environment proxies.
func originClient(origin *url.URL, base http.RoundTripper, paths map[string]bool) *http.Client {
	if base == nil {
		base = &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConns:          10,
			DisableCompression:    true,
			ForceAttemptHTTP2:     true,
		}
	}
	return &http.Client{
		Transport:     &originTransport{origin: origin, base: base, paths: paths},
		Timeout:       30 * time.Second,
		CheckRedirect: noRedirect,
	}
}

type originTransport struct {
	origin *url.URL
	base   http.RoundTripper
	paths  map[string]bool
}

func (t *originTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil || request.URL.User != nil || request.URL.Fragment != "" ||
		request.URL.RawQuery != "" ||
		request.URL.ForceQuery ||
		!sameOrigin(t.origin, request.URL) ||
		request.Host != "" && !strings.EqualFold(request.Host, request.URL.Host) {
		return nil, errors.New("document service request outside configured origin was blocked")
	}
	if request.Method != http.MethodPost || t.paths != nil && !t.paths[request.URL.Path] ||
		t.paths == nil && (request.URL.Path != t.origin.Path || request.URL.RawPath != t.origin.RawPath) {
		return nil, errors.New("document service request outside allowed RPCs was blocked")
	}
	return t.base.RoundTrip(request)
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}
