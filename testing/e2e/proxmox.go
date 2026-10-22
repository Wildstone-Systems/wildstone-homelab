package e2e

// STATUS: UNTESTED. Hosted CI and cloud agents must never reach Proxmox
// (ADR-W002), so this provider cannot be exercised from where it was
// written. Its first real run must happen from inside the tailnet — the
// owner's machine or a self-hosted runner. Expect to debug it there.
//
// Assumptions about the Proxmox side:
//   - An API token with VM.Clone/VM.Config/VM.PowerMgmt/VM.Audit on the node.
//   - A cloud-init-enabled Ubuntu 24.04 template VM (qemu-guest-agent
//     installed) identified by TemplateVMID.
//   - DHCP on the template's bridge; the VM's address is read back via the
//     guest agent.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ProxmoxProvider clones a cloud-init template on a Proxmox node.
type ProxmoxProvider struct {
	BaseURL      string // e.g. https://proxmox.tailnet:8006
	TokenID      string // user@realm!tokenname
	TokenSecret  string
	Node         string
	TemplateVMID int
	Keys         *Keypair
	InsecureTLS  bool          // homelab Proxmox typically has a self-signed cert
	BootWait     time.Duration // default 5m

	httpClient *http.Client
}

type proxmoxVM struct {
	vmid int
}

func (p *ProxmoxProvider) Create(ctx context.Context, name string) (*VM, error) {
	if p.Keys == nil {
		return nil, fmt.Errorf("ProxmoxProvider needs Keys")
	}
	vmid, err := p.nextID(ctx)
	if err != nil {
		return nil, err
	}
	_, err = p.api(ctx, "POST", fmt.Sprintf("/nodes/%s/qemu/%d/clone", p.Node, p.TemplateVMID), url.Values{
		"newid": {fmt.Sprint(vmid)},
		"name":  {name},
		"full":  {"0"},
	})
	if err != nil {
		return nil, fmt.Errorf("clone: %w", err)
	}
	_, err = p.api(ctx, "POST", fmt.Sprintf("/nodes/%s/qemu/%d/config", p.Node, vmid), url.Values{
		"ciuser":    {"wildstone"},
		"sshkeys":   {url.QueryEscape(p.Keys.Authorized)},
		"ipconfig0": {"ip=dhcp"},
	})
	if err != nil {
		return nil, fmt.Errorf("cloud-init config: %w", err)
	}
	if _, err := p.api(ctx, "POST", fmt.Sprintf("/nodes/%s/qemu/%d/status/start", p.Node, vmid), nil); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, orDefaultDur(p.BootWait, 5*time.Minute))
	defer cancel()
	ip, err := p.waitForIP(waitCtx, vmid)
	if err != nil {
		return nil, err
	}
	vm := &VM{
		Name:         name,
		Addr:         ip + ":22",
		User:         "wildstone",
		providerData: &proxmoxVM{vmid: vmid},
	}
	if err := WaitSSH(waitCtx, vm, p.Keys.Signer); err != nil {
		return nil, err
	}
	return vm, nil
}

func (p *ProxmoxProvider) Destroy(ctx context.Context, vm *VM) error {
	pv, _ := vm.providerData.(*proxmoxVM)
	if pv == nil {
		return fmt.Errorf("%s was not created by ProxmoxProvider", vm.Name)
	}
	_, _ = p.api(ctx, "POST", fmt.Sprintf("/nodes/%s/qemu/%d/status/stop", p.Node, pv.vmid), nil)
	_, err := p.api(ctx, "DELETE", fmt.Sprintf("/nodes/%s/qemu/%d", p.Node, pv.vmid), nil)
	return err
}

func (p *ProxmoxProvider) nextID(ctx context.Context) (int, error) {
	data, err := p.api(ctx, "GET", "/cluster/nextid", nil)
	if err != nil {
		return 0, err
	}
	var id string
	if err := json.Unmarshal(data, &id); err != nil {
		return 0, err
	}
	var vmid int
	_, err = fmt.Sscanf(id, "%d", &vmid)
	return vmid, err
}

// waitForIP polls the guest agent for the first non-loopback IPv4 address.
func (p *ProxmoxProvider) waitForIP(ctx context.Context, vmid int) (string, error) {
	for {
		data, err := p.api(ctx, "GET",
			fmt.Sprintf("/nodes/%s/qemu/%d/agent/network-get-interfaces", p.Node, vmid), nil)
		if err == nil {
			if ip := firstIPv4(data); ip != "" {
				return ip, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("vm %d never reported an IP via guest agent: %w", vmid, ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}
}

func firstIPv4(agentData []byte) string {
	var payload struct {
		Result []struct {
			Name        string `json:"name"`
			IPAddresses []struct {
				Type    string `json:"ip-address-type"`
				Address string `json:"ip-address"`
			} `json:"ip-addresses"`
		} `json:"result"`
	}
	if err := json.Unmarshal(agentData, &payload); err != nil {
		return ""
	}
	for _, iface := range payload.Result {
		if iface.Name == "lo" {
			continue
		}
		for _, addr := range iface.IPAddresses {
			if addr.Type == "ipv4" {
				return addr.Address
			}
		}
	}
	return ""
}

// api performs one Proxmox API call and returns the raw "data" member.
func (p *ProxmoxProvider) api(ctx context.Context, method, path string, form url.Values) (json.RawMessage, error) {
	if p.httpClient == nil {
		transport := http.DefaultTransport
		if p.InsecureTLS {
			t := http.DefaultTransport.(*http.Transport).Clone()
			t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
			transport = t
		}
		p.httpClient = &http.Client{Transport: transport, Timeout: 30 * time.Second}
	}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, p.BaseURL+"/api2/json"+path, body)
	if err != nil {
		return nil, err
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Authorization", fmt.Sprintf("PVEAPIToken=%s=%s", p.TokenID, p.TokenSecret))
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("proxmox %s %s: %s: %s", method, path, resp.Status, raw)
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("proxmox %s %s: bad envelope: %w", method, path, err)
	}
	return envelope.Data, nil
}
