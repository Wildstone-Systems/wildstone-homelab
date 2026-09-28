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

	"github.com/chickenrat17/wildstone-homelab/keystone/speech"
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
	// Addressed says the message is FOR this bot: always true in a
	// direct message, and in a group only when the bot was mentioned or
	// replied to (ADR-W033 B9.1). The transport decides, because only it
	// can read its own mention syntax, and removes the mention from Text.
	Addressed bool
	// Text is what they said.
	Text string
	// MessageID is the transport's own id for this message; it becomes
	// the dedupe key, so a redelivery does not produce a second answer.
	MessageID string
	// DisplayName is what the transport calls this account, recorded on
	// a binding for the operator's list.
	DisplayName string
	// Voice is set when the message is a recording rather than text
	// (ADR-W042). Text is then empty, or a caption.
	Voice *VoiceNote
	// Problem is a transport limitation discovered while receiving this
	// event. It is logged locally and is never relayed to Keystone: the
	// adapter did not receive readable user content to authorize or answer.
	Problem string
}

// VoiceNote is a recording a transport heard but has NOT yet fetched.
//
// Fetching is deferred on purpose: the adapter downloads only after its
// own checks have passed — the flood guard, and "is this addressed to
// the bot" in a group — so other people's voice notes in a room, and a
// stranger flooding the bot, never cost a download. Keystone then makes
// the checks that matter (who is speaking, may this leave the house)
// before anything is transcribed.
type VoiceNote struct {
	// MIME is the container as the vendor labelled it, e.g. audio/ogg.
	MIME string
	// Seconds and Size are what the vendor reported, or 0 when it did
	// not say. They are checked before anything is fetched.
	Seconds float64
	Size    int64
	// Fetch downloads the recording, reading at most limit bytes. It
	// returns errVoiceTooLarge for anything bigger.
	Fetch func(ctx context.Context, limit int64) ([]byte, error)
}

// Limits on what the adapter will download. The size is Keystone's own
// bound; the length is the longest Keystone can be configured to
// accept, since the adapter does not know the configured value —
// Keystone applies that, and says so to the person.
const (
	maxVoiceBytes   = speech.MaxAudioBytes
	maxVoiceSeconds = 600
)

// errVoiceTooLarge is a recording over maxVoiceBytes, found out while
// fetching it (the vendor did not report its size, or reported it
// wrong, or refused to hand over a file that big).
var errVoiceTooLarge = errors.New("voice message too large to fetch")

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
	// Identify reports the bot's own handle — a Telegram username, a
	// Matrix MXID — and what the vendor calls it. This adapter is the
	// only part of the deployment that can answer: Keystone holds the
	// bot token write-only and can never read it back, so without this
	// an invite cannot name the bot a person is meant to message.
	Identify(ctx context.Context) (handle, displayName string, err error)
	// Advertise publishes the command list to the vendor's own menu,
	// where the vendor has one. Telegram does (setMyCommands); Matrix
	// does not, and implements this as a no-op rather than inventing
	// something that looks like one.
	//
	// The list comes from Keystone, which knows this adapter's surface
	// from the credential it authenticated with. The adapter carries no
	// copy: a household bot advertising /status is precisely the drift
	// a second source of truth would produce.
	Advertise(ctx context.Context, commands []Command) error
}

// Command is one entry in a vendor's command menu. Name carries no
// slash: the vendors render that themselves.
type Command struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

func main() {
	// Flags carry defaults from the environment, because this runs as a
	// container: a package declares configuration as env, and the same
	// binary run by hand on a laptop still wants flags. Neither ever
	// carries a credential — those are env-only, always (see below).
	fs := flag.NewFlagSet("chat-adapter", flag.ExitOnError)
	apiBase := fs.String("api", envOr("KEYSTONE_API", "http://127.0.0.1:9441"), "Keystone public API base URL")
	transportName := fs.String("transport", envOr("CHAT_TRANSPORT", "telegram"), "which transport to run: telegram | matrix | discord | slack")
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
		guard:     newGuard(log),
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
		t, err := newMatrix(os.Getenv("MATRIX_HOMESERVER"), os.Getenv("MATRIX_ACCESS_TOKEN"), pollWait)
		if err != nil {
			return nil, err
		}
		m := t.(*matrix)
		m.appserviceLocalpart = os.Getenv("MATRIX_APPSERVICE_LOCALPART")
		m.displayName = os.Getenv("MATRIX_DISPLAY_NAME")
		return m, nil
	case "discord":
		return newDiscord(os.Getenv("DISCORD_BOT_TOKEN"), pollWait)
	case "slack":
		return newSlack(os.Getenv("SLACK_BOT_TOKEN"), os.Getenv("SLACK_APP_TOKEN"), pollWait)
	default:
		return nil, fmt.Errorf("unknown transport %q (telegram|matrix|discord|slack)", name)
	}
}

// adapter is the whole program: two loops that never share state beyond
// the client and the transport.
type adapter struct {
	// guard drops floods before they reach Keystone (guard.go).
	guard     *guard
	keystone  *keystoneClient
	transport Transport
	log       *slog.Logger
	pollWait  time.Duration
}

func (a *adapter) run(ctx context.Context) {
	done := make(chan struct{}, 2)
	go a.registerLoop(ctx)
	go func() { a.receiveLoop(ctx); done <- struct{}{} }()
	go func() { a.deliverLoop(ctx); done <- struct{}{} }()
	<-done
	<-done
	a.log.Info("chat adapter stopped")
}

// registerHeartbeat is how often the adapter says it is still here.
// Keystone treats a stale registration as "the bot is not running",
// which is what lets the Console tell a bot nobody has messaged from a
// bot that is down — so this has to be comfortably inside the window
// Keystone considers live.
const registerHeartbeat = 10 * time.Minute

// registerLoop tells Keystone which bot this is, and keeps saying so.
//
// It is a heartbeat rather than a one-shot because the useful answer is
// not "the handle" but "the handle, and this adapter was running a
// minute ago". A failure here never stops the adapter: an unregistered
// adapter relays messages perfectly, it just cannot be turned into a tap
// -to-connect link.
func (a *adapter) registerLoop(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		handle, displayName, err := a.transport.Identify(ctx)
		var commands []Command
		if err == nil {
			commands, err = a.keystone.register(ctx, a.transport.Name(), handle, displayName)
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			a.log.Warn("registering the bot's handle", "err", err,
				"note", "messages still work; the Console cannot offer a connect link until this succeeds")
			backoff = sleepBackoff(ctx, backoff)
			continue
		}
		backoff = time.Second
		a.log.Info("registered", "transport", a.transport.Name(), "handle", handle)
		// A failed menu update is not a failed registration. Messages
		// and commands both still work; only the tap-to-see list is
		// stale, and the next heartbeat retries it.
		if err := a.transport.Advertise(ctx, commands); err != nil && ctx.Err() == nil {
			a.log.Warn("publishing the command menu", "err", err,
				"note", "commands still work; the menu may be stale until the next heartbeat")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(registerHeartbeat):
		}
	}
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
	if m.Problem != "" {
		a.log.Warn("message cannot be handled", "transport", a.transport.Name(),
			"conversation", m.Conversation, "message", m.MessageID, "reason", m.Problem)
		return
	}
	// A bot answers only what is addressed to it (ADR-W033 B9.1). In a
	// group that means a mention or a reply to the bot; everything else
	// said there is other people's conversation, and never leaves this
	// process — not even to Keystone.
	if m.Group && !m.Addressed {
		return
	}
	// Somebody the household has not introduced gets as little as
	// possible: no greeting, no "that code did not work", no "try
	// again". Each of those told a stranger this is a household's
	// assistant and how to get in. The one reply an unknown account can
	// earn is "you're connected", by sending a valid key — which only a
	// Person the household invited has.
	text, wasStart := withoutStartCommand(m.Text)
	if wasStart && strings.TrimSpace(text) == "" && !m.Group {
		return
	}
	key, isKey := claimCodeIn(text)
	isKey = isKey && !m.Group
	if a.guard != nil && !a.guard.admit(m.Account, isKey) {
		return
	}
	if code, ok := key, isKey; ok {
		if err := a.keystone.redeem(ctx, a.transport.Name(), m.Account, m.DisplayName, m.Conversation, code); err != nil {
			a.log.Info("claim refused", "account", m.Account)
			return
		}
		if a.guard != nil {
			a.guard.markKnown(m.Account)
		}
		a.reply(ctx, m.Conversation, "You're connected. Ask me something.")
		a.log.Info("account bound", "account", m.Account)
		return
	}
	inbound := keystoneInbound{
		Transport:    a.transport.Name(),
		Account:      m.Account,
		Conversation: m.Conversation,
		Group:        m.Group,
		Addressed:    m.Addressed,
		Text:         m.Text,
		MessageID:    m.MessageID,
		DisplayName:  m.DisplayName,
	}
	var (
		bound bool
		err   error
	)
	if m.Voice != nil {
		bound, err = a.relayVoice(ctx, inbound, m.Voice)
	} else {
		bound, err = a.keystone.send(ctx, inbound)
	}
	switch {
	case err == nil && bound && a.guard != nil:
		a.guard.markKnown(m.Account)
	case err == nil && a.guard != nil:
		a.guard.markUnknown(m.Account)
	}
	if err != nil {
		// Logged, not said: the adapter cannot tell whether the sender is
		// somebody the household knows, and a stranger learns nothing
		// from an outage either.
		a.log.Error("relaying message", "account", m.Account, "err", err)
	}
}

// relayVoice fetches a recording and posts it to Keystone's voice door
// (ADR-W042). A recording the vendor reports as too long or too big is
// not downloaded, but is still posted — marked tooLarge, with no audio
// — so that a person Keystone knows is told to send something shorter,
// and a stranger is told nothing, exactly as for any other message.
func (a *adapter) relayVoice(ctx context.Context, msg keystoneInbound, v *VoiceNote) (bool, error) {
	msg.Text = ""
	tooLarge := v.Size > maxVoiceBytes || v.Seconds > maxVoiceSeconds
	var audio []byte
	if !tooLarge {
		var err error
		audio, err = v.Fetch(ctx, maxVoiceBytes)
		switch {
		case errors.Is(err, errVoiceTooLarge):
			tooLarge, audio = true, nil
		case err != nil:
			// Nothing reached Keystone, so nothing can be said back:
			// the adapter does not know whether this is somebody the
			// household knows. Logged without the vendor URL, which
			// carries the bot token (see telegram.do).
			return false, fmt.Errorf("fetching voice message: %w", err)
		}
	}
	return a.keystone.sendVoice(ctx, keystoneVoice{
		keystoneInbound: msg, AudioSeconds: v.Seconds, TooLarge: tooLarge,
	}, audio, v.MIME)
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
	// Voice replies go to their own worker: a slow engine or upload
	// must never hold up the text of any later message.
	voices := make(chan voiceJob, voiceQueue)
	go a.voiceWorker(ctx, voices)
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
			ready, err := a.keystone.ready(ctx, a.transport.Name(), m.Seq)
			if err != nil {
				// A revocation reports only the queue sequence and a generic
				// failure. Neither the withheld body nor its subject is logged.
				a.log.Warn("delivery stopped before send", "seq", m.Seq, "err", err)
				continue
			}
			if err := a.transport.Send(ctx, ready.Target, ready.Body); err != nil {
				a.log.Error("delivering", "seq", m.Seq, "err", err)
				// Not acked: it comes back on the next poll.
				continue
			}
			if err := a.keystone.ack(ctx, m.Seq); err != nil {
				a.log.Error("acking", "seq", m.Seq, "err", err)
			}
			// After the ack, so a slow or failed voice never has the
			// text sent twice.
			if ready.Speak {
				a.queueVoice(voices, voiceJob{seq: m.Seq, conversation: ready.Target})
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

// withoutStartCommand strips a leading /start, which is how a tapped
// invite link arrives: Telegram turns t.me/<bot>?start=<code> into the
// message "/start <code>", and a bot added to a group gets
// "/start@thebot". Without this the code arrives wrapped in a command
// and reads as an ordinary sentence, so the one-tap invite silently does
// nothing.
func withoutStartCommand(text string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "/start") {
		return text, false
	}
	rest := strings.TrimPrefix(trimmed, "/start")
	// "/start@botname payload": the command may be addressed.
	if strings.HasPrefix(rest, "@") {
		if i := strings.IndexAny(rest, " \t"); i >= 0 {
			rest = rest[i:]
		} else {
			rest = ""
		}
	} else if rest != "" && !strings.ContainsAny(rest[:1], " \t") {
		// "/startle" is a word, not the command.
		return text, false
	}
	return strings.TrimSpace(rest), true
}

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

// stripMention reports whether text contains mention (case-insensitive)
// and returns it with every occurrence removed and the spacing tidied,
// so the Agent reads the question rather than its own name.
func stripMention(text, mention string) (string, bool) {
	if mention == "" {
		return text, false
	}
	var b strings.Builder
	found := false
	for i := 0; i < len(text); {
		if i+len(mention) <= len(text) && strings.EqualFold(text[i:i+len(mention)], mention) {
			found = true
			i += len(mention)
			continue
		}
		b.WriteByte(text[i])
		i++
	}
	if !found {
		return text, false
	}
	return strings.TrimLeft(strings.Join(strings.Fields(b.String()), " "), ",:; "), true
}
