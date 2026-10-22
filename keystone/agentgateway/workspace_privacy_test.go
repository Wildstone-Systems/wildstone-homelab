package agentgateway

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

func TestWorkspaceTaintBlocksGatewayLedgerReuseAndManifest(t *testing.T) {
	f := newFixture(t)
	taskID, h := f.openTask(t, "alfred", "mika")
	task, err := f.db.GetButlerTask(taskID)
	if err != nil {
		t.Fatal(err)
	}
	person, err := f.db.GetButlerMember("mika")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := f.db.GetButler("alfred")
	if err != nil {
		t.Fatal(err)
	}
	w := runtimedb.Workspace{ID: "workspace", PersonID: person.PersonID, AgentID: agent.Seq}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(f.db.CreateWorkspace(w))
	must(f.db.CreateWorkspaceArtifact(runtimedb.WorkspaceArtifact{ID: "artifact", WorkspaceID: w.ID, Path: "report.txt"}))
	must(f.db.CreateWorkspaceRevision(runtimedb.WorkspaceRevision{ID: "revision", ArtifactID: "artifact", TaskID: task.Seq, Hash: strings.Repeat("a", 64), SizeBytes: 1, MediaType: "text/plain", Privacy: "local-only"}))
	must(f.db.ReserveWorkspaceBytes(runtimedb.WorkspaceReservation{ID: "reservation", WorkspaceID: w.ID, TaskID: task.Seq, SizeBytes: 1, Purpose: "publication"}))
	must(f.db.CreateWorkspacePublication(runtimedb.WorkspacePublication{ID: "publication", WorkspaceID: w.ID, ArtifactID: "artifact", RevisionID: "revision", TaskID: task.Seq, ReservationID: "reservation", EffectKey: "effect"}))
	must(f.db.PrepareWorkspacePublication(w.ID, "publication"))
	must(f.db.MarkWorkspaceBytesPublished(w.ID, "publication"))
	must(f.db.CommitWorkspacePublication(w.ID, "publication", "receipt"))
	setProcessingClass(t, f, "alfred", agentapi.ProcessingLocalOnly)
	req := agentapi.InvokeRequest{Handle: h, Tool: "get_system_overview", Args: json.RawMessage(`{}`)}
	first, err := f.g.Invoke(context.Background(), req)
	if err != nil || first.Outcome != agentapi.OutcomeOK {
		t.Fatalf("positive invocation: %+v %v", first, err)
	}
	ctx, p, err := f.db.BeginPrivacyTurn(context.Background(), runtimedb.ModelLocal, runtimedb.MemoryDestination{BuiltIn: true}, "")
	must(err)
	must(p.SetTask(taskID))
	_, err = f.db.ReadWorkspaceRevision(ctx, f.db.BindWorkspaceAccess("mika", "alfred", taskID, "console"), w.ID, "artifact", "revision")
	must(err)
	// No fresh read on the new call: only the persisted workspace edge connects
	// this task to the restriction. Cached results cannot bypass that context.
	setProcessingClass(t, f, "alfred", agentapi.ProcessingCloud)
	result, err := f.g.Invoke(context.Background(), req)
	if err != nil || result.Outcome != agentapi.OutcomeDenied || result.ServedFromLedger || result.Content != "" {
		t.Fatalf("cached result bypassed workspace taint: %+v %v", result, err)
	}
	if count := f.surface("alfred").count(req.Tool); count != 1 {
		t.Fatalf("denied retry dispatched %d times", count)
	}
	payload, err := json.Marshal(map[string]any{"handle": string(h)})
	must(err)
	delivery := FilterDelivery(f.db, runtimedb.AgentDelivery{Butler: agent.Seq, Kind: agentapi.DeliveryTurn, Payload: string(payload)})
	if !strings.Contains(delivery.Payload, `"withheld":true`) {
		t.Fatal("first-turn delivery lost workspace provenance")
	}
	if _, err = f.g.Manifest(context.Background(), h); err == nil {
		t.Fatal("manifest lost persisted workspace source")
	}
}

func TestWorkspaceMemoryDeliveryUsesRecordedDestination(t *testing.T) {
	for _, scenario := range []string{"builtin", "verified", "workspace only verified", "workspace only revoked binding", "workspace only narrowed admission", "workspace only owner narrowed admission", "foreign agent", "revoked binding", "revoked connection", "group", "cloud", "trusted external"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			agentName, personName := "alfred", "mika"
			if strings.Contains(scenario, "narrowed admission") {
				agentName = "joan"
				if scenario == "workspace only narrowed admission" {
					personName = "sam"
				}
			}
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			origin, _ := f.openTask(t, agentName, personName)
			task, err := f.db.GetButlerTask(origin)
			must(err)
			person, err := f.db.GetButlerMember(personName)
			must(err)
			agent, err := f.db.GetButler(agentName)
			must(err)
			var memory int64
			if !strings.HasPrefix(scenario, "workspace only") {
				memory, err = f.db.RecordMemory(runtimedb.MemoryRecord{Scope: runtimedb.MemoryScopePersonal, Kind: runtimedb.MemoryFact, Content: "synthetic workspace input", Owner: personName, Author: personName, Butler: agentName, Audience: runtimedb.ButlerAudiencePrivate, PermissionLabel: "household", SourceKind: "conversation", SourceID: origin})
				must(err)
			}
			w := runtimedb.Workspace{ID: "workspace", PersonID: person.PersonID, AgentID: agent.Seq}
			must(f.db.CreateWorkspace(w))
			must(f.db.CreateWorkspaceArtifact(runtimedb.WorkspaceArtifact{ID: "artifact", WorkspaceID: w.ID, Path: "report.txt"}))
			must(f.db.CreateWorkspaceRevision(runtimedb.WorkspaceRevision{ID: "revision", ArtifactID: "artifact", TaskID: task.Seq, Hash: strings.Repeat("a", 64), SizeBytes: 1, MediaType: "text/plain", Privacy: "normal"}))
			must(f.db.ReserveWorkspaceBytes(runtimedb.WorkspaceReservation{ID: "reservation", WorkspaceID: w.ID, TaskID: task.Seq, SizeBytes: 1, Purpose: "publication"}))
			must(f.db.CreateWorkspacePublication(runtimedb.WorkspacePublication{ID: "publication", WorkspaceID: w.ID, ArtifactID: "artifact", RevisionID: "revision", TaskID: task.Seq, ReservationID: "reservation", EffectKey: "effect"}))
			must(f.db.PrepareWorkspacePublication(w.ID, "publication"))
			must(f.db.MarkWorkspaceBytesPublished(w.ID, "publication"))
			must(f.db.CommitWorkspacePublication(w.ID, "publication", "receipt"))
			var id string
			var handle agentapi.Handle
			var connection int64
			var binding runtimedb.ChatBinding
			destination := runtimedb.MemoryDestination{BuiltIn: true}
			connectionName := "console"
			if scenario == "builtin" || scenario == "cloud" || scenario == "foreign agent" {
				id, handle = f.openTask(t, agentName, personName)
			} else {
				id, handle, connection = f.openTaskOnConnection(t, agentName, personName, "telegram", "synthetic-workspace-chat")
				at, err := f.db.ChatConnectionBySeq(connection)
				must(err)
				hash := fmt.Sprintf("%x", sha256.Sum256([]byte(t.Name())))
				must(f.db.InsertChatClaimCode(hash, runtimedb.ChatClaimCode{Connection: connection, Identity: personName, CreatedBy: "root-admin", ExpiresAt: time.Now().Add(time.Hour)}))
				binding, err = f.db.RedeemChatClaimCode(hash, at, "synthetic-account", "synthetic", "synthetic-conversation")
				must(err)
				destination = runtimedb.MemoryDestination{ConnectionRef: connection}
				connectionName = at.Name()
			}
			ctx, p, err := f.db.BeginPrivacyTurn(context.Background(), runtimedb.ModelLocal, destination, "")
			must(err)
			must(p.SetTask(id))
			_, err = f.db.ReadWorkspaceRevision(ctx, f.db.BindWorkspaceAccess(personName, agentName, id, connectionName), w.ID, "artifact", "revision")
			must(err)
			setProcessingClass(t, f, agentName, agentapi.ProcessingLocalOnly)
			payload, err := json.Marshal(map[string]any{"handle": string(handle)})
			must(err)
			recipient := agent.Seq
			if scenario == "foreign agent" {
				other, err := f.db.GetButler("joan")
				must(err)
				setProcessingClass(t, f, "joan", agentapi.ProcessingLocalOnly)
				recipient = other.Seq
			}
			for _, kind := range []agentapi.DeliveryKind{agentapi.DeliveryTurn, agentapi.DeliverySubscription, agentapi.DeliveryEscalation} {
				_, err := f.db.RecordAgentDelivery(recipient, kind, string(payload))
				must(err)
			}
			queued, err := f.db.ListUnackedAgentDeliveries(recipient, 0, 10)
			must(err)
			if len(queued) != 3 {
				t.Fatalf("queued %d deliveries", len(queued))
			}
			for _, row := range queued {
				got := FilterDelivery(f.db, row)
				if (got.Payload == row.Payload) != (scenario != "foreign agent") {
					t.Fatalf("initial delivery authority for %s", scenario)
				}
			}
			// Recheck the same queued rows after policy/binding changes.
			switch scenario {
			case "revoked binding", "workspace only revoked binding":
				must(f.db.RevokeChatBinding(binding.ID))
			case "workspace only narrowed admission", "workspace only owner narrowed admission":
				_, err := f.db.SetConnectionAdmission("telegram", "synthetic-workspace-chat", runtimedb.AdmitOwners, "root-admin")
				must(err)
			case "revoked connection":
				must(f.db.RevokeButlerAddressFor(agentName, "telegram", "synthetic-workspace-chat", personName))
			case "group":
				must(f.db.NoteChatRoom(connection, "synthetic-conversation"))
			case "cloud":
				must(f.db.SetMemoryPrivacy(personName, memory, runtimedb.PrivacyPolicy{LocalOnly: true}))
				setProcessingClass(t, f, agentName, agentapi.ProcessingCloud)
			case "trusted external":
				must(f.db.SetMemoryPrivacy(personName, memory, runtimedb.PrivacyPolicy{TrustedOnly: true}))
			}
			for _, row := range queued {
				got := FilterDelivery(f.db, row)
				allowed := scenario == "builtin" || scenario == "verified" || scenario == "workspace only verified" || scenario == "workspace only owner narrowed admission"
				if (got.Payload == row.Payload) != allowed {
					t.Fatalf("%s allowed=%v but payload withheld=%v", row.Kind, allowed, got.Payload != row.Payload)
				}
				if got.ID != row.ID || got.Kind != row.Kind || got.Cursor != row.Cursor {
					t.Fatal("filter changed delivery identity")
				}
				if !allowed && !strings.Contains(got.Payload, `"withheld":true`) {
					t.Fatalf("missing refusal: %s", got.Payload)
				}
			}
		})
	}
}
