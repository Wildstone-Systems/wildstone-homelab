# ADR-W042: The voice pipeline, part one — speech to text

- **Status:** proposed (research complete; not implemented)
- **Governed by:** ADR-W033 where they overlap (B6 privacy chain, B7 Connections, B9 addressing).
- **Scope:** turning a recorded voice message into the text of an ordinary Agent turn, on Telegram, Discord, Slack and Matrix, and dictation on the family dashboard (the assistant widget and the conversations page). It does not decide text-to-speech replies, wake words, live streaming audio, or shared household devices; §8 records what those will need so part one does not block them.
- **Depends on / relates to:** [`docs/02-implementation-scope.md`](../02-implementation-scope.md) §7 ("Voice remains post-v1. Local transcription and Kokoro text-to-speech are the current preferences, using the same identity/conversation boundary"); [`docs/agent-architecture-baseline.md`](../agent-architecture-baseline.md) "Privacy gateway" and AC12 (transcription is an external processing boundary); [`docs/audit/findings/155-identity.md`](../audit/findings/155-identity.md) F-155-12 (voice ingress must establish a Person before admission); ADR-W026 G5 (a transport is its `Transport` methods and nothing else).
- **Implementation:** none yet. §7 lists the slices and the files each one touches.

---

## 1. Context

Every inbound path carries text only, today:

| Surface | Where non-text is dropped | What a voice message looks like there |
|---|---|---|
| Telegram | `cli/cmd/chat-adapter/telegram.go` — `strings.TrimSpace(u.Message.Text) == ""` → `continue` | `message.voice` (`file_id`, `duration`, `mime_type` `audio/ogg`, Opus). Also `audio` and `video_note`. Bytes via `getFile` then `/file/bot<token>/<path>`; the Bot API caps downloads at 20 MB. |
| Discord | `discord.go` — empty `Content` is skipped; `attachments` is not parsed | Message flag `IS_VOICE_MESSAGE` (1<<13); exactly one attachment, `content_type` `audio/ogg` (mono 48 kHz Opus), with `duration_secs` and `waveform`. The CDN URL is signed and expires. |
| Slack | `slack.go` — any `subtype` (including `file_share`) is skipped | An audio clip is a `file_share` message whose file has `subtype` `slack_audio`, usually `video/mp4` even though it is audio only. Download `url_private_download` with the bot token as a Bearer header; needs the `files:read` scope, which `packages/chat-slack` does not ask for today (`chat:write`, `im:history`). |
| Matrix | `matrix.go` — empty `Content.Body` is skipped | `msgtype: m.audio`, `url: mxc://…`, `info.duration`, often `org.matrix.msc3245.voice`. Download via the authenticated media endpoint. |
| Family dashboard | `keystone/portal/web/assistant*.html` post `{agent, session, text, new}` to `POST /api/v1/assistant/ask`; empty text is a 400 | Nothing yet. Browsers record with `MediaRecorder`: `audio/webm;codecs=opus` (Chrome, Firefox, Edge) or `audio/mp4` (Safari). Needs a secure context (HTTPS), which the portal already has behind the proxy. |

Keystone's own inbound handler, `receiveChatMessage` (`keystone/api/handlers_chat_message.go`), rejects empty text and resolves the sender's binding before anything reaches an Agent. That ordering matters for voice (§2.2).

The house has no audio code, no STT/TTS package, and no GPU: `ContainerSpec` has no device passthrough (ADR-W013 A2), so anything local runs on CPU, like `packages/ai-ollama`.

### 1.1 Engines considered

| Engine | Where it runs | API | Fit |
|---|---|---|---|
| **Speaches** (faster-whisper for STT; Kokoro and Piper for TTS) | Local container, CPU or GPU | OpenAI-compatible `/v1/audio/transcriptions` and `/v1/audio/speech` | **Chosen for local.** "Ollama for speech": one container serves part one now and part two (Kokoro, already the stated TTS preference) later. |
| faster-whisper alone (e.g. `docker-whisper`) | Local container | OpenAI-compatible | Works, but part two would need a second container. |
| whisper.cpp server | Local, very light CPU | Its own `/inference` endpoint | Lightest; a second client shape to maintain. Keep as a fallback if Speaches' footprint is too large on small nodes. |
| Wyoming (`wyoming-faster-whisper`, Piper, openWakeWord) | Local, via Home Assistant | Wyoming TCP protocol | The right tool for *household devices* (§8), not for chat voice notes. |
| OpenAI (`whisper-1`, `gpt-4o-mini-transcribe`) | Cloud | Same OpenAI shape | **Chosen for cloud.** Same client as Speaches; `OPENAI_API_KEY` is already how the house supplies that provider. |
| Browser Web Speech API | The browser vendor's cloud | Browser JS | **Rejected.** Audio leaves the house through Google or Apple and never passes Wildstone's privacy check. |
| Slack's own clip transcript | Slack | Field on the file | **Not used.** Present only sometimes, and content was already processed by a third party we cannot classify. |

Because the local and cloud engines speak one API, Wildstone needs one Go client with two base URLs, not two integrations.

Every engine decodes Ogg/Opus, WebM and MP4 itself (faster-whisper uses PyAV/FFmpeg). **Wildstone never transcodes**: the distroless chat adapter stays free of FFmpeg.

---

## 2. Decision

### 2.1 Shape: adapters fetch, Keystone transcribes

```text
voice note ─► chat adapter ──(download bytes: it holds the bot token)──┐
                                                                        ▼
browser mic ─► portal page ─────────────────────────────► Keystone: binding check
                                                               │  (unbound → refused, audio discarded)
                                                               ▼
                                                        privacy check (local | cloud allowed?)
                                                               │
                                                               ▼
                                                        Transcriber ─► Speaches (local) or OpenAI (cloud)
                                                               │
                                                               ▼
                                                        text → the SAME turn path as a typed message
```

- **The adapter only downloads.** It already holds the vendor token, and Keystone holds that token write-only, so only the adapter can fetch the file. It never talks to an STT engine.
- **Keystone does the transcription.** One place for the binding check, the privacy decision, limits, audit, and the engine choice. The portal uses the same code path. An adapter compromise cannot choose a cloud engine.

Rejected: transcribing inside each adapter (four copies of the privacy rule, and adapters would need STT reachability and credentials); sending the vendor file URL to Keystone (Keystone cannot authenticate to the vendor, and Discord URLs expire).

### 2.2 Order of checks (fail closed, cheapest first)

1. **Metadata limits, in the adapter, before download:** vendor-reported duration ≤ `voice_max_seconds` (default 120) and size ≤ 10 MB. Over the limit → a short "that's too long, please send a shorter message or type it" reply, no download.
2. **Addressing:** a voice message in a group is addressed only when it replies to the bot (you cannot @mention by voice). Direct messages are always addressed. Unaddressed group audio is ignored like unaddressed text.
3. **Binding, in Keystone, before transcription** (F-155-12): the account must resolve to a live binding for this Connection. An unbound account's audio is discarded untranscribed — no CPU spent and nothing sent to a cloud engine for a stranger.
4. **Privacy (AC12):** the Transcriber has a processing class, `local` or `cloud`. `cloud` is used only when the Agent's and the Connection's privacy classification allow cloud processing. If they do not and no local engine is installed, the reply is "voice isn't available here yet; please type" — never a silent cloud fallback. If the policy check itself fails, stop.
5. **Transcribe**, with a timeout (default 60 s). Empty or failed → a short "I couldn't make that out" reply.
6. **Hand the text to the normal turn path** (`receiveChatMessage`'s body after the text check; `assistantAsk` for the portal).

### 2.3 A transcript is speech, not a command

A transcript is **never** parsed as a slash command, and never enters secret intake (`prepareSecretInput`, `/secret …`). Saying "slash secret household wifi equals …" becomes an ordinary message, not a stored secret. Speech recognisers mis-hear; commands and secrets need typing.

### 2.4 What is kept

- **Audio: never stored.** It lives in memory for one request and is dropped. Not written to disk, logs, traces, or the runtime DB.
- **Transcript: stored as the message text**, like typed text, plus two fields on the turn: `source: voice` and `audio_seconds`. The conversations page shows a mic icon on those messages.
- **Audit:** one event per transcription — Connection, Person, engine (`local`/`cloud`), duration, outcome. No text, no audio.

### 2.5 Dashboard dictation, not voice turns

On the widget and the conversations page, the mic button **dictates into the text box**; the person reads it, fixes it, and presses Send. It does not send on its own. That gives the dashboard a correction step chat apps cannot offer, and keeps `POST /api/v1/assistant/ask` unchanged.

---

## 3. Interfaces

### 3.1 Chat adapter

`Incoming` gains one optional field; `Transport` keeps its methods:

```go
// Voice is set when the message is a voice note instead of text. Text
// is then empty. The adapter has already downloaded the bytes.
Voice *VoiceNote

type VoiceNote struct {
	MIME    string        // as the vendor labelled it, e.g. audio/ogg, video/mp4
	Seconds float64       // vendor-reported duration, checked before download
	Audio   []byte        // ≤ 10 MB
}
```

`keystone.go` sends a message with `Voice` set to the new endpoint below instead of `/api/v1/chat/messages`.

### 3.2 Keystone

| Endpoint | Who calls it | Body | Result |
|---|---|---|---|
| `POST /api/v1/chat/voice` | chat adapter (same principal and verb, `VerbConverse`, as `/chat/messages`) | `multipart/form-data`: part `message` = today's `inboundMessage` JSON minus `text`, plus `audioSeconds`; part `audio` = the bytes with their MIME type | Same as `/chat/messages` |
| `POST /api/v1/assistant/transcribe` | portal page (`s.portal(surfaceAssistant, …)`) | `multipart/form-data`: `agent`, `audio` | `{"text": "…"}` — nothing queued, no turn |

Multipart, not base64 JSON: 33 % smaller and it keeps the existing `decodeStrict` JSON handlers untouched.

A new package `keystone/speech`:

```go
type Transcriber interface {
	Class() Processing                          // Local | Cloud
	Transcribe(ctx context.Context, audio []byte, mime, language string) (string, error)
}
```

One implementation, `speech.OpenAICompatible{BaseURL, Model, Key}`, serves both Speaches (`http://speaches:8000`, no key) and OpenAI (`https://api.openai.com`, `OPENAI_API_KEY` from env, as ADR-W017 A4 requires for provider keys).

### 3.3 Settings

Per household, in the Console's Agent settings: `voice: off | local | local-then-cloud | cloud` (default `off` until a Speaches package is installed, then `local`), `voice_max_seconds` (120), `voice_language` (`auto`). Per Connection: voice on/off (default on for DMs). `local-then-cloud` still obeys §2.2 step 4 per turn.

---

## 4. The `speech-speaches` package

`packages/speech-speaches/package.yaml`, modelled on `ai-ollama`:

- capability `speech`, profile `speaches`; image pinned by digest (ADR-W003 A4.2) and in `packages/versions.lock`.
- **No Route**: an unauthenticated audio API reached only as `http://speaches:8000` on the Wildstone network.
- A `speaches-models` volume outside any BackupPlan (models re-download).
- Params: `node`, `stt_model` (default a CPU-friendly int8 faster-whisper *small*; `distil-small.en` for English-only homes; *large-v3-turbo* for a strong CPU), `keep_alive`.
- `policies.classifications: [internal_only]`.
- CPU only, for the same reason as Ollama. A 30-second note on a small model takes a few seconds on a modern CPU; that is acceptable for voice notes, not for live conversation (§8).

---

## 5. Consequences

- One privacy rule, one engine client, one audit trail for four transports and the dashboard.
- Adapters grow a download path per vendor and a larger memory peak (≤ 10 MB per message).
- Slack households must re-install their Slack app with `files:read`; the package comment and the Console's setup text change to say so.
- Discord already asks for Message Content, which covers attachments; no new intent.
- Reaching a cloud STT engine is a new egress destination from Keystone; it is refused when the household is local-only.

---

## 6. Risks

| Risk | Answer |
|---|---|
| Someone else's voice on a bound account (a child using a parent's phone) | No new risk: the same account could type. Voice is admitted exactly as text is. Shared devices are a different problem (§8). |
| Mis-heard words cause an action | The Agent's existing proposal/confirmation rules apply unchanged; commands and secret intake are unreachable by voice (§2.3). |
| A large file exhausts memory or CPU | Duration and size checked before download; one transcription at a time per node (a queue, not a pool); 60 s timeout. |
| Cloud fallback leaks local-only content | §2.2 step 4; covered by an AC12 test case. |
| Audio kept by accident | No write path exists; a test asserts nothing audio-shaped reaches the DB or logs. |

---

## 7. Implementation slices (part one)

Each slice is one PR with its own tests.

1. **`keystone/speech` + `speech-speaches` package.** `Transcriber`, the OpenAI-compatible client, settings, audit event; the package. Tests with a fake HTTP engine (success, timeout, empty, error) and the privacy matrix.
2. **`POST /api/v1/chat/voice`.** Multipart parsing with limits, binding-before-transcription, privacy check, handoff to the shared turn path, `source: voice` on the turn, no-command rule. Tests beside `handlers_chat_message_test.go`, including "unbound account never reaches the Transcriber" and "`/secret` by voice is not intake".
3. **Telegram.** `voice`, `audio`, `video_note`; `getFile` download. Tests with recorded update fixtures.
4. **Dashboard dictation.** `POST /api/v1/assistant/transcribe`; mic button in `assistant-widget.html` and `assistant.html` (MediaRecorder, a visible recording state, a stop button, a 120 s cap, clear errors when the mic is denied); mic icon on voice turns in the conversation view.
5. **Discord.** Voice-message flag and single attachment download.
6. **Slack.** `file_share` + `slack_audio`, Bearer download, `files:read` in the package text.
7. **Matrix** (not asked for, same shape): `m.audio` download via authenticated media.

Slices 3, 5, 6 and 7 depend on 2; slice 4 depends on 1 only, so it can run alongside 2 and 3.

---

## 8. Part two and later: what this leaves room for

- **Spoken replies (TTS).** Speaches already serves Kokoro at `/v1/audio/speech`. `Transport.Send` needs an optional voice-reply method (Telegram `sendVoice`, Discord voice message upload, Slack file upload). Reply in voice only when the person spoke, and per a setting.
- **Household devices and wake words.** Route through Home Assistant Assist and the Wyoming protocol (openWakeWord, `wyoming-faster-whisper`, Piper), with satellites such as the Home Assistant Voice PE talking over the network — no USB passthrough needed. `packages/home-automation` would expose an Assist conversation agent that forwards to the Wildstone Agent.
- **Before any shared device ships**, F-155-12 must be closed: a new *ambiguous speaker* class that gets only configured basic household actions, because a kitchen speaker is not a bound Person.
- **Live conversation** needs streaming STT and a GPU (device passthrough, ADR-W013 A2) to be pleasant; out of scope until then.
