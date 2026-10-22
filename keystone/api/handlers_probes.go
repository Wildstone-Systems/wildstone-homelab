package api

// Onboarding validation probes (ADR-W016 A2 / scope §7.2, E10-U2): the
// live checks the Nodes view runs while pairing a machine — block
// devices for the storage step, listening ports for conflicts, and
// reachability probes for DNS/tunnel/NAS targets. Every underlying op
// is dial-or-read only (TestNfs/TestSmb dial TCP, no mount is ever
// performed — see the proto), so the whole family carries the read verb.
// The home-automation probes (radio inventory #434, readiness #436) join the family on
// the same terms: parameterless reads of fixed host locations.

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/plan"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
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
// (ListRadios, protocol minor 25, #434). The op is parameterless, so the
// endpoint takes no query: nothing here can steer what the agent reads.
func (s *Server) probeRadios(r *http.Request, _ principal.Identity) (any, int, error) {
	res, node, status, err := s.probeDispatch(r, &wsv1.Operation{Op: &wsv1.Operation_ListRadios{
		ListRadios: &wsv1.ListRadios{},
	}})
	if err != nil {
		return nil, status, err
	}
	lr := res.GetListRadios()
	out := viewRadios(lr)
	return map[string]any{
		"node": node, "platformSupported": lr.GetPlatformSupported(), "radios": out,
	}, http.StatusOK, nil
}

// radioInventoryTimeout bounds the inventory read a proposal waits on. The
// op reads a handful of sysfs files; a node that has not answered by
// then is treated as offline, not waited for.
const radioInventoryTimeout = 5 * time.Second

// radioInventory is the proposal pipeline's view of a node's radios
// (#435), through the same Agents.Apply seam the probe endpoint uses.
// nil means "could not ask": no agent plane, a node not connected, an
// agent older than plan.RadioInventoryProtocolMinor, or any error — and
// the duplicate-ownership check then compares exact paths only. A
// platformSupported=false answer is an empty, non-nil inventory: the
// node was asked and has nothing to fold.
func (s *Server) radioInventory(node string) []plan.Radio {
	if s.Agents == nil || node == "" {
		return nil
	}
	if s.LiveNodes != nil {
		live := false
		for _, n := range s.LiveNodes() {
			if n.NodeID == node {
				live = n.ProtocolMinor >= plan.RadioInventoryProtocolMinor
				break
			}
		}
		if !live {
			return nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), radioInventoryTimeout)
	defer cancel()
	res, err := s.Agents.Apply(ctx, node, &wsv1.Operation{Op: &wsv1.Operation_ListRadios{ListRadios: &wsv1.ListRadios{}}})
	if err != nil || res.GetError() != nil || res.GetListRadios() == nil {
		return nil
	}
	out := []plan.Radio{}
	for _, r := range res.GetListRadios().GetRadios() {
		paths := append([]string{}, r.GetByIdPaths()...)
		if r.GetDevPath() != "" {
			paths = append(paths, r.GetDevPath())
		}
		out = append(out, plan.Radio{ID: r.GetId(), Paths: paths})
	}
	return out
}

// readinessStateNames is the closed state vocabulary of the
// home-automation readiness probe. An unspecified or future state reads
// as "unknown", so the Console never shows a state the agent did not
// assert.
var readinessStateNames = map[wsv1.ReadinessState]string{
	wsv1.ReadinessState_READINESS_STATE_READY:       "ready",
	wsv1.ReadinessState_READINESS_STATE_MISSING:     "missing",
	wsv1.ReadinessState_READINESS_STATE_UNSUPPORTED: "unsupported",
	wsv1.ReadinessState_READINESS_STATE_UNKNOWN:     "unknown",
}

// probeHomeAutomation reports a node's LAN-native home-automation
// prerequisites (HomeAutomationReadiness, protocol minor 26, #436). Like
// the radio inventory it is parameterless: a report, never a fix.
func (s *Server) probeHomeAutomation(r *http.Request, _ principal.Identity) (any, int, error) {
	res, node, status, err := s.probeDispatch(r, &wsv1.Operation{Op: &wsv1.Operation_HomeAutomationReadiness{
		HomeAutomationReadiness: &wsv1.HomeAutomationReadiness{},
	}})
	if err != nil {
		return nil, status, err
	}
	hr := res.GetHomeAutomationReadiness()
	ifaces, checks := viewReadiness(hr)
	return map[string]any{
		"node": node, "platformSupported": hr.GetPlatformSupported(),
		"interfaces": ifaces, "checks": checks,
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

// radioUSBView and radioView are the Console's shape for one ListRadios
// entry, shared by the probe endpoint and the home-automation aggregate
// (#446) so the two can never describe the same stick differently.
type radioUSBView struct {
	VendorID     string `json:"vendorId"`
	ProductID    string `json:"productId"`
	Manufacturer string `json:"manufacturer,omitempty"`
	Product      string `json:"product,omitempty"`
	Serial       string `json:"serial,omitempty"`
	Port         string `json:"port,omitempty"`
}

type radioView struct {
	ID         string        `json:"id"`
	Kind       string        `json:"kind"`
	KindReason string        `json:"kindReason"`
	KernelName string        `json:"kernelName"`
	DevPath    string        `json:"devPath,omitempty"`
	ByIDPaths  []string      `json:"byIdPaths"`
	SysfsPath  string        `json:"sysfsPath,omitempty"`
	Bus        string        `json:"bus,omitempty"`
	Driver     string        `json:"driver,omitempty"`
	USB        *radioUSBView `json:"usb,omitempty"`
}

func viewRadios(lr *wsv1.ListRadiosResult) []radioView {
	out := []radioView{}
	for _, d := range lr.GetRadios() {
		kind, ok := radioKindNames[d.GetKind()]
		if !ok {
			kind = "unknown"
		}
		rd := radioView{
			ID: d.GetId(), Kind: kind, KindReason: d.GetKindReason(),
			KernelName: d.GetKernelName(), DevPath: d.GetDevPath(),
			ByIDPaths: append([]string{}, d.GetByIdPaths()...),
			SysfsPath: d.GetSysfsPath(), Bus: d.GetBus(), Driver: d.GetDriver(),
		}
		if d.GetUsbVendorId() != "" {
			rd.USB = &radioUSBView{
				VendorID: d.GetUsbVendorId(), ProductID: d.GetUsbProductId(),
				Manufacturer: d.GetUsbManufacturer(), Product: d.GetUsbProduct(),
				Serial: d.GetUsbSerial(), Port: d.GetUsbPort(),
			}
		}
		out = append(out, rd)
	}
	return out
}

// planRadios folds radio views into what ownership needs: the identity
// and every host path that reaches the device.
func planRadios(rs []radioView) []plan.Radio {
	out := []plan.Radio{}
	for _, r := range rs {
		paths := append([]string{}, r.ByIDPaths...)
		if r.DevPath != "" {
			paths = append(paths, r.DevPath)
		}
		out = append(out, plan.Radio{ID: r.ID, Paths: paths})
	}
	return out
}

// ifaceView and readinessCheckView are the readiness probe's shape, shared
// with the aggregate for the same reason as radioView.
type ifaceView struct {
	Name         string   `json:"name"`
	Addresses    []string `json:"addresses"`
	Up           bool     `json:"up"`
	Multicast    bool     `json:"multicast"`
	DefaultRoute bool     `json:"defaultRoute"`
}

type readinessCheckView struct {
	Check   string `json:"check"`
	State   string `json:"state"`
	Subject string `json:"subject"`
	Value   string `json:"value,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

func viewReadiness(hr *wsv1.HomeAutomationReadinessResult) ([]ifaceView, []readinessCheckView) {
	ifaces := []ifaceView{}
	for _, i := range hr.GetInterfaces() {
		ifaces = append(ifaces, ifaceView{
			Name: i.GetName(), Addresses: append([]string{}, i.GetAddresses()...),
			Up: i.GetUp(), Multicast: i.GetMulticast(), DefaultRoute: i.GetDefaultRoute(),
		})
	}
	checks := []readinessCheckView{}
	for _, c := range hr.GetChecks() {
		state, ok := readinessStateNames[c.GetState()]
		if !ok {
			state = "unknown"
		}
		checks = append(checks, readinessCheckView{
			Check: c.GetCheck(), State: state, Subject: c.GetSubject(),
			Value: c.GetValue(), Detail: c.GetDetail(),
		})
	}
	return ifaces, checks
}
