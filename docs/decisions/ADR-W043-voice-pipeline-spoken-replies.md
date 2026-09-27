# ADR-W043: The voice pipeline, part two — spoken replies

- **Status:** accepted; implemented (`keystone/speech/voice.go`, `-speech-replies` flags, `POST /api/v1/chat/outbound/{seq}/speech`, `POST /api/v1/assistant/speak`, `POST /api/v1/butlers/{name}/speak`, the adapter's `VoiceSender` for Telegram, Discord, Slack and Matrix, listen buttons on the dashboard and in the Console). It also closes ADR-W042's one deferred item, the 🎙️ marker on spoken turns (§6).
- **Governed by:** ADR-W042 (the privacy rule, the engine choice, the package) and ADR-W033 where they overlap.
- **Scope:** reading the Agent's answer aloud: as a voice message in reply to a chat voice message, and on demand (or at once, after dictation) on the family dashboard and the Console. It does not decide wake words, live conversation or shared household devices; ADR-W042 §8 still holds for those.
- **Depends on / relates to:** ADR-W042 §8 ("Speaches already serves Kokoro at `/v1/audio/speech` … reply in voice only when the person spoke, and per a setting"); [`docs/02-implementation-scope.md`](../02-implementation-scope.md) §7 (Kokoro is the stated text-to-speech preference); AC12 (a reply's text is as private as the question that produced it).

---

## 1. Context

Part one turned speech into text. The same Speaches container also serves Kokoro, a small, natural text-to-speech model that runs well on a CPU, behind the OpenAI speech API (`POST /v1/audio/speech`). OpenAI serves the same API. So part two needs no new container and no new engine client shape, only the other direction.

Speaches 0.8.3 produces MP3, FLAC, WAV or raw PCM; it cannot produce Opus. Every place a reply goes plays MP3: Telegram's `sendVoice` accepts it and shows it as a voice message, Discord, Slack and Matrix play it inline, and every browser plays it. So Wildstone asks every engine for MP3 and, as in part one, never transcodes.

## 2. Decision

### 2.1 When a reply is spoken

- **Chat:** only in answer to a voice message, only when the deployment reads replies aloud (`-speech-replies`), and only when a voice this Agent may use exists. The person gets the text as always, then the voice message. The "🎙️ I heard" line is never spoken.
- **Dashboard and Console:** a listen button on each reply, shown only when a voice this assistant may use exists. The answer to a question the person dictated is read at once; a typed question's answer only when the button is pressed. A browser that will not start sound without a tap gets a plain "press the speaker" hint, never an error.

### 2.2 Privacy: the same rule, both ways

A reply's text is as private as the question. `Router.Speak(allowCloud, text)` uses exactly the ADR-W042 §2.2 step 4 answer: a cloud voice only for an Agent that already sends what it is told out of the house. An Agent on a local model, or with only a cloud voice configured, gets no voice at all — never a quiet cloud fallback. For chat the answer is taken again when the audio is made, not trusted from when the reply was queued, and the reply's own privacy chain (`checkOutboundPrivacy`) is re-checked: a reply that may no longer go to this Connection is not spoken.

### 2.3 Chat: the adapter delivers, Keystone speaks

```text
voice message ─► … ─► answer queued with speech text (migration 0091)
adapter: poll ─► ready ─► send text ─► ack ─► POST /chat/outbound/{seq}/speech ─► SendVoice
                                                   │
                                     Keystone: same Connection? unexpired? person still admitted?
                                               privacy still allows it? engine by the Agent's class
                                               → audio/mpeg, and the speech text is cleared (once only)
```

- The audio is made **after** the text is delivered and acked, so a slow or failed voice never delays or repeats the answer, and nothing is synthesised for a reply stopped before delivery.
- The adapter never talks to a speech engine; it carries bytes, as in part one.
- Each voice reply is taken once: Keystone clears the text as it reads it.

### 2.4 Making text speakable

`speech.SpeakableText` turns an answer written to be read into one that can be heard: code blocks become "there's code in the message", links their text, bare URLs "a link", list and heading marks are dropped, table rows read as lists, and each line ends as a sentence. Anything past 1,200 characters (about a minute and a half) is cut at a sentence end with "The rest is in the message." A reply is something to hear while doing something else, not an audiobook.

### 2.5 What is kept

Audio: never. It is made in memory for one request and returned. The chat reply row holds the text to speak until the adapter takes it (at most the reply's own lifetime). Audit: one event per use — `chat.voice_reply`, `assistant.read_aloud` or `console.read_aloud` — naming the Agent, the engine and the outcome, never the words.

## 3. Interfaces

### 3.1 Keystone

| Endpoint | Caller | Body | Result |
|---|---|---|---|
| `POST /api/v1/chat/outbound/{seq}/speech` | chat adapter (`VerbConverse`, its own Connection only) | none | `audio/mpeg`, once; 404 when there is nothing to speak |
| `POST /api/v1/assistant/speak` | portal pages (same sign-in, same-site rule and assistant ceiling as `/assistant/ask`) | `{"agent","text"}` | `audio/mpeg` |
| `POST /api/v1/butlers/{name}/speak` | Console (same access as Console dictation) | `{"text"}` | `audio/mpeg` |

The audio is the response body itself (`audioResponse`), the one non-JSON body: the words were redacted before they were spoken, and a redaction pass over base64 audio would only corrupt it. `GET /api/v1/assistant/agents` and `GET /api/v1/butlers/{name}/status` say whether the listen button works (`readAloud`). A polled chat reply carries `speak: true`, never the text to speak.

`keystone/speech` gains `Speaker` (`Processing()`, `Speak(ctx, text) ([]byte, error)`), `Client.Speak` (with a `Voice` field), and on the Router `ReplyMode`, `LocalVoice`, `CloudVoice`, `CanSpeak`, `Speak`. Transcription and speech share the Router's one-at-a-time slot: they run on the same CPU. `Warm` now also downloads the voice model.

### 3.2 Chat adapter

An optional interface, so `Transport` keeps its methods (ADR-W026 G5):

```go
type VoiceSender interface {
	SendVoice(ctx context.Context, conversation string, audio []byte, mimeType string) error
}
```

| Transport | How |
|---|---|
| Telegram | `sendVoice`, multipart, `reply.mp3`. Errors are stripped of their URL, which carries the token. |
| Discord | `POST /channels/{id}/messages`, multipart with `payload_json` and `files[0]`. |
| Slack | `files.getUploadURLExternal` → upload to the returned URL (followed only on `files.slack.com`, and without the bot token) → `files.completeUploadExternal`. Needs the **`files:write`** scope; without it the log names the scope to add. |
| Matrix | `POST /_matrix/media/v3/upload`, then an `m.audio` event marked `org.matrix.msc3245.voice` so clients show a voice message. |

### 3.3 Settings

| Flag | Default | Meaning |
|---|---|---|
| `-speech-replies` | `off` | `off`, `local`, `local-then-cloud` or `cloud`: separate from `-speech-mode`, so a house can hear without speaking or the reverse. |
| `-speech-reply-model` | `speaches-ai/Kokoro-82M-v1.0-ONNX` | The local voice model, served by the engine at `-speech-url`. |
| `-speech-reply-voice` | `af_heart` | The Kokoro voice (e.g. `am_micah`, `bf_emma`). |
| `-speech-reply-cloud-model` | `gpt-4o-mini-tts` | The OpenAI model. |
| `-speech-reply-cloud-voice` | `coral` | The OpenAI voice. |

## 4. Consequences

- One more use of the one Speaches container and the one privacy rule; no new package and no new egress beyond OpenAI's speech endpoint, refused for a local-only Agent.
- Slack households that want voice replies must add `files:write` and reinstall the app; without it replies are text only and the adapter log says why.
- The portal CSP gains `media-src blob:`, for audio the page itself made from Keystone's response.
- A spoken reply costs a few seconds of CPU per answer; the one-at-a-time slot keeps it from competing with transcription.

## 5. Risks

| Risk | Answer |
|---|---|
| A local-only Agent's answer read by a cloud voice | §2.2; covered by tests for chat, dashboard and Console. |
| The text to speak outlives the reply | Taken once and cleared; expires with the reply. |
| The listen endpoint used to synthesise arbitrary text | Only by a signed-in person, for an assistant they already reach, bounded and cut to 1,200 characters — nothing they could not have by asking it. |
| A voice reply loop (the bot hearing its own voice note) | Every transport already ignores the bot's own messages. |

## 6. The 🎙️ marker (ADR-W042's deferred item)

A person's turn now records how it arrived: `butler_session_turns.source` is `voice` for a chat voice message or dictation that was sent, `''` for typed text (migration 0090). The dashboard and Console send `voice: true` with a dictated message (the text box remembers it was dictated until it is emptied or sent). The conversations page, the dashboard, the widget and the Console show a 🎙️ beside such turns, so a mis-hearing reads as one. It is a label only: it grants and withholds nothing.
