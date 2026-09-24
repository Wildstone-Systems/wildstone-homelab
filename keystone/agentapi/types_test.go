package agentapi

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode"
)

// A fixed instant, in UTC so a round trip compares equal: a local
// time carries a location pointer DeepEqual will not forgive.
var at = time.Date(2026, 9, 24, 9, 12, 0, 0, time.UTC)

// fixtures is one fully populated value of every type on the wire.
// Every field is set to something other than its zero value, so an
// omitempty tag that dropped a field, or a tag that misspelled one,
// would show up as a round trip that does not come back equal.
func fixtures() []any {
	return []any{
		ToolSpec{Name: "get_household_events", Description: "reads the timeline",
			InputSchema: json.RawMessage(`{"type":"object"}`), ReadOnlyHint: true},
		ActionSpec{Tool: "do_household_action", Tier: "domestic"},
		Manifest{AgentClass: AgentClassMember, SystemPrompt: "You are the household's.",
			Tools:           []ToolSpec{{Name: "recall", InputSchema: json.RawMessage(`{}`)}},
			ReadOnly:        true,
			ProposableKinds: []string{"Route"},
			Actions:         []ActionSpec{{Tool: "lights", Tier: "domestic"}},
			Unavailable:     []string{"lights: no executor wired"},
			Fingerprint:     "0123456789abcdef"},
		InvokeRequest{Handle: "task-1.s3cret", Tool: "recall",
			Args: json.RawMessage(`{"text":"bins"}`), IdempotencyKey: "k-1"},
		InvokeResult{Outcome: OutcomeWaiting, Content: "asked", Reason: "confirm tier",
			WaitReason: WaitConfirmPending, Proposal: &ProposalRef{ID: "prop-7"},
			ServedFromLedger: true},
		ProposalRef{ID: "prop-7"},
		RecallRequest{Handle: "task-1.s3cret", Text: "bins", Kind: MemoryPreference, Limit: 5},
		MemoryHit{Content: "bins go out Tuesday", Kind: MemoryFact, Scope: MemoryScopePersonal,
			Subject: "mika", Source: "conversation", Audience: AudienceOwner,
			Confidence: "stated", ObservedAt: at, MustVerifyLive: true},
		RememberRequest{Handle: "task-1.s3cret", Scope: MemoryScopeAgentLocal,
			Kind: MemoryPreference, Subject: "me", Content: "prefers mornings"},
		Checkpoint{Handle: "task-1.s3cret", Step: 3, Kind: StepTool, Name: "recall",
			State: StepVerified, Params: json.RawMessage(`{"text":"bins"}`), Attempt: 4,
			Result: json.RawMessage(`"ok"`), Error: "none"},
		CheckpointReceipt{Step: 3, Attempt: 4, Epoch: 2},
		WorkStart{Handle: "task-1.s3cret", Reason: "delivery"},
		WorkWait{Handle: "task-1.s3cret", Reason: WorkWaitReply, Detail: "asked the owner"},
		Evidence{Kind: "observation", Description: "lights off", SourceID: "hue:1",
			ReceiptID: 9, ReceiptStep: 3, ProposalOnly: true},
		WorkComplete{Handle: "task-1.s3cret", Summary: "done",
			Evidence: []Evidence{{Kind: "observation", SourceID: "hue:1"}}},
		WorkFail{Handle: "task-1.s3cret", Reason: "model unavailable"},
		Delivery{ID: "d-1", Kind: DeliverySubscription, Subscription: "doorbell-v2",
			Cursor: "seq:41", Handle: "task-2.s3cret", Payload: json.RawMessage(`{"type":"doorbell"}`),
			Attempt: 2, ExpiresAt: at},
		Heartbeat{At: at, Version: "hermes 0.9", Cursor: "seq:41"},
		Negotiated{Capabilities: []string{"recall"}, Delivery: DeliveryPull,
			ProcessingClass: ProcessingLocalOnly},
		RuntimeRecord{Type: RuntimeHermes, Endpoint: "https://hermes-mika.internal/",
			CredentialRef: "principal/runtime-mika-3", Health: HealthConnected,
			LastHeartbeat: at,
			Negotiated: Negotiated{Capabilities: []string{"recall", "remember"},
				Delivery: DeliveryPush, ProcessingClass: ProcessingCloud},
			ExecutionProfileRef: "platform-only"},
		ProposeRequest{Handle: "task-1.s3cret", Kind: "Route",
			Document: json.RawMessage(`{"kind":"Route"}`), Rationale: "asked for"},
		NotifyRequest{Handle: "task-1.s3cret", Text: "the bins are out", DedupeKey: "bins"},
	}
}

func TestEveryTypeRoundTripsThroughJSON(t *testing.T) {
	for _, in := range fixtures() {
		typ := reflect.TypeOf(in)
		data, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("%s: marshal: %v", typ, err)
		}
		out := reflect.New(typ)
		if err := json.Unmarshal(data, out.Interface()); err != nil {
			t.Fatalf("%s: unmarshal %s: %v", typ, data, err)
		}
		if got := out.Elem().Interface(); !reflect.DeepEqual(got, in) {
			t.Errorf("%s: round trip changed the value\n in: %#v\nout: %#v\njson: %s", typ, in, got, data)
		}
	}
}

// The ADR's YAML is the wire form. A tag that is not lowerCamel, or a
// field that fell back to its Go name, would be a second spelling of
// the same document.
func TestEveryFieldHasALowerCamelJSONTag(t *testing.T) {
	seen := map[reflect.Type]bool{}
	var check func(typ reflect.Type)
	check = func(typ reflect.Type) {
		for typ.Kind() == reflect.Ptr || typ.Kind() == reflect.Slice {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct || seen[typ] || typ.PkgPath() != reflect.TypeOf(Manifest{}).PkgPath() {
			return
		}
		seen[typ] = true
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			switch {
			case name == "":
				t.Errorf("%s.%s has no json tag", typ.Name(), f.Name)
			case !unicode.IsLower(rune(name[0])), strings.ContainsAny(name, "_-"):
				t.Errorf("%s.%s json tag %q is not lowerCamel", typ.Name(), f.Name, name)
			}
			check(f.Type)
		}
	}
	for _, v := range fixtures() {
		check(reflect.TypeOf(v))
	}
}

// The runtime record's wire form is the ADR §3.3 document, key for key.
func TestRuntimeRecordMatchesTheADRDocument(t *testing.T) {
	data, err := json.Marshal(RuntimeRecord{Type: RuntimeNative, CredentialRef: "principal/runtime-mika-1",
		Health: HealthUnregistered})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"native","credentialRef":"principal/runtime-mika-1","health":"unregistered"}`
	if string(data) != want {
		t.Errorf("native record:\n got %s\nwant %s", data, want)
	}
	data, err = json.Marshal(RuntimeRecord{Type: RuntimeHermes, Endpoint: "https://hermes-mika.internal/",
		CredentialRef: "principal/runtime-mika-3", Health: HealthConnected, LastHeartbeat: at,
		Negotiated:          Negotiated{Delivery: DeliveryPush, ProcessingClass: ProcessingLocalOnly},
		ExecutionProfileRef: "platform-only"})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"type":"hermes"`, `"endpoint":"https://hermes-mika.internal/"`,
		`"lastHeartbeat":"2026-09-24T09:12:00Z"`, `"negotiated":{"delivery":"push","processingClass":"local_only"}`,
		`"executionProfileRef":"platform-only"`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("hermes record %s lacks %s", data, key)
		}
	}
}

// An invoke result that is simply ok carries nothing but the outcome
// and its content: no empty wait reason, no null proposal.
func TestOKResultOmitsWhatItDoesNotHave(t *testing.T) {
	data, err := json.Marshal(InvokeResult{Outcome: OutcomeOK, Content: "42"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"outcome":"ok","content":"42"}`; string(data) != want {
		t.Errorf("got %s want %s", data, want)
	}
}

// Every enum, its full constant set, and one value that must fail —
// including the empty string, which is what an omitted field decodes
// to and which no handler may treat as a default.
func TestEnumsAreClosed(t *testing.T) {
	cases := []struct {
		name  string
		valid []interface{ Valid() bool }
		bad   []interface{ Valid() bool }
	}{
		{"AgentClass",
			[]interface{ Valid() bool }{AgentClassOwner, AgentClassMember},
			[]interface{ Valid() bool }{AgentClass(""), AgentClass("admin"), AgentClass("Owner")}},
		{"Outcome",
			[]interface{ Valid() bool }{OutcomeOK, OutcomeDenied, OutcomeWaiting, OutcomeRejected, OutcomeError},
			[]interface{ Valid() bool }{Outcome(""), Outcome("success"), Outcome("OK")}},
		{"WaitReason",
			[]interface{ Valid() bool }{WaitApprovalPending, WaitConfirmPending, WaitRuntimeOffline, WaitBudgetExhausted},
			[]interface{ Valid() bool }{WaitReason(""), WaitReason("reply"), WaitReason("approval-pending")}},
		{"MemoryScope",
			[]interface{ Valid() bool }{MemoryScopePersonal, MemoryScopeHousehold, MemoryScopeAgentLocal},
			[]interface{ Valid() bool }{MemoryScope(""), MemoryScope("agent-local"), MemoryScope("global")}},
		{"MemoryKind",
			[]interface{ Valid() bool }{MemoryFact, MemoryPreference, MemoryInference, MemoryProcedure},
			[]interface{ Valid() bool }{MemoryKind(""), MemoryKind("opinion")}},
		{"StepKind",
			[]interface{ Valid() bool }{StepModel, StepTool, StepWait, StepDelivery},
			[]interface{ Valid() bool }{StepKind(""), StepKind("thought")}},
		{"StepState",
			[]interface{ Valid() bool }{StepProposed, StepRunning, StepVerified, StepFailed, StepSkipped},
			[]interface{ Valid() bool }{StepState(""), StepState("done")}},
		{"WorkWaitReason",
			[]interface{ Valid() bool }{WorkWaitReply, WorkWaitApproval, WorkWaitSchedule, WorkWaitModelUnavailable,
				WorkWaitBudgetExhausted, WorkWaitClarification, WorkWaitSourceUnavailable, WorkWaitUncertainOutcome},
			[]interface{ Valid() bool }{WorkWaitReason(""), WorkWaitReason("budget_exhausted"), WorkWaitReason("runtime_offline")}},
		{"DeliveryKind",
			[]interface{ Valid() bool }{DeliveryTurn, DeliverySubscription, DeliveryEscalation, DeliveryProposalOutcome},
			[]interface{ Valid() bool }{DeliveryKind(""), DeliveryKind("webhook")}},
		{"RuntimeType",
			[]interface{ Valid() bool }{RuntimeNative, RuntimeHermes, RuntimeOpenClaw},
			[]interface{ Valid() bool }{RuntimeType(""), RuntimeType("Hermes"), RuntimeType("langchain")}},
		{"RuntimeHealth",
			[]interface{ Valid() bool }{HealthUnregistered, HealthConnected, HealthDegraded, HealthOffline, HealthRevoked},
			[]interface{ Valid() bool }{RuntimeHealth(""), RuntimeHealth("healthy"), RuntimeHealth("ok")}},
		{"DeliveryMode",
			[]interface{ Valid() bool }{DeliveryPush, DeliveryPull},
			[]interface{ Valid() bool }{DeliveryMode(""), DeliveryMode("poll")}},
		{"ProcessingClass",
			[]interface{ Valid() bool }{ProcessingLocalOnly, ProcessingCloud},
			[]interface{ Valid() bool }{ProcessingClass(""), ProcessingClass("local"), ProcessingClass("local-only")}},
	}
	for _, c := range cases {
		for _, v := range c.valid {
			if !v.Valid() {
				t.Errorf("%s: %q should be valid", c.name, v)
			}
		}
		for _, v := range c.bad {
			if v.Valid() {
				t.Errorf("%s: %q should not be valid", c.name, v)
			}
		}
	}
}

// Enum values decode from JSON as the raw string and are checked with
// Valid afterwards: an unknown value is not a decode error, so a
// handler has to ask. This pins that an unknown value arrives intact
// rather than being coerced.
func TestUnknownEnumValueSurvivesDecodeAndFailsValid(t *testing.T) {
	var r InvokeResult
	if err := json.Unmarshal([]byte(`{"outcome":"maybe","waitReason":"later"}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.Outcome != "maybe" || r.Outcome.Valid() {
		t.Errorf("outcome %q: want it kept and invalid", r.Outcome)
	}
	if r.WaitReason != "later" || r.WaitReason.Valid() {
		t.Errorf("waitReason %q: want it kept and invalid", r.WaitReason)
	}
}

func TestHandleNeverPrintsItsSecret(t *testing.T) {
	h := Handle("task-42.hunter2secret")
	if h.IsZero() {
		t.Fatal("a populated handle is not zero")
	}
	if got := h.TaskID(); got != "task-42" {
		t.Errorf("TaskID = %q", got)
	}
	for _, rendered := range []string{h.String(), h.String() + "", string([]byte(h.String()))} {
		if strings.Contains(rendered, "hunter2") {
			t.Fatalf("String leaked the secret: %q", rendered)
		}
	}
	if got := h.String(); got != "handle(task-42.…)" {
		t.Errorf("String = %q", got)
	}
	// A handle of an unexpected shape is all secret.
	if got := Handle("hunter2secret").String(); got != "handle(…)" {
		t.Errorf("no-dot handle String = %q", got)
	}
	if got := Handle("hunter2secret").TaskID(); got != "" {
		t.Errorf("no-dot handle TaskID = %q", got)
	}
	if !Handle("").IsZero() || Handle("").String() != "handle()" {
		t.Errorf("zero handle: IsZero=%v String=%q", Handle("").IsZero(), Handle("").String())
	}
	// On the wire the bearer value travels whole.
	data, err := json.Marshal(InvokeRequest{Handle: h, Tool: "recall"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"handle":"task-42.hunter2secret"`) {
		t.Errorf("JSON must carry the full handle: %s", data)
	}
}

// The client interface is the contract's surface; a method added
// without a family here is a method nobody documented which Wildstone
// side enforces it.
func TestClientSurfaceIsTheDocumentedOne(t *testing.T) {
	typ := reflect.TypeOf((*Client)(nil)).Elem()
	want := []string{"Checkpoint", "Complete", "Fail", "Heartbeat", "Invoke", "Manifest",
		"Notify", "Propose", "Recall", "Remember", "Start", "Wait"}
	var got []string
	for i := 0; i < typ.NumMethod(); i++ {
		got = append(got, typ.Method(i).Name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Client methods = %v, want %v", got, want)
	}
}
