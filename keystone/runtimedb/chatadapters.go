package runtimedb

// What bot an adapter is (ADR-W026 A3).
//
// The adapter is the only part of the deployment that knows its own
// handle: the bot token is a write-only secret (ADR-W012), and the
// catalog knows the transport, not the account. It reports the handle on
// startup and on a heartbeat, and that is what lets an invite say "tap
// this link" instead of "send this code to the household bot" — which
// assumes the person knows which of the two bots that is.
//
// The handle is untrusted text from a process holding only `converse`.
// It is validated at the API boundary before it lands here, and nothing
// in this package builds a URL from it.

import (
	"fmt"
	"time"
)

// ChatAdapter is one registered adapter: which bot serves which surface.
type ChatAdapter struct {
	Capability  string    `json:"capability"`
	Transport   string    `json:"transport"`
	Surface     string    `json:"surface"`
	Handle      string    `json:"handle"`
	DisplayName string    `json:"displayName,omitempty"`
	SeenAt      time.Time `json:"seenAt"`
}

// UpsertChatAdapter records an adapter's identity and refreshes its
// heartbeat. The capability is derived from the caller's principal, not
// from anything it sent.
func (db *DB) UpsertChatAdapter(a ChatAdapter) error {
	if a.Capability == "" || a.Transport == "" || a.Handle == "" {
		return fmt.Errorf("runtimedb: chat adapter needs a capability, transport and handle")
	}
	if !ValidButlerSurface(ButlerSurface(a.Surface)) {
		return fmt.Errorf("runtimedb: unknown surface %q", a.Surface)
	}
	_, err := db.sql.Exec(`
        INSERT INTO chat_adapters (capability, transport, surface, handle, display_name, seen_at)
        VALUES (?, ?, ?, ?, ?, ?)
        ON CONFLICT(capability) DO UPDATE SET
            transport = excluded.transport,
            surface = excluded.surface,
            handle = excluded.handle,
            display_name = excluded.display_name,
            seen_at = excluded.seen_at`,
		a.Capability, a.Transport, a.Surface, a.Handle, a.DisplayName,
		time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("runtimedb: recording chat adapter: %w", err)
	}
	return nil
}

// GetChatAdapter returns one capability's registered adapter.
func (db *DB) GetChatAdapter(capability string) (ChatAdapter, error) {
	rows, err := db.ListChatAdapters()
	if err != nil {
		return ChatAdapter{}, err
	}
	for _, a := range rows {
		if a.Capability == capability {
			return a, nil
		}
	}
	return ChatAdapter{}, ErrNotFound
}

// ListChatAdapters returns every registered adapter, newest heartbeat
// first.
func (db *DB) ListChatAdapters() ([]ChatAdapter, error) {
	rows, err := db.sql.Query(`
        SELECT capability, transport, surface, handle, display_name, seen_at
        FROM chat_adapters ORDER BY seen_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: listing chat adapters: %w", err)
	}
	defer rows.Close()
	var out []ChatAdapter
	for rows.Next() {
		var a ChatAdapter
		var seen string
		if err := rows.Scan(&a.Capability, &a.Transport, &a.Surface, &a.Handle, &a.DisplayName, &seen); err != nil {
			return nil, fmt.Errorf("runtimedb: scanning chat adapter: %w", err)
		}
		a.SeenAt, _ = time.Parse(time.RFC3339Nano, seen)
		out = append(out, a)
	}
	return out, rows.Err()
}
