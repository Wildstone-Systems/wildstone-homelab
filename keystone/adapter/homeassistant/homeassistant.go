// Package homeassistant is the Home Assistant query and event adapter
// (ADR-W030 A5.1, work package P12).
//
// It is READ-ONLY and deliberately narrow. Home Assistant's REST API
// will happily call services — turn things on, unlock things — and this
// adapter implements none of that: Act returns ErrNoSuchCapability for
// everything. A write path is a separate decision with its own approval
// story, and an adapter that can already do it is an adapter one config
// change away from doing it by accident.
//
// ADR-W030 A5.1 is explicit that the existing home-automation emitter is
// not a Home Assistant query adapter, and this is why: the emitter
// reports what happened, this answers what IS, and the second needs a
// credential with read access to every entity in the house.
//
// Everything it returns is treated as attacker-influenceable by the
// layer above: entity names and attributes are written by whatever
// integration created them.
package homeassistant

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter"
)

// The capabilities this adapter implements. Named constants because a
// capability string appears in a routine a person wrote, in a
// capability record, and in the work queue, and three spellings of it
// is a bug nobody sees until a routine silently never fires.
const (
	// CapStates reads entity states, optionally filtered.
	CapStates = "ha.states"
	// CapState reads one entity.
	CapState = "ha.state"
	// CapPresence answers who is home, which is a states read with the
	// person domain and a shape a routine can use directly.
	CapPresence = "ha.presence"
)

// Upstream is a Home Assistant instance.
type Upstream struct {
	// BaseURL is the instance, e.g. http://homeassistant.local:8123.
	BaseURL string
	// Token is a long-lived access token. It arrives from the node's
	// secret path into this process's environment; it is never in a work
	// payload and never reaches a model.
	Token string
	HTTP  *http.Client
	// Now is the clock, injectable for tests.
	Now func() time.Time
}

func (u *Upstream) Name() string { return "home-assistant" }

func (u *Upstream) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

func (u *Upstream) client() *http.Client {
	if u.HTTP != nil {
		return u.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// get calls the REST API. Any transport failure or non-200 becomes
// ErrUnavailable: from a household's point of view a Home Assistant that
// is down and one that is refusing this token are the same situation —
// it cannot answer — and the distinction belongs in a setup diagnostic,
// not in a routine's decision.
func (u *Upstream) get(ctx context.Context, path string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(u.BaseURL, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+u.Token)
	res, err := u.client().Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", adapter.ErrUnavailable, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("%w: %s answered %s: %s", adapter.ErrUnavailable,
			u.BaseURL, res.Status, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(res.Body).Decode(into)
}

// state is Home Assistant's entity shape, narrowed to what a household
// question needs. The full attribute bag is deliberately not carried:
// it is large, it is written by integrations, and a routine that needs
// one attribute should not pull in forty.
type state struct {
	EntityID    string         `json:"entity_id"`
	State       string         `json:"state"`
	LastChanged time.Time      `json:"last_changed"`
	Attributes  map[string]any `json:"attributes"`
}

// Entity is what this adapter reports.
type Entity struct {
	EntityID     string    `json:"entityId"`
	State        string    `json:"state"`
	FriendlyName string    `json:"friendlyName,omitempty"`
	LastChanged  time.Time `json:"lastChanged"`
}

func toEntity(s state) Entity {
	e := Entity{EntityID: s.EntityID, State: s.State, LastChanged: s.LastChanged}
	if name, ok := s.Attributes["friendly_name"].(string); ok {
		e.FriendlyName = name
	}
	return e
}

// Capabilities verifies against the running instance rather than
// declaring optimistically. If the API answers, the reads work; if it
// does not, they are reported unavailable and dependent work waits.
func (u *Upstream) Capabilities(ctx context.Context) ([]adapter.Capability, error) {
	var info struct {
		Version string `json:"version"`
		Message string `json:"message"`
	}
	available := true
	detail := ""
	if err := u.get(ctx, "/api/config", &info); err != nil {
		available = false
		detail = err.Error()
	}
	caps := make([]adapter.Capability, 0, 3)
	for _, name := range []string{CapStates, CapState, CapPresence} {
		caps = append(caps, adapter.Capability{
			Kind: "query", Name: name, Version: info.Version,
			Available: available, Detail: detail,
		})
	}
	return caps, nil
}

// Query answers a read.
func (u *Upstream) Query(ctx context.Context, capability string, request json.RawMessage) (adapter.Answer, error) {
	var req struct {
		EntityID string `json:"entityId"`
		Domain   string `json:"domain"`
	}
	if len(request) > 0 {
		_ = json.Unmarshal(request, &req)
	}
	switch capability {
	case CapState:
		if req.EntityID == "" {
			return adapter.Answer{}, fmt.Errorf("%s needs an entityId", CapState)
		}
		var s state
		if err := u.get(ctx, "/api/states/"+req.EntityID, &s); err != nil {
			return adapter.Answer{}, err
		}
		if s.EntityID == "" {
			// The entity does not exist. That is a real, empty answer —
			// not an outage — and reporting it as unavailable would make
			// a typo look like a broken house.
			return adapter.Answer{Empty: true, Data: map[string]any{"entityId": req.EntityID}}, nil
		}
		return adapter.Answer{Data: map[string]any{"entity": toEntity(s)}}, nil

	case CapStates, CapPresence:
		var states []state
		if err := u.get(ctx, "/api/states", &states); err != nil {
			return adapter.Answer{}, err
		}
		domain := req.Domain
		if capability == CapPresence {
			domain = "person"
		}
		entities := make([]Entity, 0, len(states))
		for _, s := range states {
			if domain != "" && !strings.HasPrefix(s.EntityID, domain+".") {
				continue
			}
			entities = append(entities, toEntity(s))
		}
		if len(entities) == 0 {
			// A house with no person entities is a real answer about a
			// house, and a routine should treat it as "nobody is
			// tracked", not as "the house is down".
			return adapter.Answer{Empty: true, Data: map[string]any{"entities": entities}}, nil
		}
		return adapter.Answer{Data: map[string]any{"entities": entities, "observedAt": u.now().UTC()}}, nil
	}
	return adapter.Answer{}, fmt.Errorf("%w: %s", adapter.ErrNoSuchCapability, capability)
}

// Act performs nothing. Read-only is the whole point: a write path is a
// separate decision with its own approval story, and an adapter that can
// already do it is one config change from doing it by accident.
func (u *Upstream) Act(context.Context, string, json.RawMessage) (adapter.Answer, error) {
	return adapter.Answer{}, fmt.Errorf("%w: this Home Assistant adapter is read-only",
		adapter.ErrNoSuchCapability)
}

var _ adapter.Upstream = (*Upstream)(nil)
