package api

// The Files surface (#197): a household member drops a photo on the
// dashboard and it lands on the household's own storage.
//
// WHAT IS NOT HERE is the shape of this file. There is no path in any
// request, no storage credential, no reference to whatever file manager
// the household happens to run, and no way for a caller to say anything
// about the filesystem except a destination from a list and a name that
// is normalised before it is used. The dashboard is a presentation
// adapter; this is the capability boundary it talks to.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/chickenrat17/wildstone-homelab/keystone/portal"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// filedropSettingsFile is the file the filedrop capability renders, and
// the ONLY way the storage root reaches this process.
//
// A capability that installs no container still has to tell the control
// plane something, and desired state is the one carrier that survives
// the gap between "an operator answered an install form" and "a request
// arrives eighteen days later" — the same problem the dashboard's title
// solves with a container environment variable (dashboardTitleEnv), and
// the same answer in the one shape a containerless package has.
//
// Read from the RESOURCE rather than from the file on disk. Both exist;
// the resource is desired state, and reading the copy the node happens
// to hold would make the answer depend on whether a write had converged
// yet.
const filedropSettingsFile = "settings.yaml"

// filedropSettings is that file's contents.
type filedropSettings struct {
	// Root is the administrator-configured storage root: the same host
	// path a file manager would be given, chosen at install time.
	Root string `yaml:"root"`
	// MaxUploadMB bounds one upload. Zero takes the package default.
	MaxUploadMB int64 `yaml:"maxUploadMB"`
}

// filedropStore builds the Files data plane from desired state.
//
// A store with no root is returned rather than an error: the surface is
// installed, so its endpoints must answer — and what they must say is
// "this is installed and has nowhere to put anything", which is a
// configuration fault an operator can act on. Answering 404 would send
// them looking for a missing capability instead.
func filedropStore(desired []resource.Resource, member string) *portal.FileStore {
	store := &portal.FileStore{Member: member}
	for _, r := range desired {
		if r.Kind != resource.KindConfigFile || r.ConfigFile == nil {
			continue
		}
		if strings.TrimPrefix(r.Metadata.Owner, "capability/") != surfaceFiles.capability {
			continue
		}
		if r.ConfigFile.Filename != filedropSettingsFile {
			continue
		}
		var settings filedropSettings
		if err := yaml.Unmarshal([]byte(r.ConfigFile.Content), &settings); err != nil {
			// A malformed settings file leaves the store unconfigured,
			// which every endpoint already reports honestly. Falling
			// back to a default root would be inventing a place to put
			// the household's photos.
			return store
		}
		store.Root = strings.TrimSpace(settings.Root)
		store.MaxUpload = settings.MaxUploadMB << 20
		return store
	}
	return store
}

// filedropCapabilities is what a client needs before it can offer an
// upload: where files may go, how big one may be, and where the full
// surface is.
//
// The client may hide things for UX, and the server stays authoritative:
// every endpoint below re-checks the destination and the limit rather
// than trusting that this response was honoured.
func (s *Server) filedropCapabilities(_ http.ResponseWriter, r *http.Request, member string, desired []resource.Resource) (any, int, error) {
	store := filedropStore(desired, member)
	// Only the destinations this member can actually use. A namespace a
	// member has no grant for is not rendered (§12.1), and "me" needs a
	// member name this deployment can turn into a directory — offering a
	// tab that answers 404 would be a control that half-works.
	namespaces := make([]map[string]string, 0, len(portal.Namespaces))
	for _, ns := range portal.Namespaces {
		if ns.Key == "me" {
			if _, ok := portal.PersonalDir(member); !ok {
				continue
			}
		}
		row := map[string]string{"key": ns.Key, "title": ns.Title}
		// What the page prints beside the tabs, so a member knows where
		// a file goes before they drop it. The personal one says who
		// can see it, because "Mine" on its own would be read as a
		// promise this cannot keep against whoever runs the machine.
		if ns.Key == "me" {
			row["note"] = "Only you can see this from the dashboard. Whoever looks after the machine can still read it."
		}
		namespaces = append(namespaces, row)
	}
	body := map[string]any{
		"member":         member,
		"namespaces":     namespaces,
		"maxUploadBytes": store.MaxUploadFor(),
		"browse":         "/filedrop/",
		"ready":          store.Ready(),
	}
	if !store.Ready() {
		body["reason"] = "no storage root is configured for this household's files"
	}
	return body, http.StatusOK, nil
}

// filedropRecent is the widget's list. A bounded limit, because the
// caller supplies it.
func (s *Server) filedropRecent(_ http.ResponseWriter, r *http.Request, member string, desired []resource.Resource) (any, int, error) {
	limit := 5
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 50 {
			return nil, http.StatusBadRequest, fmt.Errorf("limit must be between 1 and 50")
		}
		limit = n
	}
	items, err := filedropStore(desired, member).Recent(limit)
	if err != nil {
		return nil, filedropStatus(err), err
	}
	return map[string]any{"items": items}, http.StatusOK, nil
}

// filedropList is one destination's contents, for the full surface.
func (s *Server) filedropList(_ http.ResponseWriter, r *http.Request, member string, desired []resource.Resource) (any, int, error) {
	items, err := filedropStore(desired, member).List(r.URL.Query().Get("namespace"))
	if err != nil {
		return nil, filedropStatus(err), err
	}
	return map[string]any{"items": items}, http.StatusOK, nil
}

// filedropUpload streams one file to storage.
//
// The body IS the file. Not multipart, and that is a decision rather
// than a shortcut: a multipart request carries a filename inside the
// body, which means the thing that decides where bytes land is parsed
// from the same stream the bytes are in. Here the destination and the
// offered name are query parameters, the body is opaque, and the copy
// to disk never has to be paused to re-read a header.
//
// Nothing about the CONTENT decides anything. The content type a
// browser sent is not consulted, not stored, and not trusted; what
// comes back out is served as an attachment whatever it is, which is
// the only bounded answer to "somebody uploaded a .html".
func (s *Server) filedropUpload(_ http.ResponseWriter, r *http.Request, member string, desired []resource.Resource) (any, int, error) {
	store := filedropStore(desired, member)
	if !store.Ready() {
		return nil, http.StatusServiceUnavailable,
			fmt.Errorf("no storage root is configured for this household's files")
	}
	// The declared length, refused before the body is read. A phone on
	// a slow connection should be told immediately rather than after
	// uploading half a film; the streaming limit below is still what
	// enforces it, because Content-Length is the caller's claim.
	if r.ContentLength > 0 && r.ContentLength > store.MaxUploadFor() {
		return nil, http.StatusRequestEntityTooLarge,
			fmt.Errorf("that file is larger than this household allows (%d bytes)", store.MaxUploadFor())
	}
	defer r.Body.Close()

	stored, err := store.Save(r.URL.Query().Get("namespace"), r.URL.Query().Get("name"), r.Body)
	if err != nil {
		return nil, filedropStatus(err), err
	}
	// TWO STREAMS, because they answer different questions, and the
	// upload belongs in both.
	//
	// The control-plane stream is the operator's record: who wrote
	// what, where, and how big, in the same log as every other thing
	// Wildstone did. It names the file and the namespace because an
	// operator investigating storage needs exactly that.
	s.event("filedrop.upload", "", principal.Identity{Name: portalPrincipal, ActingFor: member},
		fmt.Sprintf("%s uploaded %s to %s (%d bytes)", member, stored.Name, stored.Namespace, stored.Size))
	// The HOUSEHOLD stream is what the family reads, and it is what
	// makes "Today at home" show an upload at all — #197's audit record
	// "is also what feeds Today at home", and until this append existed
	// it fed only the operator's log. The family surface renders it
	// through portal.Sentence, so what a member sees is "Sandy added a
	// file" rather than any of the detail above.
	//
	// DELIBERATELY WITHOUT THE FILENAME. A household event is read by
	// routines, by the morning report and by every member whose
	// audience scope includes it; the operator's line above is the one
	// that names the file. A photo called something private should not
	// become a sentence on a wall tablet because somebody uploaded it.
	if _, _, err := s.DB.AppendHouseholdEvent(runtimedb.HouseholdEvent{
		// Addressed by the member and the stored name so a retried
		// upload of the same file collapses onto one event rather than
		// reporting the same thing twice.
		EventID:       "filedrop:" + member + ":" + stored.Namespace + ":" + stored.Name,
		Source:        "filedrop",
		Type:          "filedrop.uploaded",
		Subject:       member,
		Entity:        stored.Namespace,
		OccurredAt:    time.Now(),
		SchemaVersion: 1,
	}); err != nil {
		// Failure to record does not fail the upload: the file is
		// already stored, and a household member holding a phone should
		// not be told their photo did not arrive because a log write
		// did not.
		s.Log.Error("household event append failed", "type", "filedrop.uploaded", "err", err)
	}
	return stored, http.StatusCreated, nil
}

// filedropFile serves one stored file back.
//
// ALWAYS as an attachment, always as an opaque byte stream, never
// sniffed. A household member can upload an HTML file, and this
// endpoint shares an origin with the portal's own pages — so serving
// what was uploaded as what it claims to be would be handing anyone who
// can upload a script on the surface that holds everyone's session.
func (s *Server) filedropFile(w http.ResponseWriter, r *http.Request, member string, desired []resource.Resource) (any, int, error) {
	name := r.URL.Query().Get("name")
	f, info, err := filedropStore(desired, member).Open(r.URL.Query().Get("namespace"), name)
	if err != nil {
		return nil, filedropStatus(err), err
	}
	defer f.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	// The filename is quoted and re-derived, so the header cannot be
	// split by a name that contains a quote or a newline — and it
	// cannot, because SafeName is what allowed the file to exist.
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", portal.SafeName(path.Base(name))))
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, f); err != nil {
		s.Log.Error("serving a portal file", "err", err)
	}
	return nil, http.StatusOK, nil
}

// filedropStatus maps the store's errors onto HTTP.
//
// Every one of them is the caller's fault except the missing root, and
// each says which: a destination that does not exist is a 400 and not a
// 404, because the destination is a field in a request rather than a
// thing at a URL.
func filedropStatus(err error) int {
	switch {
	case errors.Is(err, portal.ErrNoRoot):
		return http.StatusServiceUnavailable
	case errors.Is(err, portal.ErrNamespace):
		return http.StatusBadRequest
	case errors.Is(err, portal.ErrTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, portal.ErrNotFound):
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}

// filedropDelete removes one file.
//
// A POST rather than a DELETE, matching every other write on this
// surface: the portal's pages are plain documents with no XHR helper
// library, and one verb across the whole surface is one fewer thing for
// a reviewer to check when asking "can this be triggered by a link".
// A GET could be; a POST from another origin cannot read the reply, and
// the CSP here sets form-action 'none' so the page's own markup cannot
// post anywhere either.
func (s *Server) filedropDelete(_ http.ResponseWriter, r *http.Request, member string, desired []resource.Resource) (any, int, error) {
	store := filedropStore(desired, member)
	if !store.Ready() {
		return nil, http.StatusServiceUnavailable,
			fmt.Errorf("no storage root is configured for this household's files")
	}
	ns := r.URL.Query().Get("namespace")
	name := r.URL.Query().Get("name")
	if err := store.Delete(ns, name); err != nil {
		return nil, filedropStatus(err), err
	}
	// The operator's record names the file; the household's does not.
	s.event("filedrop.delete", "", principal.Identity{Name: portalPrincipal, ActingFor: member},
		fmt.Sprintf("%s deleted %s from %s", member, name, ns))
	if _, _, err := s.DB.AppendHouseholdEvent(runtimedb.HouseholdEvent{
		// Not deduplicated on the name: deleting, re-uploading and
		// deleting again is three things that happened, not one.
		EventID:       fmt.Sprintf("filedrop-del:%s:%s:%d", member, ns, time.Now().UnixNano()),
		Source:        "filedrop",
		Type:          "filedrop.deleted",
		Subject:       member,
		Entity:        ns,
		OccurredAt:    time.Now(),
		SchemaVersion: 1,
	}); err != nil {
		s.Log.Error("household event append failed", "type", "filedrop.deleted", "err", err)
	}
	return map[string]any{"deleted": name, "namespace": ns}, http.StatusOK, nil
}

// filedropRename changes one file's name inside its namespace.
func (s *Server) filedropRename(_ http.ResponseWriter, r *http.Request, member string, desired []resource.Resource) (any, int, error) {
	store := filedropStore(desired, member)
	if !store.Ready() {
		return nil, http.StatusServiceUnavailable,
			fmt.Errorf("no storage root is configured for this household's files")
	}
	ns := r.URL.Query().Get("namespace")
	from := r.URL.Query().Get("name")
	to := r.URL.Query().Get("to")
	final, err := store.Rename(ns, from, to)
	if err != nil {
		return nil, filedropStatus(err), err
	}
	s.event("filedrop.rename", "", principal.Identity{Name: portalPrincipal, ActingFor: member},
		fmt.Sprintf("%s renamed %s to %s in %s", member, from, final, ns))
	// No household event: a rename is not a thing that happened in the
	// house, it is a thing that happened to a filename. "Today at home"
	// carrying it would push out an arrival or a routine for a row
	// nobody in the family is served by.
	return map[string]any{"name": final, "namespace": ns, "renamed": final != to}, http.StatusOK, nil
}
