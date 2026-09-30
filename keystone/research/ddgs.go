package research

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const workerResponseBytes = 65536

type ddgs struct {
	endpoint string
	timeout  time.Duration
	client   *http.Client
}

func newDDGS(endpoint string, timeout time.Duration) (*ddgs, error) {
	u, ok := validURL(endpoint)
	if !ok || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, failure(ProviderUnavailable)
	}
	u.Path = "/v1/search"
	// Internal service transport is separate from public URL fetching. Never use
	// environment proxies, cookies, redirects, compression, or implicit retries.
	transport := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: timeout}).DialContext,
		TLSHandshakeTimeout: timeout, ResponseHeaderTimeout: timeout,
		DisableCompression: true, DisableKeepAlives: true,
		MaxResponseHeaderBytes: 8192,
	}
	return &ddgs{endpoint: u.String(), timeout: timeout, client: &http.Client{
		Transport: transport, Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (*ddgs) Capabilities() Capabilities {
	return Capabilities{SupportedFilters: []string{"region"}, Regions: []string{"wt-wt", "us-en", "uk-en"}, Formats: []string{}}
}

type workerRequest struct {
	Version int           `json:"version"`
	Query   string        `json:"query"`
	Filters SearchFilters `json:"filters"`
	Limit   int           `json:"limit"`
	Backend string        `json:"backend"`
}
type workerHit struct {
	URL        string  `json:"url"`
	Title      string  `json:"title,omitempty"`
	Snippet    *string `json:"snippet"`
	SourceDate string  `json:"sourceDate,omitempty"`
}
type workerData struct {
	Items       *[]workerHit `json:"items"`
	Provider    ProviderKind `json:"provider"`
	Backend     string       `json:"backend"`
	RetrievedAt string       `json:"retrievedAt"`
}
type workerError struct {
	Code    Code    `json:"code"`
	Message *string `json:"message"`
}
type workerResponse struct {
	Version int          `json:"version"`
	Data    *workerData  `json:"data,omitempty"`
	Error   *workerError `json:"error,omitempty"`
}

func (a *ddgs) Search(ctx context.Context, input SearchRequest) (SearchResult, error) {
	result := SearchResult{Provider: DDGS, Backend: "duckduckgo", Items: []SearchHit{}}
	if ctx.Err() != nil {
		return result, contextError(ctx)
	}
	if input.Limit == 0 {
		input.Limit = 5
	}
	if !validText(input.Query, 1024, true) || input.Limit < 1 || input.Limit > 5 {
		return result, failure(InvalidInput)
	}
	f := input.Filters
	if (f.Region != nil && !validText(*f.Region, 1024, true)) || (f.Language != nil && !validText(*f.Language, 1024, true)) || (f.RecencyDays != nil && (*f.RecencyDays < 1 || *f.RecencyDays > 36500)) {
		return result, failure(InvalidInput)
	}
	if f.Language != nil || f.RecencyDays != nil || (f.Region != nil && *f.Region != "wt-wt" && *f.Region != "us-en" && *f.Region != "uk-en") {
		return result, failure(Unsupported)
	}
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	body, err := json.Marshal(workerRequest{1, input.Query, f, input.Limit, "duckduckgo"})
	if err != nil || len(body) > 8192 {
		return result, failure(InvalidInput)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint, bytes.NewReader(body))
	if err != nil {
		return result, failure(ProviderUnavailable)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	result.Attempts = []Attempt{{Provider: DDGS, Backend: "duckduckgo"}}
	fail := func(err error) (SearchResult, error) {
		var typed *Error
		if !errors.As(err, &typed) {
			err = failure(ProviderUnavailable)
			typed = err.(*Error)
		}
		result.Attempts[0].Code = typed.Code
		return result, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		var timeout net.Error
		if ctx.Err() == nil && errors.As(err, &timeout) && timeout.Timeout() {
			return fail(failure(Timeout))
		}
		return fail(contextError(ctx))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, workerResponseBytes+1))
	if ctx.Err() != nil {
		return fail(contextError(ctx))
	}
	if err != nil || len(raw) > workerResponseBytes {
		return fail(failure(ProviderUnavailable))
	}
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || media != "application/json" || resp.Header.Get("Content-Encoding") != "" {
		return fail(failure(ProviderUnavailable))
	}
	var wire workerResponse
	if !decodeWorker(raw, &wire) || wire.Version != 1 {
		return fail(failure(ProviderUnavailable))
	}
	if resp.StatusCode != http.StatusOK {
		if wire.Data != nil || wire.Error == nil || wire.Error.Message == nil || !validText(*wire.Error.Message, 1024, true) || errorStatus(wire.Error.Code) != resp.StatusCode {
			return fail(failure(ProviderUnavailable))
		}
		return fail(failure(wire.Error.Code))
	}
	if wire.Error != nil || wire.Data == nil {
		return fail(failure(ProviderUnavailable))
	}
	data := wire.Data
	stamp, ok := utcStamp(data.RetrievedAt)
	if !ok || data.Provider != DDGS || data.Backend != "duckduckgo" || data.Items == nil || len(*data.Items) > input.Limit {
		return fail(failure(ProviderUnavailable))
	}
	hits := make([]SearchHit, 0, len(*data.Items))
	seen := map[string]bool{}
	for _, row := range *data.Items {
		u, ok := validURL(row.URL)
		if !ok || row.Snippet == nil || !validText(*row.Snippet, 4096, false) || !validText(row.Title, 512, false) {
			return fail(failure(ProviderUnavailable))
		}
		u.Fragment = ""
		if u.Path == "" {
			u.Path = "/"
		}
		u.Host = strings.ToLower(u.Host)
		source := Source{URL: u.String(), Title: row.Title, Provider: DDGS, Backend: "duckduckgo", RetrievedAt: stamp}
		if row.SourceDate != "" {
			date, ok := utcStamp(row.SourceDate)
			if !ok {
				return fail(failure(ProviderUnavailable))
			}
			source.SourceDate = &date
		}
		if seen[source.URL] {
			continue
		}
		seen[source.URL] = true
		hits = append(hits, SearchHit{Source: source, Snippet: *row.Snippet})
	}
	result.Items, result.RetrievedAt = hits, stamp
	return result, nil
}

func validText(s string, limit int, nonempty bool) bool {
	return utf8.ValidString(s) && len(s) <= limit && (!nonempty || strings.TrimSpace(s) != "") && strings.IndexFunc(s, func(r rune) bool { return r < 32 || r == 127 }) < 0
}
func validURL(s string) (*url.URL, bool) {
	if !validText(s, 2048, true) || strings.Contains(s, "\\") || strings.IndexFunc(s, unicode.IsSpace) >= 0 {
		return nil, false
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Opaque != "" || strings.Contains(u.Hostname(), "%") {
		return nil, false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, false
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return nil, false
	}
	return u, true
}
func utcStamp(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	return t.UTC(), err == nil && strings.HasSuffix(s, "Z")
}
func errorStatus(code Code) int {
	switch code {
	case InvalidInput:
		return 400
	case Unsupported:
		return 422
	case ProviderUnavailable:
		return 503
	case RateLimited:
		return 429
	case Timeout:
		return 504
	case Cancelled:
		return 499
	case InternalError:
		return 500
	}
	return 0
}

// Reject duplicate keys, nulls, invalid UTF-8 and trailing values before typed
// decoding. Unknown fields are refused rather than silently changing v1 wire.
func decodeWorker(raw []byte, dst any) bool {
	if !utf8.Valid(raw) {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	var value func(int) bool
	value = func(depth int) bool {
		if depth > 8 {
			return false
		}
		token, err := d.Token()
		if err != nil || token == nil {
			return false
		}
		delim, isDelim := token.(json.Delim)
		if !isDelim {
			return true
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				name, ok := key.(string)
				if err != nil || !ok || seen[name] {
					return false
				}
				if !workerKeys[name] {
					return false
				}
				seen[name] = true
				if !value(depth + 1) {
					return false
				}
			}
		case '[':
			for d.More() {
				if !value(depth + 1) {
					return false
				}
			}
		default:
			return false
		}
		_, err = d.Token()
		return err == nil
	}
	if !value(0) {
		return false
	}
	if _, err := d.Token(); err != io.EOF {
		return false
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(dst) == nil
}

var workerKeys = map[string]bool{"version": true, "data": true, "error": true, "items": true, "provider": true, "backend": true, "retrievedAt": true, "url": true, "title": true, "snippet": true, "sourceDate": true, "code": true, "message": true}
