package api

// The integration adapter contract (ADR-W030 A5/A5.1, work package
// P10).
//
// Two endpoints face the adapter, and both are deliberately thin. It
// long-polls for the work assigned to the connections its credential is
// bound to, and it reports a result for work it claimed. It names no
// connection when claiming: Keystone decides what an adapter may see, so
// a compromised adapter cannot reach another connection's work by asking
// for it.
//
// The rest of this file is the administrator's side — creating
// connections, binding adapters, recording verified capabilities,
// queuing work — and every one of those needs manage. There is no
// inbound administration surface an adapter can reach, because Keystone
// needs none and one would be the natural place for a compromised
// adapter to start.

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// maxAdapterPoll bounds a long poll. Longer than a chat poll because an
// integration is quieter, short enough that a fenced adapter learns it
// has been superseded within a minute rather than at its next question.
const maxAdapterPoll = 30 * time.Second

func (s *Server) listIntegrationConnections(_ *http.Request, _ principal.Identity) (any, int, error) {
	connections, err := s.DB.ListConnections()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	out := make([]map[string]any, 0, len(connections))
	for _, c := range connections {
		caps, err := s.DB.ListCapabilities(c.Name)
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		out = append(out, map[string]any{"connection": c, "capabilities": caps})
	}
	return map[string]any{"connections": out}, http.StatusOK, nil
}

// createIntegrationConnection registers a named connection. Its defaults
// follow ownership and are not parameters: "connect my calendar" must
// not also mean "index my calendar and send it to a cloud model".
func (s *Server) createIntegrationConnection(r *http.Request, id principal.Identity) (any, int, error) {
	var req struct {
		Name      string `json:"name"`
		Adapter   string `json:"adapter"`
		Endpoint  string `json:"endpoint"`
		Ownership string `json:"ownership"`
		Owner     string `json:"owner"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if req.Owner == "" {
		req.Owner = actingMember(id)
	}
	seq, err := s.DB.CreateConnection(runtimedb.IntegrationConnection{
		Name: req.Name, Adapter: req.Adapter, Endpoint: req.Endpoint,
		Ownership: req.Ownership, Owner: req.Owner, CreatedBy: actingMember(id),
	})
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{
		"seq": seq,
		"note": "read-only, live-query-only and local-only to start; actions, indexing and " +
			"cloud routing are each turned on separately",
	}, http.StatusOK, nil
}

// widenIntegrationConnection turns on actions, indexing or cloud
// routing. Each is a separate switch, and each starts off.
func (s *Server) widenIntegrationConnection(r *http.Request, _ principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	var req struct {
		Actions  bool `json:"actions"`
		Indexing bool `json:"indexing"`
		Cloud    bool `json:"cloud"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if err := s.DB.WidenConnection(name, req.Actions, req.Indexing, req.Cloud); err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("no connection named %q", name)
		}
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{"widened": name}, http.StatusOK, nil
}

// bindIntegrationAdapter binds an adapter credential to a connection.
func (s *Server) bindIntegrationAdapter(r *http.Request, id principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	var req struct {
		Principal string `json:"principal"`
		Unbind    bool   `json:"unbind"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if req.Unbind {
		// Unbinding fences the connection, so anything the old adapter
		// answers afterwards is refused rather than applied.
		if err := s.DB.UnbindAdapterPrincipal(req.Principal, name); err != nil {
			return nil, http.StatusBadRequest, err
		}
		return map[string]any{"unbound": req.Principal, "note": "the connection lease moved; " +
			"work claimed by the old adapter will be refused when it answers"}, http.StatusOK, nil
	}
	if err := s.DB.BindAdapterPrincipal(req.Principal, name, actingMember(id)); err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("no connection named %q", name)
		}
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{"bound": req.Principal}, http.StatusOK, nil
}

// recordIntegrationCapability stores what a connection was VERIFIED to
// support. Recording one says "this works"; it never says "this is
// allowed" (ADR-W030 A5).
func (s *Server) recordIntegrationCapability(r *http.Request, _ principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	var req struct {
		Kind      string `json:"kind"`
		Name      string `json:"name"`
		Version   string `json:"version"`
		Available bool   `json:"available"`
		Detail    string `json:"detail"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if err := s.DB.RecordCapability(name, runtimedb.IntegrationCapability{
		Kind: req.Kind, Name: req.Name, Version: req.Version,
		Available: req.Available, Detail: req.Detail,
	}); err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("no connection named %q", name)
		}
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{
		"recorded": req.Name,
		"note":     "discovery records what works; enabling and granting it are separate acts",
	}, http.StatusOK, nil
}

// queueIntegrationWork asks a connection a question or gives it an
// action to perform.
func (s *Server) queueIntegrationWork(r *http.Request, _ principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	var req struct {
		Kind          string `json:"kind"`
		Capability    string `json:"capability"`
		CorrelationID string `json:"correlationId"`
		Request       string `json:"request"`
		TimeoutMillis int    `json:"timeoutMillis"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	deadline := time.Duration(req.TimeoutMillis) * time.Millisecond
	seq, err := s.DB.EnqueueIntegrationWork(name, runtimedb.IntegrationWork{
		Kind: req.Kind, Capability: req.Capability,
		CorrelationID: req.CorrelationID, RequestJSON: req.Request,
	}, deadline)
	if err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("no connection named %q", name)
		}
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{"seq": seq}, http.StatusOK, nil
}

// getIntegrationWork reads one item and its answer.
func (s *Server) getIntegrationWork(r *http.Request, _ principal.Identity) (any, int, error) {
	var seq int64
	if _, err := fmt.Sscanf(r.PathValue("seq"), "%d", &seq); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("work id must be a number")
	}
	w, err := s.DB.GetIntegrationWork(seq)
	if err != nil {
		return nil, http.StatusNotFound, fmt.Errorf("no such work")
	}
	return w, http.StatusOK, nil
}

// claimIntegrationWork is the adapter's long poll.
//
// It takes no connection: the adapter gets the work for the connections
// its credential is bound to. An empty answer after the poll window is a
// normal quiet period, not an error — the adapter simply polls again.
func (s *Server) claimIntegrationWork(r *http.Request, id principal.Identity) (any, int, error) {
	// The adapter says how long it is prepared to wait, bounded by
	// maxAdapterPoll. It is a parameter rather than a constant because
	// an adapter that is about to shut down, or one checking whether
	// anything is pending before it sleeps, should not have to hold a
	// connection open for half a minute to find out there is nothing.
	var req struct {
		WaitMillis int `json:"waitMillis"`
	}
	if r.ContentLength > 0 {
		if code, err := decodeStrict(r, &req); err != nil {
			return nil, code, err
		}
	}
	wait := maxAdapterPoll
	if req.WaitMillis > 0 && time.Duration(req.WaitMillis)*time.Millisecond < wait {
		wait = time.Duration(req.WaitMillis) * time.Millisecond
	}
	if req.WaitMillis < 0 {
		wait = 0
	}
	deadline := time.Now().Add(wait)
	for {
		work, ok, err := s.DB.ClaimIntegrationWork(id.Name)
		if err != nil {
			if errors.Is(err, runtimedb.ErrNotClaimable) {
				// Not "you are not bound": an adapter probing the
				// surface learns nothing from an empty queue.
				return map[string]any{"work": nil}, http.StatusOK, nil
			}
			return nil, http.StatusInternalServerError, err
		}
		if ok {
			return map[string]any{"work": work}, http.StatusOK, nil
		}
		if time.Now().After(deadline) || r.Context().Err() != nil {
			return map[string]any{"work": nil}, http.StatusOK, nil
		}
		select {
		case <-r.Context().Done():
			return map[string]any{"work": nil}, http.StatusOK, nil
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// reportIntegrationResult records an adapter's answer.
//
// The four result types are the contract: a valid EMPTY answer is not
// the same as UNAVAILABLE or STALE, and a caller that cannot tell them
// apart will report an outage as an empty shopping list.
func (s *Server) reportIntegrationResult(r *http.Request, id principal.Identity) (any, int, error) {
	var req struct {
		Seq        int64  `json:"seq"`
		ResultType string `json:"resultType"`
		Result     string `json:"result"`
		ObservedAt string `json:"observedAt"`
		Error      string `json:"error"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	var observed time.Time
	if req.ObservedAt != "" {
		var err error
		if observed, err = time.Parse(time.RFC3339, req.ObservedAt); err != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("observedAt must be RFC3339: %w", err)
		}
	}
	err := s.DB.ReportIntegrationResult(id.Name, req.Seq, req.ResultType, req.Result, observed, req.Error)
	switch {
	case errors.Is(err, runtimedb.ErrNotClaimable), errors.Is(err, runtimedb.ErrNotFound):
		// Work this adapter did not claim and work that does not exist
		// answer identically.
		return nil, http.StatusNotFound, fmt.Errorf("no such claimed work")
	case errors.Is(err, runtimedb.ErrAttemptFenced):
		return nil, http.StatusConflict, err
	case err != nil:
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{"recorded": req.Seq}, http.StatusOK, nil
}

// ---------------------------------------------------------------- placement

// listIntegrationPlacements shows where an adapter may run and where it
// currently does.
func (s *Server) listIntegrationPlacements(r *http.Request, _ principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	placements, err := s.DB.ListPlacements(name)
	if err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("no connection named %q", name)
		}
		return nil, http.StatusInternalServerError, err
	}
	secrets, err := s.DB.ListConnectionSecrets(name)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{"placements": placements, "secrets": secrets}, http.StatusOK, nil
}

// addIntegrationPlacement declares a node. Declaring is not preparing:
// the node becomes eligible only once its container is running, its
// secret is provisioned, and readiness is verified.
func (s *Server) addIntegrationPlacement(r *http.Request, id principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	var req struct {
		Node string `json:"node"`
		Role string `json:"role"`
		Rank int    `json:"rank"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if err := s.DB.AddPlacement(name, req.Node, req.Role, req.Rank, actingMember(id)); err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("no connection named %q", name)
		}
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{
		"declared": req.Node,
		"note": "declared, not yet eligible: it takes work only once its container is running, " +
			"its secret is provisioned, and readiness has been verified",
	}, http.StatusOK, nil
}

// markIntegrationPlacementReady records the three facts that make a node
// eligible, and then places the connection.
func (s *Server) markIntegrationPlacementReady(r *http.Request, _ principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	var req struct {
		Node              string `json:"node"`
		ContainerRunning  bool   `json:"containerRunning"`
		SecretProvisioned bool   `json:"secretProvisioned"`
		ReadinessVerified bool   `json:"readinessVerified"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if err := s.DB.MarkPlacementPrepared(name, req.Node,
		req.ContainerRunning, req.SecretProvisioned, req.ReadinessVerified); err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("no placement for %q on %q", name, req.Node)
		}
		return nil, http.StatusBadRequest, err
	}
	return s.placeAndReport(name)
}

// placeIntegrationConnection asks Keystone to put the lease on the best
// prepared node — the call a health check makes when the primary stops
// answering.
func (s *Server) placeIntegrationConnection(r *http.Request, _ principal.Identity) (any, int, error) {
	return s.placeAndReport(r.PathValue("name"))
}

func (s *Server) placeAndReport(name string) (any, int, error) {
	placement, moved, err := s.DB.PlaceConnection(name, 0)
	if errors.Is(err, runtimedb.ErrNoEligibleNode) {
		// A wait, not a failure. Dependent work holds and the Butler
		// reports the outage; nothing improvises a placement.
		if serr := s.DB.SetConnectionState(name, runtimedb.ConnectionUnavailable,
			"no prepared node is eligible to run this adapter"); serr != nil {
			return nil, http.StatusInternalServerError, serr
		}
		return map[string]any{
			"holder":  nil,
			"waiting": true,
			"note": "no prepared node is eligible, so work for this connection waits. " +
				"Nothing was created: a fallback has to be approved and prepared in advance",
		}, http.StatusOK, nil
	}
	if err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("no connection named %q", name)
		}
		return nil, http.StatusInternalServerError, err
	}
	if err := s.DB.SetConnectionState(name, runtimedb.ConnectionReady, ""); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{"holder": placement.Node, "moved": moved}, http.StatusOK, nil
}

// removeIntegrationPlacement fences a node now and starts the removal it
// must then prove.
func (s *Server) removeIntegrationPlacement(r *http.Request, _ principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	var req struct {
		Node string `json:"node"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if err := s.DB.BeginPlacementRemoval(name, req.Node); err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("no placement for %q on %q", name, req.Node)
		}
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{
		"fencing": req.Node,
		"note": "the adapter is fenced already. The connection stays degraded until the container " +
			"removal, the secret erasure and the credential revocation are each proven",
	}, http.StatusOK, nil
}

// proveIntegrationPlacementRemoval records what has actually been
// verified. Partial proof is not proof.
func (s *Server) proveIntegrationPlacementRemoval(r *http.Request, _ principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	var req struct {
		Node              string `json:"node"`
		ContainerRemoved  bool   `json:"containerRemoved"`
		SecretErased      bool   `json:"secretErased"`
		CredentialRevoked bool   `json:"credentialRevoked"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	complete, err := s.DB.ProvePlacementRemoval(name, req.Node,
		req.ContainerRemoved, req.SecretErased, req.CredentialRevoked)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	conn, cerr := s.DB.GetConnection(name)
	if cerr != nil {
		return nil, http.StatusInternalServerError, cerr
	}
	return map[string]any{
		"complete":       complete,
		"state":          conn.State,
		"degradedReason": conn.DegradedReason,
	}, http.StatusOK, nil
}

// onboardIntegrationSecret records that a personal credential exists and
// where its reference lives. The VALUE never comes here — connection
// setup fans it to the eligible nodes over mTLS without persisting it in
// Keystone, and there is no field on this request that could carry one.
func (s *Server) onboardIntegrationSecret(r *http.Request, id principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	var req struct {
		Member           string `json:"member"`
		SecretRef        string `json:"secretRef"`
		UpstreamReadOnly bool   `json:"upstreamReadOnly"`
		Disclosure       string `json:"disclosure"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if req.Member == "" {
		req.Member = actingMember(id)
	}
	seq, err := s.DB.RecordConnectionSecret(runtimedb.ConnectionSecret{
		Connection: name, Member: req.Member, SecretRef: req.SecretRef,
		UpstreamReadOnly: req.UpstreamReadOnly, Disclosure: req.Disclosure,
		CreatedBy: actingMember(id),
	})
	if err != nil {
		if errors.Is(err, runtimedb.ErrNotFound) {
			return nil, http.StatusNotFound, fmt.Errorf("no connection named %q", name)
		}
		return nil, http.StatusBadRequest, err
	}
	return map[string]any{"seq": seq}, http.StatusOK, nil
}
