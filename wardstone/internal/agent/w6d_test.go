package agent

// ADR-W003 W6d: system ops — SystemdUnitStatus, TimeSyncStatus. D-Bus is
// unavailable in CI containers, so SystemdUnitStatus tests fake the
// systemdClient seam and never dial a real system bus.

import (
	"context"
	"sync"
	"testing"
	"time"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// ---------------------------------------------------------------- SystemdUnitStatus

// fakeSystemdClient returns canned property maps instead of talking to a
// real system bus.
type fakeSystemdClient struct {
	props map[string]any
	err   error
	calls int
}

func (f *fakeSystemdClient) GetUnitPropertiesContext(_ context.Context, _ string) (map[string]any, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.props, nil
}

func (f *fakeSystemdClient) Close() {}

// stubSystemdConn installs a seam that hands back client (or dialErr, if
// non-nil, without ever calling client) and records how many times the
// connection was dialed.
func stubSystemdConn(t *testing.T, client *fakeSystemdClient, dialErr error) *int {
	t.Helper()
	orig := systemdConn
	t.Cleanup(func() { systemdConn = orig })
	dials := 0
	systemdConn = func(ctx context.Context) (systemdClient, error) {
		dials++
		if dialErr != nil {
			return nil, dialErr
		}
		return client, nil
	}
	return &dials
}

func TestSystemdUnitStatusAllowlisted(t *testing.T) {
	h := startGateway(t, []string{"tok"}, "")
	fake := &fakeSystemdClient{props: map[string]any{
		"ActiveState":          "active",
		"SubState":             "running",
		"LoadState":            "loaded",
		"ActiveEnterTimestamp": uint64(1_700_000_000_000_000), // microseconds
	}}
	dials := stubSystemdConn(t, fake, nil)

	newTestAgent(t, h, "node-a", "tok", nil)
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_SystemdUnitStatus{
		SystemdUnitStatus: &wsv1.SystemdUnitStatus{Unit: "wardstoned.service"},
	}})
	sr := res.GetSystemdUnitStatus()
	if sr == nil {
		t.Fatalf("systemd unit status: %+v", res)
	}
	if sr.GetActiveState() != "active" || sr.GetSubState() != "running" || sr.GetLoadState() != "loaded" {
		t.Fatalf("unexpected fields: %+v", sr)
	}
	if want := uint64(1_700_000_000); sr.GetActiveSinceUnixSec() != want {
		t.Fatalf("us->s conversion: want %d, got %d", want, sr.GetActiveSinceUnixSec())
	}
	if *dials != 1 || fake.calls != 1 {
		t.Fatalf("want exactly one dial and one call, got dials=%d calls=%d", *dials, fake.calls)
	}
}

func TestSystemdUnitStatusNonAllowlistedIsForbiddenAndNeverDials(t *testing.T) {
	h := startGateway(t, []string{"tok"}, "")
	fake := &fakeSystemdClient{props: map[string]any{"ActiveState": "active"}}
	dials := stubSystemdConn(t, fake, nil)

	newTestAgent(t, h, "node-a", "tok", nil)
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_SystemdUnitStatus{
		SystemdUnitStatus: &wsv1.SystemdUnitStatus{Unit: "sshd.service"},
	}})
	if res.GetError().GetCode() != wsv1.ErrorCode_ERROR_CODE_FORBIDDEN {
		t.Fatalf("non-allowlisted unit: want FORBIDDEN, got %+v", res)
	}
	if *dials != 0 || fake.calls != 0 {
		t.Fatalf("non-allowlisted unit must never dial systemd: dials=%d calls=%d", *dials, fake.calls)
	}
}

func TestSystemdUnitStatusEmptyUnitIsInvalid(t *testing.T) {
	h := startGateway(t, []string{"tok"}, "")
	fake := &fakeSystemdClient{}
	dials := stubSystemdConn(t, fake, nil)

	newTestAgent(t, h, "node-a", "tok", nil)
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_SystemdUnitStatus{
		SystemdUnitStatus: &wsv1.SystemdUnitStatus{Unit: ""},
	}})
	if res.GetError().GetCode() != wsv1.ErrorCode_ERROR_CODE_INVALID {
		t.Fatalf("empty unit: want INVALID, got %+v", res)
	}
	if *dials != 0 {
		t.Fatalf("empty unit must never dial systemd: dials=%d", *dials)
	}
}

func TestSystemdUnitStatusAllowlistIsConfigurable(t *testing.T) {
	h := startGateway(t, []string{"tok"}, "")
	fake := &fakeSystemdClient{props: map[string]any{"ActiveState": "active"}}
	stubSystemdConn(t, fake, nil)

	newTestAgent(t, h, "node-a", "tok", func(c *Config) {
		c.SystemdUnitAllowlist = []string{"custom.service"}
	})
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	// Default-allowlisted unit is now refused because the config overrode
	// the default list.
	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_SystemdUnitStatus{
		SystemdUnitStatus: &wsv1.SystemdUnitStatus{Unit: "docker.service"},
	}})
	if res.GetError().GetCode() != wsv1.ErrorCode_ERROR_CODE_FORBIDDEN {
		t.Fatalf("docker.service outside configured allowlist: want FORBIDDEN, got %+v", res)
	}

	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_SystemdUnitStatus{
		SystemdUnitStatus: &wsv1.SystemdUnitStatus{Unit: "custom.service"},
	}})
	if res.GetSystemdUnitStatus() == nil {
		t.Fatalf("custom.service in configured allowlist: %+v", res)
	}
}

func TestSystemdUnitStatusConnectionErrorIsRuntime(t *testing.T) {
	h := startGateway(t, []string{"tok"}, "")
	stubSystemdConn(t, nil, errDialFailed)

	newTestAgent(t, h, "node-a", "tok", nil)
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_SystemdUnitStatus{
		SystemdUnitStatus: &wsv1.SystemdUnitStatus{Unit: "wardstoned.service"},
	}})
	if res.GetError().GetCode() != wsv1.ErrorCode_ERROR_CODE_RUNTIME {
		t.Fatalf("dbus connect failure: want RUNTIME, got %+v", res)
	}
}

func TestSystemdUnitStatusPropertyFetchErrorIsRuntime(t *testing.T) {
	h := startGateway(t, []string{"tok"}, "")
	fake := &fakeSystemdClient{err: errDialFailed}
	stubSystemdConn(t, fake, nil)

	newTestAgent(t, h, "node-a", "tok", nil)
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_SystemdUnitStatus{
		SystemdUnitStatus: &wsv1.SystemdUnitStatus{Unit: "wardstoned.service"},
	}})
	if res.GetError().GetCode() != wsv1.ErrorCode_ERROR_CODE_RUNTIME {
		t.Fatalf("GetUnitPropertiesContext failure: want RUNTIME, got %+v", res)
	}
}

var errDialFailed = &fakeSystemdErr{"simulated dbus failure"}

type fakeSystemdErr struct{ msg string }

func (e *fakeSystemdErr) Error() string { return e.msg }

// ---------------------------------------------------------------- TimeSyncStatus

func stubAdjtimex(t *testing.T, fn func() (int, error)) {
	t.Helper()
	orig := adjtimexFn
	t.Cleanup(func() { adjtimexFn = orig })
	var mu sync.Mutex
	adjtimexFn = func() (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return fn()
	}
}

func TestTimeSyncStatusSynchronized(t *testing.T) {
	h := startGateway(t, []string{"tok"}, "")
	const timeOK = 0 // unix.TIME_OK
	stubAdjtimex(t, func() (int, error) { return timeOK, nil })

	newTestAgent(t, h, "node-a", "tok", nil)
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_TimeSyncStatus{
		TimeSyncStatus: &wsv1.TimeSyncStatus{},
	}})
	tr := res.GetTimeSyncStatus()
	if tr == nil || !tr.GetSynchronized() || tr.GetDetail() == "" {
		t.Fatalf("time sync status (TIME_OK): %+v", res)
	}
}

func TestTimeSyncStatusNotSynchronized(t *testing.T) {
	h := startGateway(t, []string{"tok"}, "")
	const timeError = 5 // unix.TIME_ERROR
	stubAdjtimex(t, func() (int, error) { return timeError, nil })

	newTestAgent(t, h, "node-a", "tok", nil)
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_TimeSyncStatus{
		TimeSyncStatus: &wsv1.TimeSyncStatus{},
	}})
	tr := res.GetTimeSyncStatus()
	if tr == nil || tr.GetSynchronized() || tr.GetDetail() == "" {
		t.Fatalf("time sync status (TIME_ERROR): %+v", res)
	}
}
