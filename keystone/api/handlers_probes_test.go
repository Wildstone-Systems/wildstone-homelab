package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// TestProbeEndpoints drives the E10-U2 probe family against a fake
// agent: shapes come back mapped, missing params 400, and the family is
// readable by every role (the ops are dial-or-read only).
func TestProbeEndpoints(t *testing.T) {
	h := newHarness(t)
	h.s.Agents = applierFunc(func(_ context.Context, node string, op *wsv1.Operation) (*wsv1.OpResult, error) {
		if node != "n1" {
			t.Fatalf("dispatched to node %q", node)
		}
		switch op.GetOp().(type) {
		case *wsv1.Operation_ListBlockDevices:
			return &wsv1.OpResult{Result: &wsv1.OpResult_ListBlockDevices{ListBlockDevices: &wsv1.ListBlockDevicesResult{
				Devices: []*wsv1.BlockDevice{{Name: "sda", SizeBytes: 1 << 40}},
			}}}, nil
		case *wsv1.Operation_ListPorts:
			return &wsv1.OpResult{Result: &wsv1.OpResult_ListPorts{ListPorts: &wsv1.ListPortsResult{
				Ports: []*wsv1.ListeningPort{{Proto: "tcp", Port: 443, LocalIp: "0.0.0.0"}},
			}}}, nil
		case *wsv1.Operation_ProbeHttp:
			return &wsv1.OpResult{Result: &wsv1.OpResult_ProbeHttp{ProbeHttp: &wsv1.ProbeHttpResult{
				Reachable: true,
			}}}, nil
		case *wsv1.Operation_TestNfs:
			return &wsv1.OpResult{Result: &wsv1.OpResult_TestNfs{TestNfs: &wsv1.TestNfsResult{
				Reachable: false, Detail: "connection refused",
			}}}, nil
		default:
			t.Fatalf("unexpected op %v", op)
			return nil, nil
		}
	})

	var devs struct {
		Devices []struct {
			Name string `json:"name"`
		} `json:"devices"`
	}
	if resp := h.do(runtimedb.RoleButler, http.MethodGet, "/api/v1/nodes/n1/probes/blockdevices", nil, &devs); resp.StatusCode != http.StatusOK {
		t.Fatalf("blockdevices = %d", resp.StatusCode)
	}
	if len(devs.Devices) != 1 || devs.Devices[0].Name != "sda" {
		t.Fatalf("devices = %+v", devs)
	}

	var ports struct {
		Ports []struct {
			Port uint32 `json:"port"`
		} `json:"ports"`
	}
	if resp := h.do(runtimedb.RoleOperator, http.MethodGet, "/api/v1/nodes/n1/probes/ports", nil, &ports); resp.StatusCode != http.StatusOK {
		t.Fatalf("ports = %d", resp.StatusCode)
	}
	if len(ports.Ports) != 1 || ports.Ports[0].Port != 443 {
		t.Fatalf("ports = %+v", ports)
	}

	var hp struct {
		Reachable bool `json:"reachable"`
	}
	if resp := h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/nodes/n1/probes/http?url=http%3A%2F%2F10.0.0.1%3A80", nil, &hp); resp.StatusCode != http.StatusOK {
		t.Fatalf("http probe = %d", resp.StatusCode)
	}
	if !hp.Reachable {
		t.Fatalf("http probe = %+v", hp)
	}
	if resp := h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/nodes/n1/probes/http", nil, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing url = %d, want 400", resp.StatusCode)
	}

	var nfs struct {
		Reachable bool   `json:"reachable"`
		Detail    string `json:"detail"`
	}
	if resp := h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/nodes/n1/probes/nfs?host=nas.internal", nil, &nfs); resp.StatusCode != http.StatusOK {
		t.Fatalf("nfs probe = %d", resp.StatusCode)
	}
	if nfs.Reachable || nfs.Detail != "connection refused" {
		t.Fatalf("nfs = %+v", nfs)
	}
	if resp := h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/nodes/n1/probes/nfs", nil, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing host = %d, want 400", resp.StatusCode)
	}
}
