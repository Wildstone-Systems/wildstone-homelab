package main

// The Matrix driver (ADR-W026 G5): the second transport, written
// against the same three methods as the first.
//
// It exists to prove the contract is a contract rather than a
// Telegram-shaped hole. Nothing in keystone changed to add it, nothing
// in adapter.go knows it exists beyond one line in newTransport, and it
// makes no call this file does not contain.
//
// WHY MATRIX and not Discord or Slack. Both of those receive over a
// gateway websocket (Slack's alternative is an inbound webhook, which
// wants a public endpoint) — so either would trade away the property
// that makes the sidecar shape safe: the adapter dials OUT, and the
// deployment gains no inbound port. Matrix's /sync is a long poll over
// plain HTTPS, the same shape as Telegram's getUpdates, and a household
// can self-host the homeserver. Adding Discord later is this file again
// with a websocket library; the contract above it does not move.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type matrix struct {
	homeserver string
	token      string
	timeout    time.Duration
	http       *http.Client

	// userID is this bot's own MXID, read from the homeserver at
	// startup. Without it the bot answers its own messages, forever.
	userID string
	// since is Matrix's own cursor.
	since string
	// txn numbers outgoing messages: Matrix deduplicates on the
	// transaction id, so a retried send is not a second message.
	txn atomic.Int64
}

func newMatrix(homeserver, token string, pollWait time.Duration) (Transport, error) {
	if homeserver == "" || token == "" {
		return nil, fmt.Errorf("MATRIX_HOMESERVER and MATRIX_ACCESS_TOKEN must be set: %w", errNotConfigured)
	}
	if pollWait <= 0 {
		pollWait = 25 * time.Second
	}
	m := &matrix{homeserver: strings.TrimRight(homeserver, "/"), token: token, timeout: pollWait}
	return m, nil
}

// Advertise is a no-op: Matrix has no command-menu concept, and
// inventing one — posting a list into the room, say — would be this
// adapter deciding to send an unasked-for message. Commands still work;
// /help is how they are discovered here.
func (m *matrix) Advertise(context.Context, []Command) error { return nil }

func (m *matrix) Name() string { return "matrix" }

func (m *matrix) client() *http.Client {
	if m.http != nil {
		return m.http
	}
	return &http.Client{Timeout: m.timeout + 30*time.Second}
}

// syncResponse is the slice of Matrix's /sync this adapter reads.
type syncResponse struct {
	NextBatch string `json:"next_batch"`
	Rooms     struct {
		Join map[string]struct {
			Summary struct {
				JoinedMemberCount int `json:"m.joined_member_count"`
			} `json:"summary"`
			Timeline struct {
				Events []struct {
					Type    string `json:"type"`
					EventID string `json:"event_id"`
					Sender  string `json:"sender"`
					Content struct {
						MsgType string `json:"msgtype"`
						Body    string `json:"body"`
					} `json:"content"`
				} `json:"events"`
			} `json:"timeline"`
		} `json:"join"`
	} `json:"rooms"`
}

// Identify reports this bot's MXID. Matrix has no invite payload, so
// the link it produces only opens the conversation — the code is still
// sent by hand — but naming the right bot is most of the problem when a
// household runs two.
func (m *matrix) Identify(ctx context.Context) (string, string, error) {
	if m.userID == "" {
		if err := m.whoami(ctx); err != nil {
			return "", "", err
		}
	}
	return m.userID, "", nil
}

func (m *matrix) Receive(ctx context.Context) ([]Incoming, error) {
	if m.userID == "" {
		if err := m.whoami(ctx); err != nil {
			return nil, err
		}
	}
	q := url.Values{"timeout": {strconv.Itoa(int(m.timeout.Milliseconds()))}}
	if m.since != "" {
		q.Set("since", m.since)
	} else {
		// First sync: take the cursor and none of the backlog. A bot
		// that starts up and answers a week of history is a bot nobody
		// invites back.
		q.Set("full_state", "false")
		q.Set("timeout", "0")
	}
	var sync syncResponse
	if err := m.call(ctx, http.MethodGet, "/_matrix/client/v3/sync?"+q.Encode(), nil, &sync); err != nil {
		return nil, err
	}
	first := m.since == ""
	m.since = sync.NextBatch
	if first {
		return nil, nil
	}

	var out []Incoming
	for roomID, room := range sync.Rooms.Join {
		for _, ev := range room.Timeline.Events {
			if ev.Type != "m.room.message" || ev.Content.MsgType != "m.text" {
				continue // an image, a reaction, a membership change
			}
			if ev.Sender == m.userID {
				continue // our own reply, echoed back by the homeserver
			}
			if strings.TrimSpace(ev.Content.Body) == "" {
				continue
			}
			out = append(out, Incoming{
				Account:      ev.Sender,
				Conversation: roomID,
				// Matrix has no flag for "this is a DM": a room is one by
				// convention when it holds two people. Erring toward
				// GROUP is the safe direction — the group rules are more
				// careful, not less (ADR-W026 A7) — so a room whose
				// membership we cannot read is treated as shared.
				Group:       room.Summary.JoinedMemberCount != 2,
				Text:        ev.Content.Body,
				MessageID:   ev.EventID,
				DisplayName: ev.Sender,
			})
		}
	}
	return out, nil
}

// whoami reads this bot's own MXID. It is not optional: without it the
// adapter cannot tell its own replies from somebody's questions.
func (m *matrix) whoami(ctx context.Context) error {
	var who struct {
		UserID string `json:"user_id"`
	}
	if err := m.call(ctx, http.MethodGet, "/_matrix/client/v3/account/whoami", nil, &who); err != nil {
		return fmt.Errorf("matrix whoami: %w", err)
	}
	if who.UserID == "" {
		return fmt.Errorf("matrix whoami returned no user id")
	}
	m.userID = who.UserID
	return nil
}

func (m *matrix) Send(ctx context.Context, conversation, body string) error {
	txn := m.txn.Add(1)
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/send/m.room.message/%d-%d",
		url.PathEscape(conversation), time.Now().UnixNano(), txn)
	payload := map[string]string{"msgtype": "m.text", "body": body}
	var out struct {
		EventID string `json:"event_id"`
	}
	return m.call(ctx, http.MethodPut, path, payload, &out)
}

func (m *matrix) call(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = strings.NewReader(string(data))
	}
	req, err := http.NewRequestWithContext(ctx, method, m.homeserver+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := m.client().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return err
	}
	if res.StatusCode >= 300 {
		return fmt.Errorf("matrix %s: %s: %s", path, res.Status, strings.TrimSpace(string(data)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}
