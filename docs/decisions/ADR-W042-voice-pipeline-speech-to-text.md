# ADR-W042: The voice pipeline, part one — speech to text

- **Status:** accepted; slices 1–5 and 7 implemented (`keystone/speech`, `packages/speech-speaches`, `-speech-*` flags, `POST /api/v1/chat/voice`, Telegram, dashboard dictation, Discord, Matrix); slice 6 (Slack) not yet
- **Governed by:** ADR-W033 where they overlap (B6 privacy chain, B7 Connections, B9 addressing).
- **Scope:** turning a recorded voice message into the text of an ordinary Agent turn, on Telegram, Discord, Slack and Matrix, and dictation on the family dashboard (the assistant widget and the conversations page). It does not decide text-to-speech replies, wake words, live streaming audio, or shared household devices; §8 records what those will need so part one does not block them.
- **Depends on / relates to:** [`docs/02-implementation-scope.md`](../02-implementation-scope.md) §7 ("Voice remains post-v1. Local transcription and Kokoro text-to-speech are the current preferences, using the same identity/conversation boundary"); [`docs/agent-architecture-baseline.md`](../agent-architecture-baseline.md) "Privacy gateway" and AC12 (transcription is an external processing boundary); [`docs/audit/findings/155-identity.md`](../audit/findings/155-identity.md) F-155-12 (voice ingress must establish a Person before admission); ADR-W026 G5 (a transport is its `Transport` methods and nothing else).
- **Implementation:** slice 1: `keystone/speech` (Transcriber, Client, Router), `keystone/cmd/keystone/speech.go` (flags), `packages/speech-speaches`. Slice 2: `keystone/api/handlers_chat_voice.go`, sharing `chatInbound` with typed messages in `handlers_chat_message.go`. §7 lists the remaining slices.

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

The house has no audio code and no STT/TTS package. Local models ship CPU-only, like `packages/ai-ollama`; `ContainerSpec.devices` can now pass a GPU's render node through, but no shipped package uses it yet.

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
4. **Privacy (AC12):** the Transcriber has a processing class, `local` or `cloud`. `cloud` is used only when the Agent already sends what it is told out of the house: a named Agent assigned a cloud model, and, for an external runtime, declared `cloud` by the Owner (`voiceMayLeaveHouse`). An Agent on a local model, an external runtime that is local-only or undeclared, the legacy single-provider Butler, and any lookup error all answer local. If only a cloud engine is configured, the reply is "I can't listen to voice messages in this conversation right now; please type" — never a silent cloud fallback.
5. **Transcribe**, with a timeout (default 60 s). Empty or failed → a short "I couldn't make that out" reply.
6. **Hand the text to the normal turn path** (`receiveChatMessage`'s body after the text check; `assistantAsk` for the portal).

### 2.3 A transcript is speech, not a command

Speech recognisers mis-hear, so three doors that act on exact words stay closed to voice:

- **Slash commands.** A transcript is never parsed as one. "Slash status" said aloud is a sentence for the Agent.
- **A rule's answer.** A spoken "yes" is not recorded as the household's answer to a rule's question; it goes to the Agent.
- **Secrets.** A transcript that looks like a `/secret` command or a credential ("the alarm code is …") is **not stored and not passed on**. The disclosure is recorded, as for a secret typed in a room, and the reply says the recording is already on the vendor's servers and points to the Secrets page. The Agent never sees it, and the reply never repeats it.

### 2.4 What is kept

- **Audio: never stored.** It lives in memory for one request and is dropped. It is read part by part (not `ParseMultipartForm`, which spills large files to a temp file) and never written to disk, logs, traces, or the runtime DB.
- **Transcript: stored as the message text**, like typed text. The reply starts with `🎙️ I heard: "…"` (clipped to 300 characters), so a mis-hearing shows next to the answer to it. A `source: voice` marker on the stored turn, and a mic icon beside such turns on the conversations page, are deferred: they need a schema change and are cosmetic.
- **Audit:** one `chat.voice` event per voice message that got past the sender and room checks — account, Agent, engine (`local`/`cloud`), reported duration, outcome (`transcribed`, `off`, `unavailable`, `too_long`, `unsupported`, `empty`, `warming`, `timeout`, `failed`). No text, no audio.
- **Duplicates:** the clip is claimed before it is transcribed. Without a vendor message id, a hash of the audio stands in for the text in the dedupe key, so a redelivered clip is heard once.

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

One implementation, `speech.Client{BaseURL, Model, Key, Where, DownloadsModels}`, serves both:

- Speaches: `http://127.0.0.1:18000/v1`, no key, `DownloadsModels: true`. Speaches answers 404 for a model it has not downloaded; the Client then starts `POST /v1/models/{id}` in the background and returns `ErrWarming` ("try again shortly"). Keystone also calls it at startup (`Router.Warm`), retrying until the container is up.
- OpenAI: `https://api.openai.com/v1`. `Key` is a function, read on every call, so a key entered in the Console while Keystone runs is picked up; a missing key is `ErrUnavailable`, and no request goes out.

`speech.Router` holds the mode and both engines and applies §2.2 steps 1, 4 and 5: size and format checks, the local-first choice, the cloud refusal for a turn that may not leave the house (it checks each engine's own `Processing()`, not which slot it was configured in), one transcription at a time, the timeout, and no second opinion from the cloud for a recording the local engine found empty.

### 3.3 Settings

Deployment flags, the same shape as `-ntfy-url` and `-calendar-url`. No flag ever carries a credential.

| Flag | Default | Meaning |
|---|---|---|
| `-speech-mode` | `off` | `off`, `local`, `local-then-cloud` or `cloud`. `local-then-cloud` still obeys §2.2 step 4 on every turn. |
| `-speech-url` | empty | The local engine, e.g. `http://127.0.0.1:18000/v1`. Empty means no local engine. |
| `-speech-model` | `Systran/faster-whisper-small` | The local model. `Systran/faster-distil-whisper-small.en` suits an English-only house. |
| `-speech-cloud-model` | `gpt-4o-mini-transcribe` | The OpenAI model. |
| `-speech-language` | `auto` | Two-letter hint, e.g. `en`. |
| `-speech-max-seconds` | 120 | Longest recording accepted (5–600). |
| `-speech-timeout` | 60s | Longest one transcription may take (5s–10m). |

A value that cannot mean anything stops Keystone at startup. An engine that is named but not usable yet (no key, container not up) is only a warning: Keystone starts, and the Router answers "not available" until it is.

Moving the mode into the Console, and a per-Connection voice switch, are follow-ups once slices 2–4 show what households actually change.

---

## 4. The `speech-speaches` package

`packages/speech-speaches/package.yaml`, modelled on `ai-ollama`:

- capability `speech`, profile `speaches`; image `ghcr.io/speaches-ai/speaches:0.8.3-cpu`, pinned by digest (ADR-W003 A4.2); recorded in `packages/versions.lock`.
- **No Route**: an unauthenticated audio API. Keystone runs on the host, not in a container, so the port is published on `127.0.0.1` only (param `port`, default 18000). The package must therefore be installed on Keystone's own node.
- Hardened: all capabilities dropped, no-new-privileges, a memory ceiling (param `memory`, default `4g`) and a PID limit.
- A `speaches-models` volume outside any BackupPlan (models re-download).
- Params: `node`, `keep_loaded_seconds` (how long the model stays in memory). The model is chosen by Keystone's `-speech-model`, which asks the container to download it, so the choice lives in one place.
- Hardening: the Gradio web UI is off (`ENABLE_UI=false`), inference is CPU `int8`, and the log level is `info`.
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

1. **`keystone/speech` + `speech-speaches` package** *(done)*. `Transcriber`, the OpenAI-compatible client, settings, audit event; the package. Tests with a fake HTTP engine (success, timeout, empty, error) and the privacy matrix.
2. **`POST /api/v1/chat/voice`** *(done)*. Multipart parsing with limits, binding-before-transcription, privacy check, handoff to the shared turn path, the §2.3 rules, the "I heard" line. Tests in `handlers_chat_voice_test.go`, including "a stranger's recording never reaches the engine", "a spoken secret is not stored" and "a local Agent's audio never reaches the cloud".
3. **Telegram** *(done)*. `voice`, `audio` and `video_note` become a `VoiceNote` whose download is deferred until the adapter's own checks pass (flood guard; in a group, only a reply to the bot or an audio caption that mentions it). `getFile` then `/file/bot<token>/<path>`, capped at 10 MB on what Telegram reports and on the bytes read. A recording reported over 10 MB or 600 s is not downloaded but still posted with `tooLarge: true` and no audio, so Keystone can tell a known person "too long" after its sender check, and a stranger nothing. Transport errors are stripped of their URL, which carries the bot token.
4. **Dashboard dictation** *(done)*. `POST /api/v1/assistant/transcribe` (same sign-in, same-site rule and assistant ceiling as `/assistant/ask`; the engine follows the assistant's privacy class) returns `{"text"}` and asks the assistant nothing. `GET /api/v1/assistant/agents` now says per assistant whether the mic works (`dictation`) and how long it may record (`dictationMaxSeconds`). A mic button on the family dashboard's assistant card (`home.html`), the conversations page (`assistant.html`) and the embedded widget records with `MediaRecorder`, turns red while listening, stops itself just short of the limit, and puts the words in the text box to be checked and sent; switching assistant throws a recording away. The button is hidden where it cannot work: no HTTPS, no recorder, or (Chrome) a frame whose parent did not allow the microphone — the Homepage widget frame usually does not, so dictation there is best-effort; the dashboard and conversations page are the supported places. **The Console's Butler conversation page** has the same mic on its opening text box and its mid-conversation text box (`console/src/ui/MicButton.tsx`). It posts to `POST /api/v1/butlers/{name}/transcribe`: the recording is the request body, typed by its `Content-Type` (the Console's one fetch path, as the wallpaper upload uses), with `?seconds=`. Access is the Console ask's: the owner reaches any live Butler, anyone else only a Butler they were granted, and a Butler whose model is down still takes dictation. `GET /api/v1/butlers/{name}/status` says whether the mic works (`dictation`, `dictationMaxSeconds`). Each use is a `console.dictation` event. *Deferred:* the `source: voice` marker and mic icon on stored turns.
5. **Discord** *(done)*. A message flagged `IS_VOICE_MESSAGE` (or one whose only content is an audio attachment) becomes a `VoiceNote`; `duration_secs` and `size` are checked before download. The attachment is fetched from Discord's CDN only (`cdn.discordapp.com`, `media.discordapp.net`, HTTPS), with no credential — the URL is already signed. In a server channel a voice message is addressed only as a reply to the bot. No new intent: Message Content already covers attachments.
6. **Slack.** `file_share` + `slack_audio`, Bearer download, `files:read` in the package text.
7. **Matrix** *(done)*. `msgtype: m.audio` with an `mxc://` URL becomes a `VoiceNote` (`info.duration` is in milliseconds). It is fetched from the bot's own homeserver with its token, from the authenticated media endpoint (`/_matrix/client/v1/media/download`), falling back to the legacy `/_matrix/media/v3/download` only when the homeserver answers `M_UNRECOGNIZED`. The `mxc://` parts are validated before they become a path. Encrypted media (`file` instead of `url`) is skipped, as encrypted text already is. In a group room a voice message is addressed when its `m.mentions` names the bot, which clients set on a reply.

Slices 3, 5, 6 and 7 depend on 2; slice 4 depends on 1 only, so it can run alongside 2 and 3.

---

## 8. Part two and later: what this leaves room for

- **Spoken replies (TTS).** Speaches already serves Kokoro at `/v1/audio/speech`. `Transport.Send` needs an optional voice-reply method (Telegram `sendVoice`, Discord voice message upload, Slack file upload). Reply in voice only when the person spoke, and per a setting.
- **Household devices and wake words.** Route through Home Assistant Assist and the Wyoming protocol (openWakeWord, `wyoming-faster-whisper`, Piper), with satellites such as the Home Assistant Voice PE talking over the network — no USB passthrough needed. `packages/home-automation` would expose an Assist conversation agent that forwards to the Wildstone Agent.
- **Before any shared device ships**, F-155-12 must be closed: a new *ambiguous speaker* class that gets only configured basic household actions, because a kitchen speaker is not a bound Person.
- **Live conversation** needs streaming STT and, to be pleasant, a GPU profile of the speech package (render node via `ContainerSpec.devices`); out of scope until then.
