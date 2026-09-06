package main

// The Keystone half of the adapter: three calls, and not one of them
// reads homelab state (ADR-W026 A8).
//
// This client holds a token whose role decides which surface it serves.
// It deliberately never sends a `surface` field: the server derives it,
// and an adapter that could name one would be an adapter that could be
// talked into naming the other.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type keystoneClient struct {
	Base  string
	Token string
	HTTP  *http.Client
}

func (k *keystoneClient) client() *http.Client {
	if k.HTTP != nil {
		return k.HTTP
	}
	// Generous: the inbound call runs a model session server-side.
	return &http.Client{Timeout: 5 * time.Minute}
}

type keystoneInbound struct {
	Transport    string `json:"transport"`
	Account      string `json:"account"`
	Conversation string `json:"conversation,omitempty"`
	Group        bool   `json:"group,omitempty"`
	Text         string `json:"text"`
	MessageID    string `json:"messageId,omitempty"`
	DisplayName  string `json:"displayName,omitempty"`
}

// outboundMessage is one queued reply, as the adapter needs it. The
// queue carries more (surface, binding, correlation id); an adapter has
// no use for any of it, so it reads only what it delivers.
type outboundMessage struct {
	Seq    int64  `json:"seq"`
	Target string `json:"target"`
	Body   string `json:"body"`
}

func (k *keystoneClient) send(ctx context.Context, msg keystoneInbound) error {
	_, err := k.call(ctx, http.MethodPost, "/api/v1/chat/messages", msg)
	return err
}

func (k *keystoneClient) redeem(ctx context.Context, transport, account, displayName, code string) error {
	_, err := k.call(ctx, http.MethodPost, "/api/v1/chat/bindings/redeem", map[string]string{
		"transport": transport, "account": account, "displayName": displayName, "code": code,
	})
	return err
}

// poll long-polls this adapter's outbound queue. The cursor is always 0:
// the queue returns undelivered messages only, so "everything not yet
// acked" is the correct read after a restart, a crash, or a redeploy —
// there is no cursor to persist and lose.
func (k *keystoneClient) poll(ctx context.Context, transport string, wait time.Duration) ([]outboundMessage, error) {
	q := url.Values{
		"transport": {transport},
		"after":     {"0"},
		"waitMs":    {strconv.Itoa(int(wait.Milliseconds()))},
	}
	body, err := k.call(ctx, http.MethodGet, "/api/v1/chat/outbound?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var msgs []outboundMessage
	if err := json.Unmarshal(body, &msgs); err != nil {
		return nil, fmt.Errorf("decoding outbound: %w", err)
	}
	return msgs, nil
}

func (k *keystoneClient) ack(ctx context.Context, seq int64) error {
	_, err := k.call(ctx, http.MethodPost, fmt.Sprintf("/api/v1/chat/outbound/%d/ack", seq), nil)
	return err
}

func (k *keystoneClient) call(ctx context.Context, method, path string, body any) ([]byte, error) {
	var buf io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		buf = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(k.Base, "/")+path, buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+k.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := k.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: %s: %s", method, path, res.Status, strings.TrimSpace(string(data)))
	}
	return data, nil
}
