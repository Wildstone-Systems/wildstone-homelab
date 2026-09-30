package research

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const successWire = `{"version":1,"data":{"items":[{"url":"https://example.com/docs#part","title":"Documentation","snippet":"Example","sourceDate":"2026-09-01T00:00:00Z"}],"provider":"ddgs","backend":"duckduckgo","retrievedAt":"2026-09-30T00:00:00Z"}}`
const emptyWire = `{"version":1,"data":{"items":[],"provider":"ddgs","backend":"duckduckgo","retrievedAt":"2026-09-30T00:00:00Z"}}`

func adapterServer(t *testing.T, handler http.HandlerFunc) *ddgs {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	a, err := newDDGS(s.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func respond(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprint(w, body)
}
func assertCode(t *testing.T, err error, code Code) {
	t.Helper()
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != code {
		t.Fatalf("error = %v, want %s", err, code)
	}
}
func TestDDGSSuccess(t *testing.T) {
	for _, region := range []string{"wt-wt", "us-en", "uk-en"} {
		t.Run(region, func(t *testing.T) {
			a := adapterServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/v1/search" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("Accept-Encoding") != "" {
					t.Error("unexpected request")
				}
				var req workerRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				if req.Version != 1 || req.Query != "documentation" || req.Limit != 5 || req.Backend != "duckduckgo" || req.Filters.Region == nil || *req.Filters.Region != region {
					t.Errorf("request: %+v", req)
				}
				respond(w, 200, successWire)
			})
			got, err := a.Search(context.Background(), SearchRequest{Query: "documentation", Filters: SearchFilters{Region: &region}})
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Items) != 1 || got.Items[0].URL != "https://example.com/docs" || got.Items[0].Snippet != "Example" || got.Items[0].Title != "Documentation" || got.Items[0].Provider != DDGS || got.Items[0].Backend != "duckduckgo" || got.Items[0].SourceDate.Format(time.RFC3339) != "2026-09-01T00:00:00Z" || got.RetrievedAt.Format(time.RFC3339) != "2026-09-30T00:00:00Z" || len(got.Attempts) != 1 || got.Attempts[0].Code != "" {
				t.Fatalf("result: %+v", got)
			}
		})
	}
}
func TestDDGSContractFailuresAndEmpty(t *testing.T) {
	for _, code := range []Code{"", InvalidInput, Unsupported, ProviderUnavailable, RateLimited, Timeout, Cancelled, InternalError} {
		t.Run(string(code), func(t *testing.T) {
			var calls atomic.Int32
			a := adapterServer(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if code == "" {
					respond(w, 200, emptyWire)
					return
				}
				respond(w, errorStatus(code), fmt.Sprintf(`{"version":1,"error":{"code":%q,"message":"private upstream detail"}}`, code))
			})
			result, err := a.Search(context.Background(), SearchRequest{Query: "example"})
			if code == "" {
				if err != nil || result.Items == nil || len(result.Items) != 0 {
					t.Fatalf("empty: %+v %v", result, err)
				}
			} else {
				assertCode(t, err, code)
				if strings.Contains(err.Error(), "private") {
					t.Fatal("raw error exposed")
				}
			}
			if calls.Load() != 1 || len(result.Attempts) != 1 || result.Attempts[0].Code != code {
				t.Fatalf("attempts: %+v", result.Attempts)
			}
		})
	}
}
func TestDDGSRefusesConstraintsBeforeIO(t *testing.T) {
	region, language, blank := "de-de", "en", ""
	days, zero := 7, 0
	cases := []struct {
		req  SearchRequest
		code Code
	}{
		{SearchRequest{Query: ""}, InvalidInput}, {SearchRequest{Query: strings.Repeat("é", 513)}, InvalidInput},
		{SearchRequest{Query: "a\nb"}, InvalidInput}, {SearchRequest{Query: string([]byte{0xff})}, InvalidInput},
		{SearchRequest{Query: "x", Limit: 6}, InvalidInput}, {SearchRequest{Query: "x", Limit: -1}, InvalidInput},
		{SearchRequest{Query: "x", Filters: SearchFilters{Region: &region}}, Unsupported},
		{SearchRequest{Query: "x", Filters: SearchFilters{Language: &language}}, Unsupported},
		{SearchRequest{Query: "x", Filters: SearchFilters{RecencyDays: &days}}, Unsupported},
		{SearchRequest{Query: "x", Filters: SearchFilters{RecencyDays: &zero}}, InvalidInput},
		{SearchRequest{Query: "x", Filters: SearchFilters{Region: &blank}}, InvalidInput},
	}
	a := adapterServer(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected network") })
	for _, tc := range cases {
		result, err := a.Search(context.Background(), tc.req)
		assertCode(t, err, tc.code)
		if len(result.Attempts) != 0 {
			t.Fatal("preflight counted as attempt")
		}
	}
	caps := a.Capabilities()
	if len(caps.SupportedFilters) != 1 || caps.SupportedFilters[0] != "region" || len(caps.Formats) != 0 || len(caps.Regions) != 3 {
		t.Fatalf("caps: %+v", caps)
	}
	caps.Regions[0] = "changed"
	if a.Capabilities().Regions[0] != "wt-wt" {
		t.Fatal("mutable capabilities")
	}
}
func TestDDGSRejectsMalformedWire(t *testing.T) {
	cases := map[string]string{
		"version":          strings.Replace(successWire, `"version":1`, `"version":2`, 1),
		"duplicate":        strings.Replace(successWire, `"version":1`, `"version":1,"version":1`, 1),
		"unknown":          strings.Replace(successWire, `"version":1`, `"version":1,"extra":1`, 1),
		"trailing":         successWire + `{}`,
		"null":             strings.Replace(successWire, `"Example"`, `null`, 1),
		"missing snippet":  strings.Replace(successWire, `"snippet":"Example",`, ``, 1),
		"missing items":    strings.Replace(emptyWire, `"items":[],`, ``, 1),
		"provider":         strings.Replace(successWire, `"provider":"ddgs"`, `"provider":"tavily"`, 1),
		"backend":          strings.Replace(successWire, `"backend":"duckduckgo"`, `"backend":"auto"`, 1),
		"date":             strings.Replace(successWire, `2026-09-01T00:00:00Z`, `yesterday`, 1),
		"retrieved":        strings.Replace(successWire, `2026-09-30T00:00:00Z`, `2026-09-30T01:00:00+01:00`, 1),
		"userinfo":         strings.Replace(successWire, `https://example.com`, `https://user@example.com`, 1),
		"scheme":           strings.Replace(successWire, `https://example.com`, `file://example.com`, 1),
		"port":             strings.Replace(successWire, `https://example.com`, `https://example.com:99999`, 1),
		"oversize title":   strings.Replace(successWire, `Documentation`, strings.Repeat("x", 513), 1),
		"oversize snippet": strings.Replace(successWire, `Example`, strings.Repeat("x", 4097), 1),
		"oversize body":    strings.Repeat(" ", workerResponseBytes) + emptyWire,
		"success error":    `{"version":1,"error":{"code":"timeout","message":"x"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			a := adapterServer(t, func(w http.ResponseWriter, r *http.Request) { respond(w, 200, body) })
			result, err := a.Search(context.Background(), SearchRequest{Query: "example"})
			assertCode(t, err, ProviderUnavailable)
			if len(result.Items) != 0 {
				t.Fatal("partial results escaped")
			}
		})
	}
}
func TestDDGSRejectsHTTPAndErrorMismatch(t *testing.T) {
	for _, status := range []int{301, 302, 307, 400, 429, 500, 503, 504} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var followed atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed.Add(1) }))
			defer target.Close()
			a := adapterServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", target.URL)
				respond(w, status, successWire)
			})
			_, err := a.Search(context.Background(), SearchRequest{Query: "example"})
			assertCode(t, err, ProviderUnavailable)
			if followed.Load() != 0 {
				t.Fatal("redirect followed")
			}
		})
	}
	for _, body := range []string{`{"version":1,"error":{"code":"timeout","message":"x"}}`, `{"version":1,"error":{"code":"unknown","message":"x"}}}`} {
		a := adapterServer(t, func(w http.ResponseWriter, r *http.Request) { respond(w, 503, body) })
		_, err := a.Search(context.Background(), SearchRequest{Query: "example"})
		assertCode(t, err, ProviderUnavailable)
	}
}
func TestDDGSCancellationAndDeadlines(t *testing.T) {
	for _, phase := range []string{"headers", "body"} {
		for _, deadline := range []bool{false, true} {
			t.Run(fmt.Sprint(phase, deadline), func(t *testing.T) {
				started, disconnected := make(chan struct{}), make(chan struct{})
				a := adapterServer(t, func(w http.ResponseWriter, r *http.Request) {
					io.Copy(io.Discard, r.Body)
					if phase == "body" {
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprint(w, `{"version":`)
						w.(http.Flusher).Flush()
					}
					close(started)
					<-r.Context().Done()
					close(disconnected)
				})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if deadline {
					a.timeout = 100 * time.Millisecond
				}
				done := make(chan error, 1)
				go func() { _, err := a.Search(ctx, SearchRequest{Query: "example"}); done <- err }()
				select {
				case <-started:
				case <-time.After(2 * time.Second):
					t.Fatal("request did not start")
				}
				if !deadline {
					cancel()
				}
				select {
				case err := <-done:
					if deadline {
						assertCode(t, err, Timeout)
					} else {
						assertCode(t, err, Cancelled)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("request did not stop")
				}
				select {
				case <-disconnected:
				case <-time.After(2 * time.Second):
					t.Fatal("worker did not see disconnect")
				}
			})
		}
	}
	a := adapterServer(t, func(http.ResponseWriter, *http.Request) { t.Error("cancelled call dispatched") })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := a.Search(ctx, SearchRequest{Query: "example"})
	assertCode(t, err, Cancelled)
	if len(result.Attempts) != 0 {
		t.Fatal("cancelled preflight counted")
	}
}
func readyProvider() Provider {
	return Provider{Config: Config{Name: "Example", Kind: DDGS, EndpointRef: "service/ddgs"}, State: "ready", ConfigRevision: "revision", TestedConfigRevision: "revision"}
}
func TestFactoryRegistryPolicy(t *testing.T) {
	var resolved, called atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called.Add(1); respond(w, 200, emptyWire) }))
	defer s.Close()
	f := Factory{ResolveEndpoint: func(ctx context.Context, ref string) (string, error) {
		resolved.Add(1)
		if ref != "service/ddgs" {
			t.Error("wrong ref")
		}
		return s.URL, nil
	}}
	p := readyProvider()
	a, err := f.NewSearch(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	p.Backends = []string{"auto"} // construction captures its own validated policy
	if _, err = a.Search(context.Background(), SearchRequest{Query: "example"}); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		change func(*Provider)
		code   Code
	}{
		{"stale", func(p *Provider) { p.TestedConfigRevision = "old" }, ProviderUnavailable},
		{"untested", func(p *Provider) { p.State = "untested" }, ProviderUnavailable},
		{"no endpoint", func(p *Provider) { p.EndpointRef = "" }, ProviderUnavailable},
		{"backend", func(p *Provider) { p.Backends = []string{"auto"} }, Unsupported},
		{"fallback", func(p *Provider) { p.FallbackOrder = []string{"google"} }, Unsupported},
		{"paid", func(p *Provider) { p.Kind = Tavily }, Unsupported},
		{"reserved", func(p *Provider) { p.Kind = SearXNG }, Unsupported},
		{"credential", func(p *Provider) { p.CredentialRef = "secret/example" }, Unsupported},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := readyProvider()
			tc.change(&p)
			n := resolved.Load()
			_, err := f.NewSearch(context.Background(), p)
			assertCode(t, err, tc.code)
			if resolved.Load() != n {
				t.Fatal("invalid policy resolved")
			}
		})
	}
	p = readyProvider()
	p.State = "untested"
	p.TestedConfigRevision = ""
	if err := f.Test(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if called.Load() != 2 {
		t.Fatal("unexpected upstream attempts")
	}
	_, err = (Factory{}).NewSearch(context.Background(), readyProvider())
	assertCode(t, err, ProviderUnavailable)
}
func TestFactoryRefusesUnsafeEndpoints(t *testing.T) {
	for _, endpoint := range []string{"", "file:///tmp/worker", "https://user@example.com", "https://example.com/search", "https://example.com/?q=x", "https://example.com/#x", "https://example.com:0"} {
		f := Factory{ResolveEndpoint: func(context.Context, string) (string, error) { return endpoint, nil }}
		_, err := f.NewSearch(context.Background(), readyProvider())
		assertCode(t, err, ProviderUnavailable)
	}
}

// These test-only shapes prove search does not require DDGS wire types or the
// extraction interface. They do not register/enable reserved providers.
type fakeSearXNG struct{}

func (fakeSearXNG) Capabilities() Capabilities {
	return Capabilities{SupportedFilters: []string{"language"}}
}
func (fakeSearXNG) Search(ctx context.Context, r SearchRequest) (SearchResult, error) {
	wire := struct {
		Results []struct{ URL, Title, Content string }
	}{}
	_ = json.Unmarshal([]byte(`{"Results":[{"URL":"https://example.com/","Title":"Example","Content":"Result"}]}`), &wire)
	row := wire.Results[0]
	return SearchResult{Provider: SearXNG, Items: []SearchHit{{Source: Source{URL: row.URL, Title: row.Title, Provider: SearXNG}, Snippet: row.Content}}}, nil
}

type fakeTavily struct{}

func (fakeTavily) Capabilities() Capabilities {
	return Capabilities{SupportedFilters: []string{"recencyDays"}}
}
func (fakeTavily) Search(ctx context.Context, r SearchRequest) (SearchResult, error) {
	wire := struct {
		Results []struct {
			URL, Content string
			Score        float64
		}
	}{}
	_ = json.Unmarshal([]byte(`{"Results":[{"URL":"https://example.com/","Content":"Result","Score":0.9}]}`), &wire)
	return SearchResult{Provider: Tavily, Items: []SearchHit{{Source: Source{URL: wire.Results[0].URL, Provider: Tavily}, Snippet: wire.Results[0].Content}}}, nil
}
func TestReservedProviderSeams(t *testing.T) {
	for _, a := range []SearchAdapter{fakeSearXNG{}, fakeTavily{}} {
		result, err := a.Search(context.Background(), SearchRequest{Query: "example"})
		if err != nil || len(result.Items) != 1 || result.Items[0].Snippet != "Result" {
			t.Fatalf("%+v %v", result, err)
		}
		if _, ok := a.(ExtractionAdapter); ok {
			t.Fatal("search unexpectedly requires extraction")
		}
	}
}
