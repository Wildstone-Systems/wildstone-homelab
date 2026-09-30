package publicfetch

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

type resolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (f resolverFunc) LookupNetIP(c context.Context, n, h string) ([]netip.Addr, error) {
	return f(c, n, h)
}
func publicResolver(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
}

// All upstreams are in-memory pipes. No fixture dials its asserted public IP.
func fixture(t *testing.T, handler func(*http.Request) string) (Fetcher, *int) {
	t.Helper()
	calls := new(int)
	return Fetcher{resolver: resolverFunc(publicResolver), dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		*calls++
		if network != "tcp" || address != "8.8.8.8:80" {
			t.Errorf("un-pinned dial: %s %s", network, address)
		}
		a, b := net.Pipe()
		go func() {
			defer b.Close()
			req, err := http.ReadRequest(bufio.NewReader(b))
			if err != nil {
				return
			}
			io.WriteString(b, handler(req))
		}()
		return a, nil
	}}, calls
}
func response(headers, body string) string {
	return fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: %d\r\n%s\r\n%s", len(body), headers, body)
}
func redirect(location string) string {
	return "HTTP/1.1 302 Found\r\nContent-Length: 0\r\nLocation: " + location + "\r\n\r\n"
}

func TestNonPublicNeverDialed(t *testing.T) {
	blocked := []string{"0.0.0.0", "10.0.0.1", "100.64.0.1", "127.0.0.1", "168.63.129.16", "169.254.169.254", "172.16.0.1", "192.0.0.8", "192.0.2.1", "192.88.99.1", "192.168.0.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "224.0.0.1", "240.0.0.1", "255.255.255.255", "::", "::1", "::ffff:127.0.0.1", "fc00::1", "fe80::1", "ff02::1", "64:ff9b::a00:1", "64:ff9b:1::1", "100::1", "2001::1", "2001:2::1", "2001:db8::1", "2002:a00:1::1", "3fff::1", "4000::1"}
	for _, s := range blocked {
		t.Run(s, func(t *testing.T) {
			ip := netip.MustParseAddr(s)
			if publicIP(ip) {
				t.Fatal("classified public")
			}
			for _, mixed := range []bool{false, true} {
				f, calls := fixture(t, func(*http.Request) string { return response("", "unexpected") })
				f.resolver = resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
					a := []netip.Addr{ip}
					if mixed {
						a = append([]netip.Addr{netip.MustParseAddr("8.8.8.8")}, a...)
					}
					return a, nil
				})
				_, err := f.Fetch(context.Background(), "http://example.com/")
				if !errors.Is(err, ErrDestination) || *calls != 0 {
					t.Fatalf("err=%v dials=%d", err, *calls)
				}
			}
			host := s
			if ip.Is6() {
				host = "[" + s + "]"
			}
			f, calls := fixture(t, func(*http.Request) string { return "" })
			_, err := f.Fetch(context.Background(), "http://"+host+"/")
			if err != ErrDestination || *calls != 0 {
				t.Fatalf("literal: %v, %d", err, *calls)
			}
		})
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111", "::ffff:8.8.8.8"} {
		if !publicIP(netip.MustParseAddr(s)) {
			t.Errorf("public address refused: %s", s)
		}
	}
}

func TestInvalidURLNeverResolvesOrDials(t *testing.T) {
	urls := []string{"", "file:///etc/passwd", "ftp://example.com", "http://REPLACED-user:REPLACED-password@example.com", "http://example.com:0", "http://example.com:65536", "http://example.com:", "http://[fe80::1%25eth0]", "http:///path", "http://example.com/\nsecret", "http://example.com/" + strings.Repeat("x", 2048)}
	for _, u := range urls {
		t.Run(u[:min(len(u), 50)], func(t *testing.T) {
			f, calls := fixture(t, func(*http.Request) string { return "" })
			f.resolver = resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
				t.Error("invalid URL resolved")
				return nil, nil
			})
			_, err := f.Fetch(context.Background(), u)
			if err != ErrURL || *calls != 0 {
				t.Fatalf("err=%v calls=%d", err, *calls)
			}
		})
	}
}

func TestPinnedDNSAndFreshRedirects(t *testing.T) {
	resolves := 0
	f, calls := fixture(t, func(r *http.Request) string {
		if r.Host != "example.com" {
			t.Errorf("Host %q", r.Host)
		}
		return redirect("/next")
	})
	f.resolver = resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		resolves++
		if resolves == 1 {
			return publicResolver(nil, "", "")
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	})
	_, err := f.Fetch(context.Background(), "http://example.com/start")
	if err != ErrDestination || resolves != 2 || *calls != 1 {
		t.Fatalf("err=%v resolves=%d calls=%d", err, resolves, *calls)
	}
}

func TestRedirectEscapesAndLimit(t *testing.T) {
	for _, target := range []string{"http://127.0.0.1/", "http://[::1]/", "file:///etc/passwd", "http://REPLACED-user@example.com/", "http://example.com/" + strings.Repeat("x", 2048)} {
		f, calls := fixture(t, func(*http.Request) string { return redirect(target) })
		_, err := f.Fetch(context.Background(), "http://example.com/")
		if err == nil || *calls != 1 {
			t.Fatalf("err=%v calls=%d", err, *calls)
		}
	}
	f, calls := fixture(t, func(*http.Request) string { return redirect("/again") })
	_, err := f.Fetch(context.Background(), "http://example.com/")
	if err != ErrRedirect || *calls != MaxRedirects+1 {
		t.Fatalf("err=%v calls=%d", err, *calls)
	}
}

func TestNoCookiesCredentialsRefererOrProxy(t *testing.T) {
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(key, "http://127.0.0.1:1")
	}
	t.Setenv("NO_PROXY", "")
	step := 0
	f, calls := fixture(t, func(r *http.Request) string {
		for _, h := range []string{"Authorization", "Proxy-Authorization", "Cookie", "Referer"} {
			if r.Header.Get(h) != "" {
				t.Errorf("leaked %s", h)
			}
		}
		step++
		if step == 1 {
			return "HTTP/1.1 302 Found\r\nLocation: http://other.example/final\r\nSet-Cookie: session=REPLACED-cookie\r\nContent-Length: 0\r\n\r\n"
		}
		return response("", "hello")
	})
	p, err := f.Fetch(context.Background(), "http://example.com/start?REPLACED-query#fragment")
	if err != nil || string(p.Body) != "hello" || p.URL != "http://other.example/final" || *calls != 2 {
		t.Fatalf("page=%+v err=%v calls=%d", p, err, *calls)
	}
}

func TestSizeAndRepresentation(t *testing.T) {
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	gz.Write(bytes.Repeat([]byte("x"), MaxBytes+1))
	gz.Close()
	tests := []struct {
		name, raw string
		want      error
	}{
		{"exact", response("", strings.Repeat("x", MaxBytes)), nil},
		{"too large", response("", strings.Repeat("x", MaxBytes+1)), ErrSize},
		{"gzip bomb", response("Content-Encoding: gzip\r\n", compressed.String()), ErrSize},
		{"bad gzip", response("Content-Encoding: gzip\r\n", "REPLACED-content"), ErrTransport},
		{"unknown encoding", response("Content-Encoding: br\r\n", "x"), ErrUnsupported},
		{"pdf", "HTTP/1.1 200 OK\r\nContent-Type: application/pdf\r\nContent-Length: 1\r\n\r\nx", ErrUnsupported},
		{"upstream error", "HTTP/1.1 500 REPLACED-status\r\nContent-Length: 16\r\n\r\nREPLACED-content", ErrTransport},
		{"rate limit", "HTTP/1.1 429 Too Many Requests\r\nContent-Length: 0\r\n\r\n", ErrRateLimited},
		{"partial content", "HTTP/1.1 206 Partial Content\r\nContent-Type: text/plain\r\nContent-Range: bytes 0-0/99\r\nContent-Length: 1\r\n\r\nx", ErrTransport},
		{"partial without range", "HTTP/1.1 206 Partial Content\r\nContent-Type: text/plain\r\nContent-Length: 1\r\n\r\nx", ErrTransport},
		{"no content", "HTTP/1.1 204 No Content\r\nContent-Type: text/plain\r\n\r\n", ErrTransport},
		{"truncated", "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 99\r\n\r\nx", ErrTransport},
		{"chunked oversized", "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nTransfer-Encoding: chunked\r\n\r\n" + fmt.Sprintf("%x\r\n", MaxBytes+1) + strings.Repeat("x", MaxBytes+1) + "\r\n0\r\n\r\n", ErrSize},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, _ := fixture(t, func(*http.Request) string { return tt.raw })
			p, err := f.Fetch(context.Background(), "http://example.com/?REPLACED-query")
			if err != tt.want {
				t.Fatalf("got %v want %v", err, tt.want)
			}
			if err != nil && len(p.Body) != 0 {
				t.Fatal("partial page exposed")
			}
		})
	}
}

func TestCancellationAndDeadline(t *testing.T) {
	for _, phase := range []string{"dns", "dial", "headers", "body"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			f, _ := fixture(t, func(*http.Request) string { return response("", "ok") })
			switch phase {
			case "dns":
				f.resolver = resolverFunc(func(c context.Context, _, _ string) ([]netip.Addr, error) { <-c.Done(); return nil, c.Err() })
			case "dial":
				f.dial = func(c context.Context, _, _ string) (net.Conn, error) { <-c.Done(); return nil, c.Err() }
			default:
				f.dial = func(c context.Context, _, _ string) (net.Conn, error) {
					a, b := net.Pipe()
					go func() {
						defer b.Close()
						http.ReadRequest(bufio.NewReader(b))
						if phase == "body" {
							io.WriteString(b, "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 99\r\n\r\nx")
						}
						<-c.Done()
					}()
					return a, nil
				}
			}
			_, err := f.Fetch(ctx, "http://example.com/")
			if err != ErrTimeout {
				t.Fatalf("%v", err)
			}
		})
	}
	f, calls := fixture(t, func(*http.Request) string { return "" })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := f.Fetch(ctx, "http://example.com/")
	if err != ErrCancelled || *calls != 0 {
		t.Fatalf("%v calls=%d", err, *calls)
	}
	f.resolver = resolverFunc(func(c context.Context, _, _ string) ([]netip.Addr, error) {
		deadline, ok := c.Deadline()
		if !ok || time.Until(deadline) > MaxDuration || time.Until(deadline) < 29*time.Second {
			t.Error("missing 30s deadline")
		}
		return nil, errors.New("REPLACED-private-error")
	})
	_, err = f.Fetch(context.Background(), "http://example.com/?REPLACED-query")
	if err != ErrTransport {
		t.Fatalf("unsafe error: %v", err)
	}
}

func TestTLSVerifiesOriginalHostname(t *testing.T) {
	// Local TLS server is reachable only through the private fake dial seam.
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "ok")
	}))
	defer server.Close()
	var names []string
	var mu sync.Mutex
	f := Fetcher{resolver: resolverFunc(publicResolver), dial: func(ctx context.Context, _, address string) (net.Conn, error) {
		if address != "8.8.8.8:443" {
			t.Errorf("unpinned TLS %s", address)
		}
		// The transport still performs its own TLS verification above this raw
		// connection; a trusted fixture connection cannot disable that check.
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}}
	server.TLS.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		mu.Lock()
		names = append(names, hello.ServerName)
		mu.Unlock()
		return nil, nil
	}
	_, err := f.Fetch(context.Background(), "https://example.com/")
	if err != ErrTransport {
		t.Fatalf("untrusted/wrong-name certificate accepted: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(names) != 1 || names[0] != "example.com" {
		t.Fatalf("SNI=%v", names)
	}
}

func TestCompressedWireCeilingAndSuccessfulGzip(t *testing.T) {
	var good bytes.Buffer
	gz := gzip.NewWriter(&good)
	gz.Write([]byte("hello"))
	gz.Close()
	// Valid concatenated members with bounded comments consume wire bytes
	// while producing very few page bytes: the compressed cap is independent.
	var member bytes.Buffer
	mgz := gzip.NewWriter(&member)
	mgz.Comment = strings.Repeat("x", 400)
	mgz.Write([]byte("x"))
	mgz.Close()
	bomb := bytes.Repeat(member.Bytes(), MaxBytes/member.Len()+1)
	for _, tt := range []struct {
		name string
		body []byte
		want error
	}{
		{"valid", good.Bytes(), nil}, {"wire", bomb, ErrSize},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw := "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Encoding: gzip\r\nTransfer-Encoding: chunked\r\n\r\n" + fmt.Sprintf("%x\r\n", len(tt.body)) + string(tt.body) + "\r\n0\r\n\r\n"
			f, _ := fixture(t, func(*http.Request) string { return raw })
			p, err := f.Fetch(context.Background(), "http://example.com/")
			if err != tt.want {
				t.Fatalf("err=%v want=%v", err, tt.want)
			}
			if err == nil && string(p.Body) != "hello" {
				t.Fatalf("body=%q", p.Body)
			}
		})
	}
}

func TestRedirectsShareDeadline(t *testing.T) {
	var first time.Time
	calls := 0
	f, _ := fixture(t, func(*http.Request) string { return redirect("/next") })
	f.resolver = resolverFunc(func(ctx context.Context, _, _ string) ([]netip.Addr, error) {
		d, ok := ctx.Deadline()
		if !ok {
			t.Fatal("missing deadline")
		}
		calls++
		if calls == 1 {
			first = d
		} else if !d.Equal(first) {
			t.Error("redirect reset deadline")
		}
		return publicResolver(ctx, "", "")
	})
	_, err := f.Fetch(context.Background(), "http://example.com/")
	if err != ErrRedirect || calls != MaxRedirects+1 {
		t.Fatalf("%v calls=%d", err, calls)
	}
}

func TestDialCancellationReachesSocketAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered, done := make(chan struct{}), make(chan struct{})
	f := Fetcher{resolver: resolverFunc(publicResolver), dial: func(c context.Context, _, _ string) (net.Conn, error) {
		close(entered)
		<-c.Done()
		close(done)
		return nil, c.Err()
	}}
	result := make(chan error, 1)
	go func() { _, err := f.Fetch(ctx, "http://example.com/"); result <- err }()
	<-entered
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("socket attempt survived cancellation")
	}
	if err := <-result; err != ErrCancelled {
		t.Fatal(err)
	}
}

func TestIPv6PinnedLiteral(t *testing.T) {
	f := Fetcher{dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "[2606:4700:4700::1111]:8080" {
			t.Errorf("address=%s", address)
		}
		a, b := net.Pipe()
		go func() {
			defer b.Close()
			r, err := http.ReadRequest(bufio.NewReader(b))
			if err != nil {
				return
			}
			if r.Host != "[2606:4700:4700::1111]:8080" {
				t.Errorf("Host=%q", r.Host)
			}
			io.WriteString(b, response("", "ok"))
		}()
		return a, nil
	}}
	p, err := f.Fetch(context.Background(), "http://[2606:4700:4700::1111]:8080/")
	if err != nil || string(p.Body) != "ok" {
		t.Fatalf("%v %q", err, p.Body)
	}
}

func TestExtractionMetadataAndAuthenticationChallenges(t *testing.T) {
	for _, status := range []int{401, 403, 407} {
		f, _ := fixture(t, func(*http.Request) string {
			return fmt.Sprintf("HTTP/1.1 %d Blocked\r\nContent-Length: 0\r\n\r\n", status)
		})
		if _, err := f.Fetch(context.Background(), "http://example.com/"); err != ErrBlocked {
			t.Fatalf("%d: %v", status, err)
		}
	}
	f, _ := fixture(t, func(*http.Request) string {
		return "HTTP/1.1 200 OK\r\nContent-Type: text/html; charset=windows-1252\r\nLast-Modified: Wed, 30 Sep 2026 10:00:00 GMT\r\nContent-Length: 1\r\n\r\nx"
	})
	p, err := f.Fetch(context.Background(), "http://example.com/")
	if err != nil || p.Charset != "windows-1252" || p.SourceDate.Format(time.RFC3339) != "2026-09-30T10:00:00Z" {
		t.Fatalf("%+v %v", p, err)
	}
	f, _ = fixture(t, func(*http.Request) string {
		return "HTTP/1.1 200 OK\r\nWWW-Authenticate: Basic\r\nContent-Length: 0\r\n\r\n"
	})
	if _, err = f.Fetch(context.Background(), "http://example.com/"); err != ErrBlocked {
		t.Fatal(err)
	}
}
