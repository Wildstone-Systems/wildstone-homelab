package api

// Onboarding validation probes (ADR-W016 A2 / scope §7.2, E10-U2): the
// live checks the Nodes view runs while pairing a machine — block
// devices for the storage step, listening ports for conflicts, and
// reachability probes for DNS/tunnel/NAS targets. Every underlying op
// is dial-or-read only (TestNfs/TestSmb dial TCP, no mount is ever
// performed — see the proto), so the whole family carries the read verb.

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
