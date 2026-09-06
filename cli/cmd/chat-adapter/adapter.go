// Command chat-adapter is a Wildstone chat transport adapter (ADR-W026
// A2): a small process that speaks one messaging vendor's protocol on
// one side and Keystone's chat contract on the other.
//
// It is deliberately dumb. It holds one narrow credential, it does not
// choose which Butler answers — its credential's role decides that,
// server-side — and it never asserts who somebody is: it reports the
// chat account it saw, and a binding Keystone already holds turns that
// into a person. Nothing about the homelab is visible to it.
//
// Running it: one process per SURFACE per transport, each with its own
// bot and its own Keystone token (ADR-W026 A3). Two surfaces in one
// process is exactly the thing the whole design refuses.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

var version = "dev" // set via -ldflags at release

// Incoming is one message a transport heard.
type Incoming struct {
	// Account is the transport's own id for whoever spoke. It is the
	// name Keystone resolves against a binding, never a claim of
	// identity.
	Account string
	// Conversation is where a reply goes: a direct chat, or a group.
	Conversation string
	// Group marks a shared audience (ADR-W026 A7).
	Group bool
	// Text is what they said.
	Text string
	// MessageID is the transport's own id for this message; it becomes
	// the dedupe key, so a redelivery does not produce a second answer.
	MessageID string
	// DisplayName is what the transport calls this account, recorded on
	// a binding for the operator's list.
	DisplayName string
}

// Transport is one messaging vendor. Everything vendor-specific lives
// behind these three methods; adding a transport is implementing them
// and nothing else (ADR-W026 G5).
type Transport interface {
	// Name is the transport's canonical name, e.g. "telegram". Keystone
	// stores it on bindings and queued messages.
	Name() string
	// Receive blocks until there are messages or ctx is done. An empty
	// slice with a nil error means "nothing yet", which is the normal
	// outcome of a long poll.
	Receive(ctx context.Context) ([]Incoming, error)
	// Send delivers one message to a conversation.
	Send(ctx context.Context, conversation, body string) error
}

func main() {
	// Flags carry defaults from the environment, because this runs as a
	// container: a package declares configuration as env, and the same
	// binary run by hand on a laptop still wants flags. Neither ever
	// carries a credential — those are env-only, always (see below).
	fs := flag.NewFlagSet("chat-adapter", flag.ExitOnError)
	apiBase := fs.String("api", envOr("KEYSTONE_API", "http://127.0.0.1:9441"), "Keystone public API base URL")
	transportName := fs.String("transport", envOr("CHAT_TRANSPORT", "telegram"), "which transport to run: telegram | matrix")
	pollWait := fs.Duration("poll-wait", 25*time.Second, "how long to long-poll each side")
	showVersion := fs.Bool("version", false, "print version and exit")
	fs.Parse(os.Args[1:]) //nolint:errcheck // ExitOnError

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if *showVersion {
		fmt.Println("chat-adapter", version)
		return
	}

	// Credentials come from the environment only, never a flag — the
	// same rule the Butler's provider keys follow (ADR-W017 A4), for the
	// same reason: a flag is in every process listing on the host.
	token := os.Getenv("KEYSTONE_TOKEN")
	if token == "" {
		log.Error("KEYSTONE_TOKEN is not set; the adapter needs a Keystone token holding the converse verb")
		os.Exit(2)
	}

	transport, err := newTransport(*transportName, *pollWait)
	if err != nil {
		log.Error("configuring transport", "err", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	a := &adapter{
		keystone:  &keystoneClient{Base: *apiBase, Token: token},
		transport: transport,
		log:       log,
		pollWait:  *pollWait,
	}
	log.Info("chat adapter starting", "transport", transport.Name(), "api", *apiBase,
		"note", "the surface this serves is decided by the token's role, server-side")
	a.run(ctx)
}

// envOr reads an environment variable, falling back to a default.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// newTransport builds the named transport from its own environment.
func newTransport(name string, pollWait time.Duration) (Transport, error) {
	switch name {
	case "telegram":
		return newTelegram(os.Getenv("TELEGRAM_BOT_TOKEN"), pollWait)
	case "matrix":
		return newMatrix(os.Getenv("MATRIX_HOMESERVER"), os.Getenv("MATRIX_ACCESS_TOKEN"), pollWait)
	default:
		return nil, fmt.Errorf("unknown transport %q (telegram|matrix)", name)
	}
}

// adapter is the whole program: two loops that never share state beyond
// the client and the transport.
type adapter struct {
	keystone  *keystoneClient
	transport Transport
	log       *slog.Logger
	pollWait  time.Duration
}

func (a *adapter) run(ctx context.Context) {
	done := make(chan struct{}, 2)
	go func() { a.receiveLoop(ctx); done <- struct{}{} }()
	go func() { a.deliverLoop(ctx); done <- struct{}{} }()
	<-done
	<-done
	a.log.Info("chat adapter stopped")
}

// receiveLoop carries what people said to Keystone.
func (a *adapter) receiveLoop(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		msgs, err := a.transport.Receive(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			a.log.Error("receiving", "transport", a.transport.Name(), "err", err)
			backoff = sleepBackoff(ctx, backoff)
			continue
		}
		backoff = time.Second
		for _, m := range msgs {
			a.handle(ctx, m)
		}
	}
}

// handle sends one heard message onward. A message that LOOKS like a
// claim code is offered as one first: that is how an unbound account
// becomes bound (ADR-W026 A4), and it is the only special case the
// adapter knows. Everything else is relayed verbatim — an adapter that
// interprets messages is an adapter with opinions about the household.
func (a *adapter) handle(ctx context.Context, m Incoming) {
	if code, ok := claimCodeIn(m.Text); ok && !m.Group {
		if err := a.keystone.redeem(ctx, a.transport.Name(), m.Account, m.DisplayName, m.Conversation, code); err != nil {
			// The refusal is deliberately vague — Keystone does not say
			// why, and neither does this.
			a.reply(ctx, m.Conversation, "That code did not work. Ask for a fresh one.")
			a.log.Info("claim refused", "account", m.Account)
			return
		}
		a.reply(ctx, m.Conversation, "You're connected. Ask me something.")
		a.log.Info("account bound", "account", m.Account)
		return
	}
	if err := a.keystone.send(ctx, keystoneInbound{
		Transport:    a.transport.Name(),
		Account:      m.Account,
		Conversation: m.Conversation,
		Group:        m.Group,
		Text:         m.Text,
		MessageID:    m.MessageID,
		DisplayName:  m.DisplayName,
	}); err != nil {
		a.log.Error("relaying message", "account", m.Account, "err", err)
		// Say something rather than swallowing it: silence from a bot
		// is indistinguishable from being ignored.
		a.reply(ctx, m.Conversation, "I could not reach the house just now. Try again in a moment.")
	}
}

// reply sends one message directly, for the two cases that never reach
// the queue: a claim outcome, and an apology when Keystone is
// unreachable. Everything the Butler says goes through the queue.
func (a *adapter) reply(ctx context.Context, conversation, body string) {
	if err := a.transport.Send(ctx, conversation, body); err != nil {
		a.log.Error("sending", "err", err)
	}
}

// deliverLoop carries what the Butler said back out. Delivery is
// at-least-once: a message is acked only after the transport accepted
// it, so an adapter that dies mid-send re-reads it and sends again.
func (a *adapter) deliverLoop(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		msgs, err := a.keystone.poll(ctx, a.transport.Name(), a.pollWait)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			a.log.Error("polling outbound", "err", err)
			backoff = sleepBackoff(ctx, backoff)
			continue
		}
		backoff = time.Second
		for _, m := range msgs {
			if err := a.transport.Send(ctx, m.Target, m.Body); err != nil {
				a.log.Error("delivering", "seq", m.Seq, "err", err)
				// Not acked: it comes back on the next poll.
				continue
			}
			if err := a.keystone.ack(ctx, m.Seq); err != nil {
				a.log.Error("acking", "seq", m.Seq, "err", err)
			}
		}
	}
}

// sleepBackoff waits, doubling up to a ceiling. A transport that is down
// should not be hammered, and a control plane that is restarting should
// be found again quickly once it is back.
func sleepBackoff(ctx context.Context, d time.Duration) time.Duration {
	const maxBackoff = 30 * time.Second
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
	if d *= 2; d > maxBackoff {
		d = maxBackoff
	}
	return d
}

// claimAlphabet is the claim code's alphabet (keystone/principal): no
// 0/O, no 1/I/L, no vowels.
const claimAlphabet = "23456789BCDFGHJKMNPQRSTVWXYZ"

// claimCodeIn reports whether a message is somebody sending their claim
// code, and returns it. The test is strict on purpose: the message must
// be the code and nothing else, so an ordinary sentence that happens to
// contain twelve consonants is not mistaken for one.
func claimCodeIn(text string) (string, bool) {
	cleaned := strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' {
			return -1
		}
		return r
	}, strings.ToUpper(strings.TrimSpace(text)))
	if len(cleaned) != 12 {
		return "", false
	}
	for _, r := range cleaned {
		if !strings.ContainsRune(claimAlphabet, r) {
			return "", false
		}
	}
	return strings.TrimSpace(text), true
}

var errNotConfigured = errors.New("transport is not configured")
