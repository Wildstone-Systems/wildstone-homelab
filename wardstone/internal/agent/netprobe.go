package agent

// TestNfs, TestSmb, ProbeTcp, ProbeHttp (ADR-W003 W6c). All are pure
// network probes — no mounting, no shell-out. Target allowlisting (which
// hosts/URLs may be probed at all) is Keystone-side policy per the ADR;
// the agent enforces only shape (host/port/scheme sanity) and timeout
// bounds. An unreachable target is a valid, successful answer
// (reachable=false) — never an OpError.

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

const (
	defaultProbeTimeout = 10 * time.Second
	maxProbeTimeout     = 30 * time.Second
	maxProbeHTTPBody    = 64 << 10 // 64 KiB (ADR-W003 §A4.4 bounded responses)
)

// resolveProbeTimeout applies the default/cap rule shared by ProbeTcp and
// ProbeHttp: 0 means the 10s default; anything over 30s is INVALID.
func resolveProbeTimeout(seconds uint32) (time.Duration, *wsv1.OpError) {
	if seconds == 0 {
		return defaultProbeTimeout, nil
	}
	d := time.Duration(seconds) * time.Second
	if d > maxProbeTimeout {
		return 0, opErr(wsv1.ErrorCode_ERROR_CODE_INVALID,
			"timeout_seconds %d exceeds max %d", seconds, int(maxProbeTimeout.Seconds()))
	}
	return d, nil
}

// tcpProbe dials host:port with the given timeout. A dial failure is
// reported via the (false, detail) return, never as a Go error — callers
// treat "unreachable" as a normal result.
func tcpProbe(ctx context.Context, host string, port uint32, timeout time.Duration) (reachable bool, detail string, latency time.Duration) {
	addr := net.JoinHostPort(host, fmt.Sprint(port))
	d := net.Dialer{Timeout: timeout}
	start := time.Now()
	conn, err := d.DialContext(ctx, "tcp", addr)
	latency = time.Since(start)
	if err != nil {
		return false, err.Error(), latency
	}
	conn.Close()
	return true, "", latency
}

func (a *Agent) testNfs(ctx context.Context, op *wsv1.TestNfs) (*wsv1.OpResult_TestNfs, *wsv1.OpError) {
	if op.GetHost() == "" {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "host is required")
	}
	port := op.GetPort()
	if port == 0 {
		port = 2049
	}
	reachable, detail, _ := tcpProbe(ctx, op.GetHost(), port, defaultProbeTimeout)
	return &wsv1.OpResult_TestNfs{TestNfs: &wsv1.TestNfsResult{Reachable: reachable, Detail: detail}}, nil
}

func (a *Agent) testSmb(ctx context.Context, op *wsv1.TestSmb) (*wsv1.OpResult_TestSmb, *wsv1.OpError) {
	if op.GetHost() == "" {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "host is required")
	}
	port := op.GetPort()
	if port == 0 {
		port = 445
	}
	reachable, detail, _ := tcpProbe(ctx, op.GetHost(), port, defaultProbeTimeout)
	return &wsv1.OpResult_TestSmb{TestSmb: &wsv1.TestSmbResult{Reachable: reachable, Detail: detail}}, nil
}

func (a *Agent) probeTcp(ctx context.Context, op *wsv1.ProbeTcp) (*wsv1.OpResult_ProbeTcp, *wsv1.OpError) {
	if op.GetHost() == "" {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "host is required")
	}
	if op.GetPort() < 1 || op.GetPort() > 65535 {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "port %d out of range 1-65535", op.GetPort())
	}
	timeout, oerr := resolveProbeTimeout(op.GetTimeoutSeconds())
	if oerr != nil {
		return nil, oerr
	}
	reachable, detail, latency := tcpProbe(ctx, op.GetHost(), op.GetPort(), timeout)
	return &wsv1.OpResult_ProbeTcp{ProbeTcp: &wsv1.ProbeTcpResult{
		Reachable: reachable,
		LatencyMs: uint32(latency.Milliseconds()),
		Detail:    detail,
	}}, nil
}

func (a *Agent) probeHttp(ctx context.Context, op *wsv1.ProbeHttp) (*wsv1.OpResult_ProbeHttp, *wsv1.OpError) {
	u, err := url.Parse(op.GetUrl())
	if err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "invalid url %q: %v", op.GetUrl(), err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_FORBIDDEN,
			"url scheme %q not allowed (http/https only)", u.Scheme)
	}
	timeout, oerr := resolveProbeTimeout(op.GetTimeoutSeconds())
	if oerr != nil {
		return nil, oerr
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, op.GetUrl(), nil)
	if err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "build request: %v", err)
	}
	client := &http.Client{
		Timeout: timeout,
		// Redirects are never followed: the target's Location header is
		// never fetched, only the first response's status code is
		// reported. Following redirects here would let a probed target
		// redirect Keystone's allowlist check to an arbitrary host.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start)
	if err != nil {
		return &wsv1.OpResult_ProbeHttp{ProbeHttp: &wsv1.ProbeHttpResult{
			Reachable: false,
			Detail:    err.Error(),
			LatencyMs: uint32(latency.Milliseconds()),
		}}, nil
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxProbeHTTPBody)) // bounded drain, ADR-W003 §A4.4

	return &wsv1.OpResult_ProbeHttp{ProbeHttp: &wsv1.ProbeHttpResult{
		Reachable:  true,
		StatusCode: uint32(resp.StatusCode),
		LatencyMs:  uint32(latency.Milliseconds()),
	}}, nil
}
