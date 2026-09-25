package native

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// The fake sidecar is this test binary re-executed. It speaks the real
// protocol but follows a script instead of running the SDK, so every
// way a runtime can misbehave — hang, freeze, loop, crash, lie — can be
// staged deterministically, with no Node and no model.

const (
	fakeSidecarEnv = "NATIVE_FAKE_SIDECAR"
	fakeScriptEnv  = "NATIVE_FAKE_SCRIPT"
	fakeCaptureEnv = "NATIVE_FAKE_CAPTURE"
)

// step is one scripted action.
type step struct {
	// Send writes a message.
	Send *message `json:"send,omitempty"`
	// Call sends a tool call and waits for its result.
	Call *message `json:"call,omitempty"`
	// CallForever repeats Call until killed.
	CallForever bool `json:"call_forever,omitempty"`
	// FinalWithLastResult ends with the last tool result as the answer.
	FinalWithLastResult bool `json:"final_with_last_result,omitempty"`
	// Heartbeats starts background heartbeats.
	Heartbeats bool `json:"heartbeats,omitempty"`
	// Turns starts background turn messages (a model that is busy but
	// never finishes).
	Turns   bool   `json:"turns,omitempty"`
	SleepMS int    `json:"sleep_ms,omitempty"`
	Exit    *int   `json:"exit,omitempty"`
	Stderr  string `json:"stderr,omitempty"`
}

// TestFakeSidecar is not a test: it is the fake sidecar's main.
func TestFakeSidecar(t *testing.T) {
	if os.Getenv(fakeSidecarEnv) != "1" {
		t.Skip("helper process")
	}
	os.Exit(fakeSidecarMain())
}

func fakeSidecarMain() int {
	var script []step
	if err := json.Unmarshal([]byte(os.Getenv(fakeScriptEnv)), &script); err != nil {
		fmt.Fprintln(os.Stderr, "bad script:", err)
		return 2
	}
	var mu sync.Mutex
	out := json.NewEncoder(os.Stdout)
	send := func(m message) {
		mu.Lock()
		defer mu.Unlock()
		_ = out.Encode(m)
	}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 64<<10), maxLine)
	read := func() (message, bool) {
		if !in.Scan() {
			return message{}, false
		}
		var m message
		_ = json.Unmarshal(in.Bytes(), &m)
		return m, true
	}

	start, ok := read()
	if !ok || start.Type != msgStart {
		return 3
	}
	if path := os.Getenv(fakeCaptureEnv); path != "" {
		capture := map[string]any{"start": start, "env": os.Environ()}
		b, _ := json.Marshal(capture)
		_ = os.WriteFile(path, b, 0o600)
	}
	send(message{Type: msgReady, Protocol: ProtocolVersion, Engine: "fake", EngineVersion: "0"})

	var last string
	calls := 0
	call := func(c *message) bool {
		calls++
		m := *c
		m.Type = msgToolCall
		m.CallID = fmt.Sprintf("call-%d", calls)
		send(m)
		for {
			r, ok := read()
			if !ok {
				return false
			}
			if r.Type == msgToolRes && r.CallID == m.CallID {
				last = r.Output
				return true
			}
		}
	}
	ticker := func(every time.Duration, m func() message) {
		go func() {
			for range time.Tick(every) {
				send(m())
			}
		}()
	}

	for _, s := range script {
		switch {
		case s.Send != nil:
			send(*s.Send)
		case s.Call != nil && s.CallForever:
			for call(s.Call) {
			}
			return 0
		case s.Call != nil:
			if !call(s.Call) {
				return 0
			}
		case s.FinalWithLastResult:
			send(message{Type: msgFinal, Output: "result: " + last, History: json.RawMessage(`[{"seen":true}]`)})
		case s.Heartbeats:
			ticker(20*time.Millisecond, func() message { return message{Type: msgHeartbeat} })
		case s.Turns:
			n := 0
			ticker(20*time.Millisecond, func() message { n++; return message{Type: msgTurn, Turn: n} })
		case s.SleepMS > 0:
			time.Sleep(time.Duration(s.SleepMS) * time.Millisecond)
		case s.Exit != nil:
			fmt.Fprint(os.Stderr, s.Stderr)
			return *s.Exit
		}
	}
	// Script done: stay alive until Keystone closes stdin or kills us,
	// like a sidecar that has nothing more to say.
	for {
		if _, ok := read(); !ok {
			return 0
		}
	}
}
