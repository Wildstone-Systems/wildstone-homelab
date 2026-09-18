package api

// The states the design draws, detected rather than simulated.
//
// §8 gives the mockup four page states — normal, down, tailnet only,
// fresh install — and a control to switch between them, "because the
// dashboard's worst moments are the ones a design review skips". The
// control is a review affordance. Three of the four are REAL conditions
// this deployment can be in, and the right way to build them is to
// detect them, not to offer a household a dropdown that makes their
// dashboard pretend an app is down.
//
// So there is no pageState here. There is a band that says "Photos
// isn't answering" when Photos is not answering, says "you're on the
// household network" where that is the posture, and says "nothing
// installed yet" on a fresh install — and says nothing at all on a
// normal day, which is the fourth state and the one that needs no code.
//
// LEAD WITH WHAT IS NOT LOST. §8's wording, and the reason these
// carry a consequence rather than only a fault: "Photos is unreachable,
// and Cameras is unreachable because its machine is offline — a
// different fact, said differently."

import (
	"fmt"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/ingressorigin"
	"github.com/chickenrat17/wildstone-homelab/keystone/reconcile"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// condition is one thing the household should be told about the state
// of the house, in the band above everything else.
type condition struct {
	// Kind picks the note's colour: "bad", "warn" or "info". Never
	// "ok" — a band that congratulates a household on a working house
	// is a band they learn to scroll past.
	Kind string `json:"kind"`
	// Title is one household sentence.
	Title string `json:"title"`
	// Detail is the consequence, which is the half a family actually
	// needs: what is not lost, and whether anything is expected of them.
	Detail string `json:"detail,omitempty"`
	// Dismissible marks a standing fact rather than a fault — the
	// tailnet note is true every day and should be sayable once.
	Dismissible bool `json:"dismissible,omitempty"`
	// ID keys the per-member dismissal.
	ID string `json:"id,omitempty"`
}

// maxDownNamed is how many unreachable apps are named before the band
// counts the rest. Two, because the band is a band: a house with nine
// things down has one problem, not nine, and a list that long pushes
// everything a member came for off the screen.
const maxDownNamed = 2

// homeConditions builds the band from what is actually true.
func (s *Server) homeConditions(desired []resource.Resource, apps []reconcile.App,
	status map[string]string, statusKnown bool) []condition {
	out := []condition{}

	// Fresh install. Zero routed services is the NORMAL state of a
	// correct install — sign-in, ingress and the dashboard all precede
	// anything a family uses — so this says the install is working and
	// names the command, rather than reading as a failure.
	if len(apps) == 0 || (len(apps) == 1 && apps[0].Href == "") {
		return append(out, condition{
			Kind:   "warn",
			Title:  "Nothing installed yet",
			Detail: "Sign-in, the network and this dashboard are running — a fresh install, working as intended. Apps appear here as the owner turns them on.",
		})
	}

	// Not answering. Named, and the machine's own state distinguishes
	// "the app is down" from "the machine it runs on is offline",
	// which are different facts with different remedies.
	if statusKnown {
		offline := s.offlineNodes()
		var down []reconcile.App
		for _, a := range apps {
			if status[a.Route] == "bad" {
				down = append(down, a)
			}
		}
		for i, a := range down {
			if i >= maxDownNamed {
				out = append(out, condition{
					Kind:  "bad",
					Title: fmt.Sprintf("%d more apps aren't answering", len(down)-maxDownNamed),
				})
				break
			}
			c := condition{Kind: "bad", Title: a.Title + " isn't answering"}
			if a.Node != "" && offline[a.Node] {
				// A different fact, said differently — and it is the
				// one that tells a member not to bother retrying.
				c.Title = a.Title + " can't be reached — the machine it runs on is offline"
				c.Detail = "Nothing is lost. It comes back with the machine."
			} else {
				c.Detail = "Wildstone is retrying. Whatever it holds is still there and still backed up."
			}
			out = append(out, c)
		}
	}

	// Tailnet only. §8: say it once, in household language, "instead of
	// letting a member discover it as a broken link from a café".
	if ingressorigin.Of(desired).TLS == "tailscale" {
		out = append(out, condition{
			Kind:        "info",
			ID:          "tailnet",
			Dismissible: true,
			Title:       "You're on the household network",
			Detail:      "Everything below is reachable from here. Off the network, these links only work with the household VPN on — the addresses are names on your own network, not on the internet.",
		})
	}
	return out
}

// offlineNodes is the set of machines that are not connected right now.
//
// From LiveNodes rather than from the observation table: a container
// whose node has gone away stops being observed at all, so "no
// observation" and "the machine is off" look identical from there. The
// gateway knows which agents are actually connected.
func (s *Server) offlineNodes() map[string]bool {
	offline := map[string]bool{}
	nodes, err := s.DB.ListNodes()
	if err != nil {
		return offline
	}
	live := map[string]bool{}
	if s.LiveNodes != nil {
		for _, n := range s.LiveNodes() {
			live[n.NodeID] = true
		}
	} else {
		// With no live view, nothing is claimed to be offline: saying a
		// machine is off when it may not be is worse than saying only
		// that the app is not answering.
		return offline
	}
	for _, n := range nodes {
		if !live[strings.TrimSpace(n.NodeID)] {
			offline[n.NodeID] = true
		}
	}
	return offline
}
