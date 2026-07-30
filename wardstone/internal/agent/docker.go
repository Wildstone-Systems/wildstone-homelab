package agent

import (
	"bytes"
	"context"
	"encoding/json"
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
type dockerClient struct {
	http *http.Client
	base string
}

func newDockerClient(endpoint string) (*dockerClient, error) {
	if sock, ok := strings.CutPrefix(endpoint, "unix://"); ok {
		return &dockerClient{
			base: "http://docker",
			http: &http.Client{
				Timeout: 2 * time.Minute,
				Transport: &http.Transport{
					DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
						var d net.Dialer
						return d.DialContext(ctx, "unix", sock)
					},
				},
			},
		}, nil
	}
	if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
		return &dockerClient{base: strings.TrimRight(endpoint, "/"), http: &http.Client{Timeout: 2 * time.Minute}}, nil
	}
	return nil, fmt.Errorf("docker endpoint %q: want unix:// or http(s)://", endpoint)
}

type containerInspect struct {
	ID    string `json:"Id"`
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

type dockerError struct {
	status  int
	message string
}

func (e *dockerError) Error() string  { return fmt.Sprintf("docker: %d: %s", e.status, e.message) }
func (e *dockerError) notFound() bool { return e.status == http.StatusNotFound }

func (d *dockerClient) call(ctx context.Context, method, path string, query url.Values, body any) ([]byte, error) {
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
	Image      string            `json:"Image"`
	Env        []string          `json:"Env,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
	HostConfig hostConfig        `json:"HostConfig"`
}

type hostConfig struct {
	Binds         []string                 `json:"Binds,omitempty"`
	PortBindings  map[string][]portBinding `json:"PortBindings,omitempty"`
	RestartPolicy restartPolicy            `json:"RestartPolicy"`
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

func (d *dockerClient) pull(ctx context.Context, ref string) error {
	_, err := d.call(ctx, "POST", "/images/create", url.Values{"fromImage": {ref}}, nil)
	return err
}
