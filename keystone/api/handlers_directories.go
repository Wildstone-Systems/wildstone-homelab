package api

// The install form's folder picker: which directories exist on a node,
// under the only prefixes a bind mount may point at.
//
// It exists because a free-text path field is the field that let a
// corrupted default through unnoticed. The redactor turned a package's
// own default into "/[REDACTED]", the form served it back as the value
// to submit, and the operator accepted what they were shown — there was
// nothing to compare it against. A control that can only offer folders
// that exist makes that whole class unrepresentable rather than merely
// fixed.
//
// The roots are the managed prefixes and nothing else, which is both the
// safety boundary and the honest one: the node refuses a bind mount
// outside them at apply time, so a picker that browsed the whole
// filesystem would offer folders that can never work. An operator whose
// media lives on /mnt learns that here, from a picker that does not
// offer it, rather than from a container that fails three ticks later.

import (
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// BindRoots are the paths a picker may browse, and the paths a bind
// mount may name. Kept here as the API's copy of the node's own
// ManagedPrefixes: Keystone cannot read the agent's configuration, and a
// picker offering a root the node refuses would be worse than no picker.
var BindRoots = []string{"/srv/wildstone", "/etc/wildstone"}

type directoryListing struct {
	// Path is the directory that was listed, cleaned.
	Path string `json:"path"`
	// Parent is the directory above it, or empty at a root — so the
	// picker can offer "up" without doing path arithmetic of its own.
	Parent string `json:"parent,omitempty"`
	// Roots are the paths a bind mount may name at all. The picker shows
	// them when nothing is chosen yet, and they are the answer to "why
	// can I not see /mnt".
	Roots []string `json:"roots"`
	// Directories are the immediate subdirectories, sorted.
	Directories []string `json:"directories"`
}

// underBindRoot reports whether p is one of the roots or inside one.
func underBindRoot(p string) bool {
	for _, root := range BindRoots {
		if p == root || strings.HasPrefix(p, root+"/") {
			return true
		}
	}
	return false
}

func (s *Server) listDirectories(r *http.Request, _ principal.Identity) (any, int, error) {
	node := r.PathValue("node")
	if node == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("node is required")
	}
	q := strings.TrimSpace(r.URL.Query().Get("path"))
	if q == "" {
		// Nothing chosen yet: offer the roots rather than guessing one.
		return directoryListing{Roots: BindRoots, Directories: []string{}}, http.StatusOK, nil
	}
	clean := path.Clean(q)
	if !underBindRoot(clean) {
		return nil, http.StatusForbidden, fmt.Errorf(
			"%s is not somewhere a service's data can live: a bind mount must be under %s",
			clean, strings.Join(BindRoots, " or "))
	}

	res, err := s.Agents.Apply(r.Context(), node, &wsv1.Operation{Op: &wsv1.Operation_ListDirectory{
		ListDirectory: &wsv1.ListDirectory{Path: clean},
	}})
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("node %s: %w", node, err)
	}
	if oe := res.GetError(); oe != nil {
		return nil, opStatus(oe), fmt.Errorf("node %s: %s", node, oe.GetMessage())
	}

	out := directoryListing{Path: clean, Roots: BindRoots, Directories: []string{}}
	if !isBindRoot(clean) {
		out.Parent = path.Dir(clean)
	}
	out.Directories = append(out.Directories, res.GetListDirectory().GetDirectories()...)
	return out, http.StatusOK, nil
}

func isBindRoot(p string) bool {
	for _, root := range BindRoots {
		if p == root {
			return true
		}
	}
	return false
}
