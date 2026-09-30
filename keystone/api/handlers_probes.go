package api

// Onboarding validation probes (ADR-W016 A2 / scope §7.2, E10-U2): the
// live checks the Nodes view runs while pairing a machine — block
// devices for the storage step, listening ports for conflicts, and
// reachability probes for DNS/tunnel/NAS targets. Every underlying op
// is dial-or-read only (TestNfs/TestSmb dial TCP, no mount is ever
// performed — see the proto), so the whole family carries the read verb.
// The home-automation probes (radio inventory, #434) join the family on
// the same terms: parameterless reads of fixed host locations.

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

func (s *Server) probeDispatch(r *http.Request, op *wsv1.Operation) (*wsv1.OpResult, string, int, error) {
	if s.Agents == nil {
		return nil, "", http.StatusServiceUnavailable, fmt.Errorf("agent plane not wired")
	}
	node := r.PathValue("node")
	if node == "" {
		return nil, "", http.StatusBadRequest, fmt.Errorf("node required")
	}
	res, err := s.Agents.Apply(r.Context(), node, op)
	if err != nil {
		return nil, "", http.StatusBadGateway, fmt.Errorf("node %s: %w", node, err)
	}
	if oe := res.GetError(); oe != nil {
		return nil, "", opStatus(oe), fmt.Errorf("node %s: %s", node, oe.GetMessage())
	}
	return res, node, 0, nil
}

func (s *Server) probeBlockDevices(r *http.Request, _ principal.Identity) (any, int, error) {
	res, node, status, err := s.probeDispatch(r, &wsv1.Operation{Op: &wsv1.Operation_ListBlockDevices{
		ListBlockDevices: &wsv1.ListBlockDevices{},
	}})
	if err != nil {
		return nil, status, err
	}
	type dev struct {
		Name      string `json:"name"`
		SizeBytes uint64 `json:"sizeBytes"`
		Removable bool   `json:"removable"`
	}
	out := []dev{}
	for _, d := range res.GetListBlockDevices().GetDevices() {
		out = append(out, dev{Name: d.GetName(), SizeBytes: d.GetSizeBytes(), Removable: d.GetRemovable()})
	}
	return map[string]any{"node": node, "devices": out}, http.StatusOK, nil
}

func (s *Server) probePorts(r *http.Request, _ principal.Identity) (any, int, error) {
	res, node, status, err := s.probeDispatch(r, &wsv1.Operation{Op: &wsv1.Operation_ListPorts{
		ListPorts: &wsv1.ListPorts{},
	}})
	if err != nil {
		return nil, status, err
	}
	type port struct {
		Proto   string `json:"proto"`
		Port    uint32 `json:"port"`
		LocalIP string `json:"localIp"`
	}
	out := []port{}
	for _, p := range res.GetListPorts().GetPorts() {
		out = append(out, port{Proto: p.GetProto(), Port: p.GetPort(), LocalIP: p.GetLocalIp()})
	}
	return map[string]any{"node": node, "ports": out}, http.StatusOK, nil
}

// radioKindNames is the closed vocabulary the radio probe answers with.
// An unspecified or future kind reads as "unknown": the Console must
// never present a guess the agent did not make.
var radioKindNames = map[wsv1.RadioKind]string{
	wsv1.RadioKind_RADIO_KIND_UNKNOWN:   "unknown",
	wsv1.RadioKind_RADIO_KIND_BLUETOOTH: "bluetooth",
	wsv1.RadioKind_RADIO_KIND_ZIGBEE:    "zigbee",
	wsv1.RadioKind_RADIO_KIND_ZWAVE:     "zwave",
	wsv1.RadioKind_RADIO_KIND_THREAD:    "thread",
}

// probeRadios inventories a node's serial and Bluetooth adapters
// (ListRadios, protocol minor 24, #434). The op is parameterless, so the
// endpoint takes no query: nothing here can steer what the agent reads.
func (s *Server) probeRadios(r *http.Request, _ principal.Identity) (any, int, error) {
	res, node, status, err := s.probeDispatch(r, &wsv1.Operation{Op: &wsv1.Operation_ListRadios{
		ListRadios: &wsv1.ListRadios{},
	}})
	if err != nil {
		return nil, status, err
	}
	type usb struct {
		VendorID     string `json:"vendorId"`
		ProductID    string `json:"productId"`
		Manufacturer string `json:"manufacturer,omitempty"`
		Product      string `json:"product,omitempty"`
		Serial       string `json:"serial,omitempty"`
		Port         string `json:"port,omitempty"`
	}
	type radio struct {
		ID         string   `json:"id"`
		Kind       string   `json:"kind"`
		KindReason string   `json:"kindReason"`
		KernelName string   `json:"kernelName"`
		DevPath    string   `json:"devPath,omitempty"`
		ByIDPaths  []string `json:"byIdPaths"`
		SysfsPath  string   `json:"sysfsPath,omitempty"`
		Bus        string   `json:"bus,omitempty"`
		Driver     string   `json:"driver,omitempty"`
		USB        *usb     `json:"usb,omitempty"`
	}
	lr := res.GetListRadios()
	out := []radio{}
	for _, d := range lr.GetRadios() {
		kind, ok := radioKindNames[d.GetKind()]
		if !ok {
			kind = "unknown"
		}
		rd := radio{
			ID: d.GetId(), Kind: kind, KindReason: d.GetKindReason(),
			KernelName: d.GetKernelName(), DevPath: d.GetDevPath(),
			ByIDPaths: append([]string{}, d.GetByIdPaths()...),
			SysfsPath: d.GetSysfsPath(), Bus: d.GetBus(), Driver: d.GetDriver(),
		}
		if d.GetUsbVendorId() != "" {
			rd.USB = &usb{
				VendorID: d.GetUsbVendorId(), ProductID: d.GetUsbProductId(),
				Manufacturer: d.GetUsbManufacturer(), Product: d.GetUsbProduct(),
				Serial: d.GetUsbSerial(), Port: d.GetUsbPort(),
			}
		}
		out = append(out, rd)
	}
	return map[string]any{
		"node": node, "platformSupported": lr.GetPlatformSupported(), "radios": out,
	}, http.StatusOK, nil
}

func (s *Server) probeHttp(r *http.Request, _ principal.Identity) (any, int, error) {
	url := r.URL.Query().Get("url")
	if url == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("url query parameter required")
	}
	res, node, status, err := s.probeDispatch(r, &wsv1.Operation{Op: &wsv1.Operation_ProbeHttp{
		ProbeHttp: &wsv1.ProbeHttp{Url: url},
	}})
	if err != nil {
		return nil, status, err
	}
	p := res.GetProbeHttp()
	return map[string]any{
		"node": node, "url": url,
		"reachable": p.GetReachable(), "detail": p.GetDetail(),
	}, http.StatusOK, nil
}

// probeDial serves the nfs and smb reachability checks, which share a
// host[/port] shape and a {reachable, detail} result.
func (s *Server) probeDial(kind string) handlerFunc {
	return func(r *http.Request, _ principal.Identity) (any, int, error) {
		host := r.URL.Query().Get("host")
		if host == "" {
			return nil, http.StatusBadRequest, fmt.Errorf("host query parameter required")
		}
		var port uint64
		if p := r.URL.Query().Get("port"); p != "" {
			var err error
			if port, err = strconv.ParseUint(p, 10, 16); err != nil {
				return nil, http.StatusBadRequest, fmt.Errorf("port must be a number")
			}
		}
		var op *wsv1.Operation
		if kind == "nfs" {
			op = &wsv1.Operation{Op: &wsv1.Operation_TestNfs{TestNfs: &wsv1.TestNfs{Host: host, Port: uint32(port)}}}
		} else {
			op = &wsv1.Operation{Op: &wsv1.Operation_TestSmb{TestSmb: &wsv1.TestSmb{Host: host, Port: uint32(port)}}}
		}
		res, node, status, err := s.probeDispatch(r, op)
		if err != nil {
			return nil, status, err
		}
		var reachable bool
		var detail string
		if kind == "nfs" {
			reachable, detail = res.GetTestNfs().GetReachable(), res.GetTestNfs().GetDetail()
		} else {
			reachable, detail = res.GetTestSmb().GetReachable(), res.GetTestSmb().GetDetail()
		}
		return map[string]any{
			"node": node, "host": host,
			"reachable": reachable, "detail": detail,
		}, http.StatusOK, nil
	}
}
