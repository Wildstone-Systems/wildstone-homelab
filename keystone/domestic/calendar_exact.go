package domestic

// Exact, Person-scoped calendar effects (epic #326 S06; ADR-W030 A5.7).
//
// The tools in calendar.go take what a model resolved — a title and a
// date — and find the one event it names. A reviewed mail candidate is
// the opposite case: the Person approved ONE exact event, and the write
// must be that event, at most once, whatever the network does. So these
// act on a deterministic UID the caller chooses, and never search.
//
// They are the same path the tools take, not a second one: the acting
// Person's CURRENT access decides which collection a calendar id names
// (Resolve, re-checked immediately before every write), the household
// execution fence and the privacy transport wrap every request, and the
// deployment's calendar credential is the only one used. No mailbox
// credential reaches here; nothing here can read one.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/adapter/caldav"
)

// ErrCalendarAccess is every refusal because the acting Person may not
// read (or, for a write, write) the calendar they named — including a
// calendar that is not declared at all. The wrapped detail is for logs.
var ErrCalendarAccess = errors.New("domestic: that calendar is not one this person may use")

// upstreamFor resolves a calendar id for member under their current
// access and returns a copy of the upstream wired the way Execute wires
// a tool call: execution fence, privacy transport, and — for a write —
// access re-resolved immediately before the request is sent.
func (c *Calendar) upstreamFor(ctx context.Context, member, id string, write bool) (*caldav.Upstream, string, error) {
	if c == nil {
		return nil, "", fmt.Errorf("no calendar is configured on this control plane")
	}
	if member == "" || id == "" {
		return nil, "", fmt.Errorf("%w: a calendar and a person are both required", ErrCalendarAccess)
	}
	var upstream caldav.Upstream
	resolved := id
	if c.Resolve != nil {
		u, rid, err := c.Resolve(member, id, write)
		if err != nil {
			return nil, "", fmt.Errorf("%w: %v", ErrCalendarAccess, err)
		}
		upstream, resolved = *u, rid
	} else {
		// No registry resolver (a single-collection deployment, or a
		// test): only the one configured calendar exists.
		if c.Upstream == nil || c.Upstream.CollectionURL == "" {
			return nil, "", fmt.Errorf("no calendar is configured on this control plane")
		}
		upstream = *c.Upstream
	}
	if upstream.CollectionURL == "" {
		return nil, "", fmt.Errorf("no calendar is configured on this control plane")
	}
	client := upstream.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	upstream.HTTP = privacyHTTPClient(executionHTTPClient(client))
	prior := upstream.BeforeWrite
	resolve, authority := c.Resolve, c.Authority
	upstream.BeforeWrite = func() error {
		if err := checkExecution(ctx, authority); err != nil {
			return err
		}
		if resolve != nil {
			if _, _, err := resolve(member, resolved, true); err != nil {
				return fmt.Errorf("%w: %v", ErrCalendarAccess, err)
			}
		}
		if prior != nil {
			return prior()
		}
		return nil
	}
	return &upstream, resolved, nil
}

// CheckWritable reports whether member may write calendar id now.
func (c *Calendar) CheckWritable(ctx context.Context, member, id string) error {
	_, _, err := c.upstreamFor(ctx, member, id, true)
	return err
}

// Availability is calendar.check_availability for one calendar the
// acting Person can read now: the events overlapping [start, end). A
// calendar they cannot read is ErrCalendarAccess, never an empty
// answer, so a caller cannot mistake "not yours" for "free".
func (c *Calendar) Availability(ctx context.Context, member, id string, start, end time.Time) ([]caldav.Event, string, error) {
	if !end.After(start) {
		return nil, "", fmt.Errorf("the window ends at or before it starts")
	}
	u, resolved, err := c.upstreamFor(ctx, member, id, false)
	if err != nil {
		return nil, "", err
	}
	events, err := u.Events(ctx, start, end)
	if err != nil {
		return nil, resolved, err
	}
	sortEvents(events)
	return events, resolved, nil
}

// EventByUID reads the event at the deterministic UID, or nil when
// there is none. It needs read access only.
func (c *Calendar) EventByUID(ctx context.Context, member, id, uid string) (*caldav.Event, string, error) {
	u, _, err := c.upstreamFor(ctx, member, id, false)
	if err != nil {
		return nil, "", err
	}
	return u.EventByUID(ctx, uid)
}

// CreateExact writes w as a new event under its own UID with
// If-None-Match: *, so it can never replace anything — a second
// attempt at the same UID is caldav.ErrPreconditionFailed, never a
// second event.
func (c *Calendar) CreateExact(ctx context.Context, member, id string, w caldav.EventWrite) (href, etag string, err error) {
	if w.UID == "" {
		return "", "", fmt.Errorf("an exact create needs its UID")
	}
	if err := checkExecution(ctx, c.Authority); err != nil {
		return "", "", err
	}
	u, _, err := c.upstreamFor(ctx, member, id, true)
	if err != nil {
		return "", "", err
	}
	answer, err := u.CreateEvent(ctx, w)
	if err != nil {
		return "", "", err
	}
	href, etag = answerRef(answer.Data)
	return href, etag, nil
}

// UpdateExact rewrites the fields w carries on the event at w.UID,
// conditional on its current ETag.
func (c *Calendar) UpdateExact(ctx context.Context, member, id string, w caldav.EventWrite) (href, etag string, err error) {
	if w.UID == "" {
		return "", "", fmt.Errorf("an exact update needs its UID")
	}
	if err := checkExecution(ctx, c.Authority); err != nil {
		return "", "", err
	}
	u, _, err := c.upstreamFor(ctx, member, id, true)
	if err != nil {
		return "", "", err
	}
	answer, err := u.UpdateEvent(ctx, w)
	if err != nil {
		return "", "", err
	}
	href, etag = answerRef(answer.Data)
	return href, etag, nil
}

// DeleteExact removes the event at uid. An event already gone is not
// an error: the effect wanted is its absence.
func (c *Calendar) DeleteExact(ctx context.Context, member, id, uid string) error {
	if uid == "" {
		return fmt.Errorf("an exact delete needs its UID")
	}
	if err := checkExecution(ctx, c.Authority); err != nil {
		return err
	}
	u, _, err := c.upstreamFor(ctx, member, id, true)
	if err != nil {
		return err
	}
	_, err = u.DeleteEvent(ctx, caldav.EventWrite{UID: uid})
	return err
}

func answerRef(data any) (href, etag string) {
	m, _ := data.(map[string]any)
	href, _ = m["href"].(string)
	etag, _ = m["etag"].(string)
	return href, etag
}
