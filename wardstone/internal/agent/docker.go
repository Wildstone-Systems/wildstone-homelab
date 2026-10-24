package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// dockerClient is a deliberately tiny Docker Engine API client (stdlib
// only — Wardstone's "~zero dependencies" rule). Endpoint is either
// "unix:///var/run/docker.sock" or an http://host:port URL (tests).
//
// It holds two clients over one transport. http bounds every call at
// callTimeout, which suits inspect, stats, logs and the like. pulls has no
// client timeout: an image pull streams progress for as long as the
// download takes, and a multi-GB image on a home connection outlasts any
// bound sized for quick calls (#361). A pull is bounded by pullTimeout
// instead, through its context.
type dockerClient struct {
	http        *http.Client
	pulls       *http.Client
	base        string
	pullTimeout time.Duration
}

// The Docker Engine client is the node's runtime backend (runtime.go).
var _ runtimeBackend = (*dockerClient)(nil)

const (
	// dockerCallTimeout bounds every Docker Engine API call except a pull.
	dockerCallTimeout = 2 * time.Minute
	// dockerPullTimeout bounds one image pull. A pull that runs out of it
	// has kept the layers it finished, so the retry resumes rather than
	// starting over.
	dockerPullTimeout = 30 * time.Minute
)

func newDockerClient(endpoint string) (*dockerClient, error) {
	var transport http.RoundTripper
	base := strings.TrimRight(endpoint, "/")
	if sock, ok := strings.CutPrefix(endpoint, "unix://"); ok {
		base = "http://docker"
		transport = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		}
	} else if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		return nil, fmt.Errorf("docker endpoint %q: want unix:// or http(s)://", endpoint)
	}
	return &dockerClient{
		base:        base,
		http:        &http.Client{Timeout: dockerCallTimeout, Transport: transport},
		pulls:       &http.Client{Transport: transport},
		pullTimeout: dockerPullTimeout,
	}, nil
}

type containerInspect struct {
	ID    string `json:"Id"`
	State struct {
		Running bool `json:"Running"`
		// Status and ExitCode describe a container that has STOPPED,
		// which Running alone cannot distinguish from one that never
		// started. A one-shot job that finished cleanly and one that
		// died on its first line are both Running=false.
		Status   string `json:"Status"`
		ExitCode int32  `json:"ExitCode"`
		// Health is the container's OWN healthcheck, absent from the
		// document entirely when the image declares none — which is why
		// it is a pointer: a missing block and a healthcheck reporting
		// nothing are different facts, and only one of them is worth
		// reporting.
		Health *struct {
			Status string `json:"Status"`
		} `json:"Health,omitempty"`
	} `json:"State"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	HostConfig struct {
		PortBindings map[string][]portBinding `json:"PortBindings"`
		// NetworkMode is Docker's own record of the namespace the
		// container runs in: "host", or a bridge/network name.
		NetworkMode string `json:"NetworkMode"`
		// Devices is what Docker will hand the container on its next
		// start: the RESOLVED host node, fixed when it was created. See
		// staleDevice for why that can stop matching the stick.
		Devices []deviceMapping `json:"Devices"`
	} `json:"HostConfig"`
	// Networks is keyed by Docker network name; a container attached to
	// several has an address on each. Parsed because Keystone has to
	// know which address the identity proxy will appear as — see
	// InspectContainerResult.ip_addresses for why that cannot be a
	// subnet, a DNS name, or a value pinned at install time.
	NetworkSettings struct {
		Ports    map[string][]portBinding `json:"Ports"`
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

type dockerError struct {
	status  int
	message string
}

func (e *dockerError) Error() string  { return fmt.Sprintf("docker: %d: %s", e.status, e.message) }
func (e *dockerError) notFound() bool { return e.status == http.StatusNotFound }

func (d *dockerClient) call(ctx context.Context, method, path string, query url.Values, body any) ([]byte, error) {
	return d.callWithHeaders(ctx, method, path, query, body, nil)
}

// callWithHeaders is call plus request headers, for the one caller that
// needs them (the registry credential on a pull). The header value is
// never logged: the error path below reports the daemon's response, not
// the request.
func (d *dockerClient) callWithHeaders(ctx context.Context, method, path string, query url.Values, body any, headers map[string]string) ([]byte, error) {
	u := d.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(raw))
		var payload struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &payload) == nil && payload.Message != "" {
			msg = payload.Message
		}
		return nil, &dockerError{status: resp.StatusCode, message: msg}
	}
	return raw, nil
}

func (d *dockerClient) inspect(ctx context.Context, name string) (*containerInspect, error) {
	raw, err := d.call(ctx, "GET", "/containers/"+url.PathEscape(name)+"/json", nil, nil)
	if err != nil {
		return nil, err
	}
	var ci containerInspect
	if err := json.Unmarshal(raw, &ci); err != nil {
		return nil, err
	}
	return &ci, nil
}

type createBody struct {
	Image        string              `json:"Image"`
	Cmd          []string            `json:"Cmd,omitempty"`
	Env          []string            `json:"Env,omitempty"`
	Labels       map[string]string   `json:"Labels,omitempty"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	HostConfig   hostConfig          `json:"HostConfig"`
	// NetworkingConfig attaches the container at create time. Docker
	// registers the container's name as a DNS alias on a user-defined
	// network automatically, which is what makes "<name>:<port>"
	// resolvable between containers — the default bridge has no
	// embedded DNS at all.
	NetworkingConfig *networkingConfig `json:"NetworkingConfig,omitempty"`
}

type networkingConfig struct {
	EndpointsConfig map[string]endpointConfig `json:"EndpointsConfig"`
}

// endpointConfig is intentionally empty: Docker fills in the address and
// adds the container name as an alias. Wildstone pins no IPs.
type endpointConfig struct{}

type hostConfig struct {
	Binds         []string                 `json:"Binds,omitempty"`
	PortBindings  map[string][]portBinding `json:"PortBindings,omitempty"`
	RestartPolicy restartPolicy            `json:"RestartPolicy"`
	// Devices and CapAdd (protocol minor 12) reach here only through
	// the allowlists in devices.go; nothing else constructs a hostConfig.
	Devices []deviceMapping `json:"Devices,omitempty"`
	CapAdd  []string        `json:"CapAdd,omitempty"`
	// ExtraHosts are "name:ip" entries Docker writes into the
	// container's /etc/hosts (protocol minor 14). Resolved by the agent
	// from host_aliases, because the node is where the name resolves —
	// except hostGatewayAlias, written as Docker's "host-gateway"
	// (protocol minor 24).
	ExtraHosts []string `json:"ExtraHosts,omitempty"`

	// The H1 hardening fields (protocol minor 18). Reached only through
	// ensureContainer's checks (checkCapDrop, dockerSizeToBytes,
	// cpusToNanoCPUs) — nothing else constructs a hostConfig, the same
	// invariant the comment above Devices/CapAdd already states.
	ReadonlyRootfs bool     `json:"ReadonlyRootfs,omitempty"`
	CapDrop        []string `json:"CapDrop,omitempty"`
	// SecurityOpt carries "no-new-privileges:true" when the spec asks for
	// it; empty otherwise. A slice rather than a bool field because that
	// is the shape Docker's own API takes ("Docker security options"),
	// even though the agent only ever populates this one entry.
	SecurityOpt []string `json:"SecurityOpt,omitempty"`
	// Memory (bytes), NanoCpus (billionths of a core) and PidsLimit are
	// Docker's own HostConfig.Resources fields, converted from the spec's
	// human units ("4g", "2") by dockerSizeToBytes/cpusToNanoCPUs — the
	// wire and the YAML both stay in the units an operator reads, and the
	// arithmetic happens once, at the edge that actually calls Docker.
	Memory   int64 `json:"Memory,omitempty"`
	NanoCpus int64 `json:"NanoCpus,omitempty"`
	// PidsLimit's zero value is the same as leaving it unset (no limit),
	// which is also what the spec's own PidsLimit==0 means — there is no
	// "explicitly unlimited" distinct from "not set" on either side.
	PidsLimit int64 `json:"PidsLimit,omitempty"`
	// ShmSize (bytes); zero leaves Docker's own default (64 MiB).
	ShmSize int64 `json:"ShmSize,omitempty"`
	// NetworkMode is "host" for a CONTAINER_NETWORK_MODE_HOST op and
	// empty otherwise, which Docker reads as its default bridge (protocol
	// minor 23). Nothing else is ever written here: "container:<id>"
	// would join another container's namespace.
	NetworkMode string `json:"NetworkMode,omitempty"`
}

// deviceMapping is Docker's HostConfig.Devices entry.
type deviceMapping struct {
	PathOnHost        string `json:"PathOnHost"`
	PathInContainer   string `json:"PathInContainer"`
	CgroupPermissions string `json:"CgroupPermissions"`
}

type portBinding struct {
	HostIP   string `json:"HostIp,omitempty"`
	HostPort string `json:"HostPort"`
}

type restartPolicy struct {
	Name string `json:"Name"`
}

func (d *dockerClient) create(ctx context.Context, name string, body createBody) (string, error) {
	raw, err := d.call(ctx, "POST", "/containers/create", url.Values{"name": {name}}, body)
	if err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

func (d *dockerClient) start(ctx context.Context, id string) error {
	_, err := d.call(ctx, "POST", "/containers/"+url.PathEscape(id)+"/start", nil, nil)
	if derr, ok := err.(*dockerError); ok && derr.status == http.StatusNotModified {
		return nil // already running
	}
	return err
}

func (d *dockerClient) stop(ctx context.Context, name string, timeoutSec uint32) error {
	q := url.Values{}
	if timeoutSec > 0 {
		q.Set("t", fmt.Sprint(timeoutSec))
	}
	_, err := d.call(ctx, "POST", "/containers/"+url.PathEscape(name)+"/stop", q, nil)
	if derr, ok := err.(*dockerError); ok && derr.status == http.StatusNotModified {
		return nil // already stopped
	}
	return err
}

func (d *dockerClient) logs(ctx context.Context, name string, tail uint32, sinceUnix int64) ([]byte, error) {
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}}
	if tail == 0 {
		tail = 100
	}
	q.Set("tail", fmt.Sprint(tail))
	if sinceUnix > 0 {
		q.Set("since", fmt.Sprint(sinceUnix))
	}
	return d.call(ctx, "GET", "/containers/"+url.PathEscape(name)+"/logs", q, nil)
}

// pull fetches an image. auth, when non-empty, is a base64url AuthConfig
// for the registry the ref names — see registryauth.go. It is a header
// rather than a query parameter deliberately: query strings end up in
// proxy and daemon logs.
//
// The daemon answers 200 as soon as the pull starts and then streams
// progress as JSON messages until it ends. A failure partway through (a
// layer that will not download, a full disk) arrives as an "error"
// message inside that 200 stream, so the stream is read to its end and
// checked, rather than taking the status as the outcome. Reading to the
// end also matters on its own: closing the response early cancels the
// daemon's pull.
func (d *dockerClient) pull(ctx context.Context, ref, auth string) error {
	ctx, cancel := context.WithTimeout(ctx, d.pullTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST",
		d.base+"/images/create?"+url.Values{"fromImage": {ref}}.Encode(), nil)
	if err != nil {
		return err
	}
	if auth != "" {
		req.Header.Set("X-Registry-Auth", auth)
	}
	resp, err := d.pulls.Do(req)
	if err != nil {
		return d.pullFailed(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		msg := strings.TrimSpace(string(raw))
		var payload struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &payload) == nil && payload.Message != "" {
			msg = payload.Message
		}
		return &dockerError{status: resp.StatusCode, message: msg}
	}
	dec := json.NewDecoder(resp.Body)
	for {
		var m struct {
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := dec.Decode(&m); err == io.EOF {
			return nil
		} else if err != nil {
			return d.pullFailed(ctx, err)
		}
		if m.ErrorDetail.Message != "" {
			return fmt.Errorf("docker: pull: %s", m.ErrorDetail.Message)
		}
		if m.Error != "" {
			return fmt.Errorf("docker: pull: %s", m.Error)
		}
	}
}

// pullFailed says why a pull stopped when it was its own deadline, which
// the transport reports only as a bare "context deadline exceeded".
func (d *dockerClient) pullFailed(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("pull did not finish within %s (layers already downloaded are kept for the retry): %w",
			d.pullTimeout, err)
	}
	return err
}

func (d *dockerClient) remove(ctx context.Context, id string, force bool) error {
	q := url.Values{}
	if force {
		q.Set("force", "1")
	}
	_, err := d.call(ctx, "DELETE", "/containers/"+url.PathEscape(id), q, nil)
	return err
}

type statsResult struct {
	CPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
	} `json:"cpu_stats"`
	MemoryStats struct {
		Usage uint64 `json:"usage"`
		Limit uint64 `json:"limit"`
	} `json:"memory_stats"`
}

func (d *dockerClient) stats(ctx context.Context, id string) (statsResult, error) {
	raw, err := d.call(ctx, "GET", "/containers/"+url.PathEscape(id)+"/stats", url.Values{"stream": {"false"}}, nil)
	if err != nil {
		return statsResult{}, err
	}
	var out statsResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return statsResult{}, err
	}
	return out, nil
}

type volumeInspectResult struct {
	Name string `json:"Name"`
	// Mountpoint is the host path backing the volume. Needed by the seed
	// ops, which write into an application's own data before it first
	// starts; every other op reaches a volume by mounting it into a
	// container instead.
	Mountpoint string            `json:"Mountpoint"`
	Labels     map[string]string `json:"Labels"`
}

// volumeInspect returns nil, notFound-error when the volume does not
// exist (matches containerInspect's error-passthrough convention — the
// caller distinguishes via dockerError.notFound()).
func (d *dockerClient) volumeInspect(ctx context.Context, name string) (*volumeInspectResult, error) {
	raw, err := d.call(ctx, "GET", "/volumes/"+url.PathEscape(name), nil, nil)
	if err != nil {
		return nil, err
	}
	var out volumeInspectResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (d *dockerClient) volumeCreate(ctx context.Context, name string, labels map[string]string) error {
	body := map[string]any{"Name": name, "Labels": labels}
	_, err := d.call(ctx, "POST", "/volumes/create", nil, body)
	return err
}

type networkInspectResult struct {
	Name   string            `json:"Name"`
	Labels map[string]string `json:"Labels"`
}

// networkInspect returns nil, notFound-error when the network does not
// exist (same convention as containerInspect/volumeInspect).
func (d *dockerClient) networkInspect(ctx context.Context, name string) (*networkInspectResult, error) {
	raw, err := d.call(ctx, "GET", "/networks/"+url.PathEscape(name), nil, nil)
	if err != nil {
		return nil, err
	}
	var out networkInspectResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (d *dockerClient) networkCreate(ctx context.Context, name string, labels map[string]string) error {
	body := map[string]any{"Name": name, "Labels": labels}
	_, err := d.call(ctx, "POST", "/networks/create", nil, body)
	return err
}

type pruneResult struct {
	ImagesDeleted  []struct{} `json:"ImagesDeleted"`
	SpaceReclaimed uint64     `json:"SpaceReclaimed"`
}

// pruneImages prunes ONLY wildstone-owned images. The label filter is a
// hard invariant (ADR-W003 §A4.3) — never call /images/prune without it.
func (d *dockerClient) pruneImages(ctx context.Context) (pruneResult, error) {
	filters, err := json.Marshal(map[string][]string{
		"label": {OwnerLabel + "=" + ownerValue},
	})
	if err != nil {
		return pruneResult{}, err
	}
	raw, err := d.call(ctx, "POST", "/images/prune", url.Values{"filters": {string(filters)}}, nil)
	if err != nil {
		return pruneResult{}, err
	}
	var out pruneResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return pruneResult{}, err
	}
	return out, nil
}
