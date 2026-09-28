package pantry

// The real PantryOS upstream: contract pantry-1 over HTTP (ADR-W044 A2).
//
// PantryOS publishes a surface shaped exactly like adapter.Upstream —
// capabilities, query, act — and answers in this runtime's own result
// types, so this file is a translation table and nothing more. Pantry
// logic lives in PantryOS; if it starts appearing here, the contract is
// missing something and belongs in PantryOS's ADR 0026 instead.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter"
)

const (
	// Contract is the PantryOS integration contract this upstream speaks.
	Contract = "pantry-1"
	// apiPrefix is where PantryOS mounts it.
	apiPrefix = "/api/integrations/wildstone/v1"
)

// The rest of pantry-1, beyond the three the fake also answers.
const (
	CapExpiring    = "pantry.expiring"
	CapSummary     = "pantry.summary"
	CapRecipes     = "pantry.recipes"
	CapRecipe      = "pantry.recipe"
	CapCookCurrent = "pantry.cook.current"
	CapTimers      = "pantry.timers"

	CapCheck       = "pantry.check"
	CapCookStart   = "pantry.cook.start"
	CapCookStep    = "pantry.cook.step"
	CapCookFinish  = "pantry.cook.finish"
	CapCookAbandon = "pantry.cook.abandon"
	CapTimerSet    = "pantry.timer.set"
	CapTimerCancel = "pantry.timer.cancel"
	CapMealLog     = "pantry.meal.log"
	CapMealUndo    = "pantry.meal.undo"
)

// ErrRejected is PantryOS refusing a request it understood: a list that
// is not the grocery list, a missing recipe id, "next" with nothing
// being cooked. It is a wrong question, not an outage, and callers must
// not read it as "PantryOS is down" — or as "nothing there".
var ErrRejected = errors.New("pantry: pantryos refused the request")

// knownCapabilities is what pantry-1 declares, used to report every
// capability as unavailable WITH a reason when PantryOS cannot be asked,
// rather than reporting nothing at all.
var knownCapabilities = []adapter.Capability{
	{Kind: "query", Name: CapList}, {Kind: "query", Name: CapInventory},
	{Kind: "query", Name: CapExpiring}, {Kind: "query", Name: CapSummary},
	{Kind: "query", Name: CapRecipes}, {Kind: "query", Name: CapRecipe},
	{Kind: "query", Name: CapCookCurrent}, {Kind: "query", Name: CapTimers},
	{Kind: "action", Name: CapAdd}, {Kind: "action", Name: CapCheck},
	{Kind: "action", Name: CapCookStart}, {Kind: "action", Name: CapCookStep},
	{Kind: "action", Name: CapCookFinish}, {Kind: "action", Name: CapCookAbandon},
	{Kind: "action", Name: CapTimerSet}, {Kind: "action", Name: CapTimerCancel},
	{Kind: "action", Name: CapMealLog}, {Kind: "action", Name: CapMealUndo},
}

// HTTP is a running PantryOS.
type HTTP struct {
	// BaseURL is PantryOS's address, e.g. https://pantry.example.internal.
	BaseURL string
	// AgentKey is a PantryOS agent key. It arrives from the node's secret
	// path into this process's environment; it is never in a work payload
	// and never reaches a model. A read-only key answers every query;
	// actions need one with write scope, and PantryOS refuses them
	// otherwise.
	AgentKey string
	HTTP     *http.Client
}

func (h *HTTP) Name() string { return "pantryos" }

// client never follows a redirect. The agent key travels in a custom
// header, which Go's redirect policy does not strip on a cross-origin
// hop, and a 307/308 would carry an action's body along with it. PantryOS
// does not redirect its integration routes, so a redirect means the
// address is wrong or something in between is — either way, stop, and
// report the 3xx as the unavailable answer it is.
func (h *HTTP) client() *http.Client {
	c := http.Client{Timeout: 10 * time.Second}
	if h.HTTP != nil {
		c = *h.HTTP
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

// envelope is every pantry-1 answer.
type envelope struct {
	Contract   string          `json:"contract"`
	Capability string          `json:"capability"`
	Status     string          `json:"status"`
	ObservedAt string          `json:"observedAt"`
	Data       json.RawMessage `json:"data"`
}

// problem is PantryOS's error body.
type problem struct {
	Detail string `json:"detail"`
}

// do sends one request and sorts the response into the result types.
//
// The one judgement call is 404. PantryOS answers an unknown capability
// with 404 and a detail naming it — that is ErrNoSuchCapability. A 404
// WITHOUT that detail means the address is not a PantryOS at all (a
// wrong base URL, a proxy's own page), and calling that "no such
// capability" would make a misconfiguration look like a missing
// feature; it is unavailable.
func (h *HTTP) do(ctx context.Context, method, path string, body io.Reader) (envelope, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(h.BaseURL, "/")+apiPrefix+path, body)
	if err != nil {
		return envelope{}, err
	}
	req.Header.Set("X-Agent-Key", h.AgentKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := h.client().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return envelope{}, ctx.Err()
		}
		return envelope{}, fmt.Errorf("%w: %v", adapter.ErrUnavailable, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))

	if res.StatusCode != http.StatusOK {
		var p problem
		_ = json.Unmarshal(raw, &p)
		detail := strings.TrimSpace(p.Detail)
		if detail == "" {
			detail = strings.TrimSpace(string(raw))
			if len(detail) > 200 {
				detail = detail[:200]
			}
		}
		switch {
		case res.StatusCode == http.StatusNotFound && strings.HasPrefix(detail, "PantryOS has no "):
			return envelope{}, fmt.Errorf("%w: %s", adapter.ErrNoSuchCapability, detail)
		case res.StatusCode == http.StatusUnprocessableEntity:
			return envelope{}, fmt.Errorf("%w: %s", ErrRejected, detail)
		default:
			// 401/403 (a revoked or read-only key), 5xx, a wrong address:
			// PantryOS cannot answer this, and dependent work waits.
			return envelope{}, fmt.Errorf("%w: %s answered %s: %s", adapter.ErrUnavailable, h.BaseURL, res.Status, detail)
		}
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return envelope{}, fmt.Errorf("%w: %s sent a body that is not a pantry-1 answer: %v", adapter.ErrUnavailable, h.BaseURL, err)
	}
	return env, nil
}

// answer turns a pantry-1 envelope into an Answer — only when it is the
// answer to THIS question: the contract this adapter speaks and the
// capability it asked for. A pantry-2 body, or a cached or misrouted
// answer to something else, is not a result.
func answer(env envelope, capability string) (adapter.Answer, error) {
	if env.Contract != Contract {
		return adapter.Answer{}, fmt.Errorf("%w: pantryos answered in contract %q, this adapter speaks %q", adapter.ErrUnavailable, env.Contract, Contract)
	}
	if env.Capability != capability {
		return adapter.Answer{}, fmt.Errorf("%w: asked for %s, pantryos answered for %q", adapter.ErrUnavailable, capability, env.Capability)
	}
	var data map[string]any
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &data); err != nil {
			return adapter.Answer{}, fmt.Errorf("%w: data is not a JSON object: %v", adapter.ErrUnavailable, err)
		}
	}
	if data == nil {
		data = map[string]any{}
	}
	var observed time.Time
	if env.ObservedAt != "" {
		if t, err := time.Parse(time.RFC3339Nano, env.ObservedAt); err == nil {
			observed = t
		}
	}
	switch env.Status {
	case "ok":
		return adapter.Answer{Data: data}, nil
	case "empty":
		return adapter.Answer{Data: data, Empty: true}, nil
	case "stale":
		if observed.IsZero() {
			// The runtime refuses a stale answer with no observation time,
			// and it is right to: this would be reported as current.
			return adapter.Answer{}, fmt.Errorf("%w: a stale answer arrived without observedAt", adapter.ErrUnavailable)
		}
		return adapter.Answer{Data: data, Stale: true, ObservedAt: observed}, nil
	case "unavailable":
		return adapter.Answer{}, fmt.Errorf("%w: pantryos reports %s unavailable", adapter.ErrUnavailable, env.Capability)
	}
	return adapter.Answer{}, fmt.Errorf("%w: unknown result status %q", adapter.ErrUnavailable, env.Status)
}

// Capabilities asks PantryOS what it can do today. If it cannot be
// asked, every pantry-1 capability is reported unavailable with the
// reason, so the connection says why rather than going quiet.
func (h *HTTP) Capabilities(ctx context.Context) ([]adapter.Capability, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(h.BaseURL, "/")+apiPrefix+"/capabilities", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Agent-Key", h.AgentKey)
	var body struct {
		Contract     string `json:"contract"`
		Capabilities []struct {
			Kind      string `json:"kind"`
			Name      string `json:"name"`
			Version   string `json:"version"`
			Available bool   `json:"available"`
		} `json:"capabilities"`
	}
	detail := ""
	res, err := h.client().Do(req)
	switch {
	case err != nil:
		detail = fmt.Sprintf("pantryos unreachable at %s: %v", h.BaseURL, err)
	case res.StatusCode != http.StatusOK:
		detail = fmt.Sprintf("pantryos at %s answered %s", h.BaseURL, res.Status)
		res.Body.Close()
	default:
		derr := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&body)
		res.Body.Close()
		if derr != nil {
			detail = fmt.Sprintf("pantryos at %s sent an unreadable capability list: %v", h.BaseURL, derr)
		} else if body.Contract != Contract {
			detail = fmt.Sprintf("pantryos at %s speaks contract %q, this adapter speaks %q", h.BaseURL, body.Contract, Contract)
		}
	}

	if detail != "" {
		out := make([]adapter.Capability, 0, len(knownCapabilities))
		for _, c := range knownCapabilities {
			c.Version, c.Available, c.Detail = Contract, false, detail
			out = append(out, c)
		}
		return out, nil
	}
	out := make([]adapter.Capability, 0, len(body.Capabilities))
	for _, c := range body.Capabilities {
		if c.Name == "" || c.Kind == "" {
			continue
		}
		entry := adapter.Capability{Kind: c.Kind, Name: c.Name, Version: c.Version, Available: c.Available}
		if !c.Available {
			entry.Detail = "pantryos reports this capability switched off"
		}
		out = append(out, entry)
	}
	return out, nil
}

// queryString flattens a JSON request object into query parameters.
// pantry-1 queries take scalars only; anything nested is a caller bug
// and is refused here rather than silently dropped.
func queryString(request json.RawMessage) (string, error) {
	if len(bytes.TrimSpace(request)) == 0 {
		return "", nil
	}
	var fields map[string]any
	if err := json.Unmarshal(request, &fields); err != nil {
		return "", fmt.Errorf("%w: a query request must be a JSON object: %v", ErrRejected, err)
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	values := url.Values{}
	for _, k := range keys {
		switch v := fields[k].(type) {
		case nil:
			continue
		case string:
			values.Set(k, v)
		case bool:
			values.Set(k, strconv.FormatBool(v))
		case float64:
			values.Set(k, strconv.FormatFloat(v, 'f', -1, 64))
		default:
			return "", fmt.Errorf("%w: query parameter %q must be a string, number or boolean", ErrRejected, k)
		}
	}
	if len(values) == 0 {
		return "", nil
	}
	return "?" + values.Encode(), nil
}

// Query answers a read.
func (h *HTTP) Query(ctx context.Context, capability string, request json.RawMessage) (adapter.Answer, error) {
	qs, err := queryString(request)
	if err != nil {
		return adapter.Answer{}, err
	}
	env, err := h.do(ctx, http.MethodGet, "/query/"+url.PathEscape(capability)+qs, nil)
	if err != nil {
		return adapter.Answer{}, err
	}
	return answer(env, capability)
}

// Act performs a declared action. Whether it is ALLOWED was decided
// before the work was queued (grants, the connection's actions switch,
// the action's class); this only carries it out.
func (h *HTTP) Act(ctx context.Context, capability string, request json.RawMessage) (adapter.Answer, error) {
	body := bytes.TrimSpace(request)
	if len(body) == 0 {
		body = []byte("{}")
	}
	env, err := h.do(ctx, http.MethodPost, "/act/"+url.PathEscape(capability), bytes.NewReader(body))
	if err != nil {
		return adapter.Answer{}, err
	}
	return answer(env, capability)
}

var _ adapter.Upstream = (*HTTP)(nil)
