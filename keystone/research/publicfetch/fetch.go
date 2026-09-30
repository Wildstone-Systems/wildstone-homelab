// Package publicfetch implements the dormant public HTTP transport of ADR-W047.
// Callers must authorize the current Person/Agent, egress provenance and task
// budget before Fetch. This package registers no tools and grants no authority.
// Configured internal search providers must use a separate transport.
package publicfetch

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	MaxDuration  = 30 * time.Second
	MaxBytes     = 5 << 20
	MaxRedirects = 5
)

// Static errors intentionally exclude URLs, DNS answers, upstream bodies and
// wrapped network errors, which may contain protected content.
var (
	ErrURL         = errors.New("invalid_input: invalid public URL")
	ErrDestination = errors.New("permission_denied: non-public destination")
	ErrTransport   = errors.New("provider_unavailable: public retrieval failed")
	ErrTimeout     = errors.New("timeout: public retrieval deadline exceeded")
	ErrCancelled   = errors.New("cancelled: public retrieval cancelled")
	ErrSize        = errors.New("quota_exceeded: page byte ceiling exceeded")
	ErrUnsupported = errors.New("unsupported: response representation")
	ErrRedirect    = errors.New("invalid_input: redirect limit exceeded")
	ErrRateLimited = errors.New("rate_limited: public upstream refused request")
)

// Page holds untrusted bytes for the separate extraction layer, never a tool
// response or workspace download. Headers/cookies are deliberately not exposed.
type Page struct {
	URL         string
	MediaType   string
	Body        []byte
	RetrievedAt time.Time
}

type resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

// Fetcher is safe for concurrent use. Its zero value uses the system resolver
// and a direct TCP dialer; test seams are private so callers cannot supply a
// proxy, cookie jar, TLS bypass, or arbitrary RoundTripper.
type Fetcher struct {
	resolver resolver
	dial     func(context.Context, string, string) (net.Conn, error)
}

// Fetch applies one deadline across DNS, every redirect, TLS and body decoding.
// It performs no retries. Every hop resolves afresh and pins a validated literal
// IP while preserving the URL hostname for Host and TLS certificate checks.
func (f Fetcher) Fetch(ctx context.Context, rawURL string) (Page, error) {
	ctx, cancel := context.WithTimeout(ctx, MaxDuration)
	defer cancel()
	u, err := parseURL(rawURL)
	if err != nil {
		return Page{}, err
	}
	for hop := 0; ; hop++ {
		if ctx.Err() != nil {
			return Page{}, networkError(ctx)
		}
		resp, err := f.request(ctx, u)
		if err != nil {
			return Page{}, err
		}
		if isRedirect(resp.StatusCode) {
			location := resp.Header.Get("Location")
			resp.Body.Close()
			if hop == MaxRedirects {
				return Page{}, ErrRedirect
			}
			if location == "" || len(location) > 2048 {
				return Page{}, ErrURL
			}
			rel, err := url.Parse(location)
			if err != nil {
				return Page{}, ErrURL
			}
			u, err = parseURL(u.ResolveReference(rel).String())
			if err != nil {
				return Page{}, err
			}
			continue
		}
		page, err := readPage(resp, u)
		resp.Body.Close()
		if ctx.Err() != nil {
			return Page{}, networkError(ctx)
		}
		return page, err
	}
}

func parseURL(raw string) (*url.URL, error) {
	if len(raw) == 0 || len(raw) > 2048 {
		return nil, ErrURL
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Opaque != "" || u.Hostname() == "" ||
		(u.Scheme != "http" && u.Scheme != "https") || strings.Contains(u.Hostname(), "%") {
		return nil, ErrURL
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, ErrURL
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return nil, ErrURL
	}
	u.Fragment = ""
	return u, nil
}

func (f Fetcher) request(ctx context.Context, u *url.URL) (*http.Response, error) {
	r := f.resolver
	if r == nil {
		r = net.DefaultResolver
	}
	var addresses []netip.Addr
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil {
		addresses = []netip.Addr{ip}
	} else {
		var err error
		addresses, err = r.LookupNetIP(ctx, "ip", u.Hostname())
		if err != nil {
			return nil, networkError(ctx)
		}
	}
	if len(addresses) == 0 {
		return nil, ErrDestination
	}
	// Refuse mixed public/private answers before any connection is attempted.
	for _, ip := range addresses {
		if !publicIP(ip) {
			return nil, ErrDestination
		}
	}
	if ctx.Err() != nil {
		return nil, networkError(ctx)
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	pinned := net.JoinHostPort(addresses[0].Unmap().String(), port)
	dial := f.dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: MaxDuration}).DialContext
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(_ context.Context, network, _ string) (net.Conn, error) {
			return dial(ctx, network, pinned)
		},
		DisableKeepAlives:      true,
		DisableCompression:     true,
		MaxResponseHeaderBytes: 64 << 10,
		TLSHandshakeTimeout:    MaxDuration,
		ResponseHeaderTimeout:  MaxDuration,
	}
	// Each hop gets a fresh direct transport: no shared pool, proxy environment,
	// client credentials, cookie jar, automatic redirect or implicit retry.
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, ErrURL
	}
	req.Header.Set("Accept", "text/html, text/markdown, text/plain")
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := client.Do(req)
	if err != nil {
		transport.CloseIdleConnections()
		return nil, networkError(ctx)
	}
	return resp, nil
}

func readPage(resp *http.Response, u *url.URL) (Page, error) {
	if resp.StatusCode == http.StatusTooManyRequests {
		return Page{}, ErrRateLimited
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Page{}, ErrTransport
	}
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || (media != "text/html" && media != "text/plain" && media != "text/markdown") {
		return Page{}, ErrUnsupported
	}
	if resp.ContentLength > MaxBytes {
		return Page{}, ErrSize
	}
	// Bound compressed bytes independently from decompressed bytes. The extra
	// byte distinguishes a complete response at the ceiling from truncation.
	wire := &io.LimitedReader{R: resp.Body, N: MaxBytes + 1}
	var reader io.Reader = wire
	switch strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))) {
	case "", "identity":
	case "gzip":
		gz, err := gzip.NewReader(wire)
		if err != nil {
			if wire.N == 0 {
				return Page{}, ErrSize
			}
			return Page{}, ErrTransport
		}
		defer gz.Close()
		reader = gz
	default:
		return Page{}, ErrUnsupported
	}
	body, err := io.ReadAll(io.LimitReader(reader, MaxBytes+1))
	if len(body) > MaxBytes || wire.N == 0 {
		return Page{}, ErrSize
	}
	if err != nil {
		return Page{}, ErrTransport
	}
	return Page{URL: u.String(), MediaType: media, Body: body, RetrievedAt: time.Now().UTC()}, nil
}

func networkError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ErrTimeout
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return ErrCancelled
	}
	return ErrTransport
}

func isRedirect(status int) bool {
	return status == 301 || status == 302 || status == 303 || status == 307 || status == 308
}

// Conservative special-use exclusions: globally unicast is insufficient (it
// includes private, documentation, benchmark, shared and reserved networks).
// IPv6 is restricted to global unicast 2000::/3 and excludes special-use blocks,
// including transition mechanisms that could embed a non-public IPv4 address.
var excluded = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"),
	// Azure host virtual address is not public egress despite its numeric range.
	netip.MustParsePrefix("168.63.129.16/32"),
	netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("224.0.0.0/3"),
	netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("3fff::/20"),
}

func publicIP(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Zone() != "" {
		return false
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return false
	}
	if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, prefix := range excluded {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}
