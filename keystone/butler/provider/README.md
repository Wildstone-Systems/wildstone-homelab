# keystone/butler/provider

Provider-neutral LLM abstraction for the Wildstone Butler (ADR-W006, Spike
S4, work package S4a). This package holds:

- The `Provider` interface and provider-neutral types (`ChatRequest`,
  `ChatResponse`, `Message`, `Tool`, `ToolCall`, `ToolResult`).
- Three backend clients, each stdlib `net/http` + `encoding/json`:
  - `AnthropicProvider` — Anthropic Messages API (`POST /v1/messages`).
  - `OpenAIProvider` — an OpenAI-compatible `chat/completions` endpoint
    (covers LiteLLM, vLLM, OpenRouter, LM Studio).
  - `OllamaProvider` — a local Ollama daemon's native `/api/chat` endpoint.
- `Probe(ctx, provider)` — a scripted tool-reliability eval that returns
  `Full` (the model reliably tool-calls) or `QAOnly` (it doesn't, so it
  should be restricted to read-only Q&A rather than trusted as an
  operator).

## Backends

### Anthropic

```go
p := provider.NewAnthropicProvider(os.Getenv("ANTHROPIC_API_KEY"))
// p.Model defaults to provider.DefaultAnthropicModel ("claude-opus-5").
// p.BaseURL defaults to "https://api.anthropic.com"; override to point at
// a fake or a different endpoint.
```

The key is read from config/env by the caller (never hardcoded, never
logged, and never included in returned errors — see the key-safety test in
`anthropic_test.go`).

### OpenAI-compatible

```go
p := provider.NewOpenAIProvider("https://api.openai.com/v1", os.Getenv("OPENAI_API_KEY"))
// Point BaseURL at a LiteLLM/vLLM/OpenRouter/LM Studio gateway instead of
// the real OpenAI host to use a compatible backend.
```

### Ollama

```go
p := provider.NewOllamaProvider("http://localhost:11434")
// No API key — Ollama is a local, unauthenticated daemon by default.
```

## Real network calls: owner-run only, never CI

Every provider is unit-tested against an `httptest` fake modeling that
backend's request/response shape — no real network call happens in the test
suite, and CI never talks to a real LLM.

Exercising a **real** backend is an **owner-run, manual step**:

- **Anthropic**: requires a real `ANTHROPIC_API_KEY` with billing behind it.
  Run manually, e.g.:

  ```go
  p := provider.NewAnthropicProvider(os.Getenv("ANTHROPIC_API_KEY"))
  resp, err := p.Chat(ctx, provider.ChatRequest{
      Messages:  []provider.Message{{Role: "user", Text: "ping"}},
      MaxTokens: 32,
  })
  ```

- **Ollama**: requires a local Ollama daemon running with a model already
  pulled (`ollama pull llama3.1`). Point `OllamaProvider.Host` at it (default
  `http://localhost:11434`) and set `Model` to whatever you pulled.

- **OpenAI-compatible**: requires a real key/endpoint for whichever
  compatible service you're targeting (OpenAI itself, a LiteLLM gateway,
  etc.).

None of these are wired into `go test ./...` — they require credentials or a
running local daemon that CI does not have, per ADR-W006 A2 ("Every provider
is tested against a fake HTTP server... No real LLM call in CI").

## Reliability probe

```go
verdict, err := provider.Probe(ctx, p)
if verdict == provider.QAOnly {
    // restrict this model to read tools only; no propose_change
}
```

`Probe` sends a single scripted diagnostic task ("a fixture service is
unhealthy; call the tool that fetches its health") with one `get_health`
tool available, and checks whether the model calls it. This is intentionally
self-contained — no dependency on real S2 state or a real provider; the S4b
package supplies the real tool gateway and agent loop that use this
verdict.
