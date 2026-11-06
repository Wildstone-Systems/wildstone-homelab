package api

import (
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/capability"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/deployment"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/publish"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"gopkg.in/yaml.v3"
)

func deploymentHarness(t *testing.T, observation ...*deployment.ObservationConfig) (*harness, deployment.Config) {
	t.Helper()
	h := newHarness(t)
	h.s.SetTrustedIngress([]string{"127.0.0.1", "::1"})
	h.owner("example-owner", "example-other-owner")
	h.person("example-member")
	if _, err := h.s.DB.AcquireCoordinator("example-coordinator"); err != nil {
		t.Fatal(err)
	}
	h.s.State.Accepted = h.s.DB.StatefileHead
	root := t.TempDir()
	cfg := deployment.Config{Instance: "example-app", ProfileFile: filepath.Join(root, "profile.yaml"), EncryptedSecretFile: filepath.Join(root, "secret.sops.yaml"), Repository: filepath.Join(root, "deployment.git"), MirrorRepository: filepath.Join(root, "delivery.git"), Bindings: capability.HelmBindings{Name: "example-app", Namespace: "example-app", ServiceAccount: "example-reconciler", ConfigClaim: "example-config", MetadataClaim: "example-metadata", MediaClaim: "example-media", SecretName: "example-jwt"}, SecretDelivery: &deployment.SecretDeliveryConfig{Generation: 1, OutputPath: "secret.sops.yaml", KustomizationNamespace: "example-flux", KustomizationName: "example-app", DecryptionSecretName: "example-age-identity", RestartPolicy: "recreate-on-ciphertext-revision"}}
	profile, err := os.ReadFile("../capability/testdata/audiobookshelf-helm.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.ProfileFile, profile, 0600); err != nil {
		t.Fatal(err)
	}
	secret, err := yaml.Marshal(map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": cfg.Bindings.SecretName, "namespace": cfg.Bindings.Namespace}, "type": "Opaque", "stringData": map[string]any{"jwt-secret": "ENC[" + "REPLACED-payload]"}, "sops": map[string]any{"age": []any{map[string]any{"recipient": "age1syntheticrecipientnotadeploymentidentity", "enc": "-----BEGIN AGE ENCRYPTED FILE-----\nREPLACED-stanza\n-----END AGE ENCRYPTED FILE-----\n"}}, "lastmodified": "2026-11-06T00:00:00Z", "mac": "ENC[" + "REPLACED-mac]", "encrypted_regex": "^(data|stringData)$", "version": "3.13.3"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.EncryptedSecretFile, secret, 0600); err != nil {
		t.Fatal(err)
	}
	if len(observation) > 0 {
		cfg.Observation = observation[0]
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "deployment.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	h.s.Deployments, err = deployment.Open(path, h.s.DB, h.s.State, &publish.Publisher{DB: h.s.DB, State: h.s.State})
	if err != nil {
		t.Fatal(err)
	}
	return h, cfg
}

const deploymentProposal = `{"deployment":{"instance":"example-app","timezone":"UTC"}}`

func TestDeploymentPublicPlanRequiresFreshOwnerAndExactReview(t *testing.T) {
	h, cfg := deploymentHarness(t)
	fresh := sessionCookie(h.s.StartConsoleSession("example-owner", time.Now()))
	member := sessionCookie(h.s.StartConsoleSession("example-member", time.Now()))
	if code, _ := h.sessionRequest("POST", "/api/v1/plans", deploymentProposal, "example-member", member); code != http.StatusUnauthorized {
		t.Fatalf("member proposal: %d", code)
	}
	stale := sessionCookie(h.s.StartConsoleSession("example-owner", time.Now().Add(-time.Hour)))
	if code, _ := h.sessionRequest("POST", "/api/v1/plans", deploymentProposal, "example-owner", stale); code != http.StatusForbidden {
		t.Fatalf("stale proposal: %d", code)
	}
	for _, role := range []runtimedb.Role{runtimedb.RoleAdmin, runtimedb.RoleOperator, runtimedb.RoleButler} {
		if res := h.do(role, "POST", "/api/v1/plans", json.RawMessage(deploymentProposal), nil); res.StatusCode != http.StatusForbidden {
			t.Fatalf("machine %s proposed deployment: %d", role, res.StatusCode)
		}
	}
	for _, body := range []string{
		`{"deployment":{"instance":"example-app","timezone":"UTC","values":{"anything":true}}}`,
		`{"deployment":{"instance":"example-app","timezone":"UTC"},"repository":"/external"}`,
		`{"deployment":{"instance":"example-app","timezone":"UTC"},"delete":[{"kind":"HelmDeployment","name":"example-app"}]}`,
	} {
		if code, out := h.sessionRequest("POST", "/api/v1/plans", body, "example-owner", fresh); code != http.StatusBadRequest {
			t.Fatalf("unsafe proposal %d %v", code, out)
		}
	}
	code, out := h.sessionRequest("POST", "/api/v1/plans", deploymentProposal, "example-owner", fresh)
	if code != http.StatusCreated {
		t.Fatalf("proposal: %d %v", code, out)
	}
	id := out["id"].(string)
	review := out["deployment"].(map[string]any)
	for _, field := range []string{"productRevision", "projectionDigest", "chartCommit", "image", "encryptedSecretRevision", "profileDigest", "inventoryDigest"} {
		if review[field] == nil || review[field] == "" {
			t.Errorf("missing review %s", field)
		}
	}
	secretReview, ok := review["secret"].(map[string]any)
	if !ok || secretReview["validation"] != "encrypted-reference-validated" || secretReview["secretGeneration"] != float64(1) || secretReview["outputPath"] != "secret.sops.yaml" || secretReview["privateFileCount"] != float64(1) {
		t.Fatalf("safe managed Secret review missing: %v", review["secret"])
	}
	raw, _ := json.Marshal(out)
	for _, private := range []string{cfg.Repository, cfg.MirrorRepository, cfg.EncryptedSecretFile, "REPLACED-payload", "REPLACED-mac", "REPLACED-stanza", "age1syntheticrecipientnotadeploymentidentity", "ENC["} {
		if strings.Contains(string(raw), private) {
			t.Errorf("review exposed private material")
		}
	}
	if code, _ := h.sessionRequest("POST", "/api/v1/plans/"+id+"/approve", "", "example-owner", stale); code != http.StatusForbidden {
		t.Fatalf("stale approval: %d", code)
	}
	other := sessionCookie(h.s.StartConsoleSession("example-other-owner", time.Now()))
	if code, _ := h.sessionRequest("POST", "/api/v1/plans/"+id+"/approve", "", "example-other-owner", other); code != http.StatusConflict {
		t.Fatalf("different Owner approved bound review: %d", code)
	}
	for _, role := range []runtimedb.Role{runtimedb.RoleAdmin, runtimedb.RoleOperator, runtimedb.RoleButler} {
		if res := h.do(role, "POST", "/api/v1/plans/"+id+"/approve", nil, nil); res.StatusCode != http.StatusForbidden {
			t.Fatalf("machine %s approved deployment: %d", role, res.StatusCode)
		}
	}
	if code, out := h.sessionRequest("POST", "/api/v1/plans/"+id+"/approve", "", "example-owner", fresh); code != http.StatusOK {
		t.Fatalf("approval: %d %v", code, out)
	}
	h.s.Deployments.Resume()
	code, out = h.sessionRequest("GET", "/api/v1/plans/"+id, "", "example-owner", fresh)
	if code != http.StatusOK || out["state"] != "applied" {
		t.Fatalf("local outcome: %d %v", code, out)
	}
	status := out["deploymentStatus"].(map[string]any)
	if status["phase"] != "promoted" || status["reconciliation"] != "unobserved" || status["application"] != "unobserved" {
		t.Fatalf("false readiness: %v", status)
	}
	// The real public encoder must preserve the exact local revision and the
	// full typed review, not merely a nonempty redaction placeholder.
	local := h.s.Deployments.Status(id)
	if status["promotedRevision"] != local.PromotedRevision || status["productRevision"] != local.ProductRevision {
		t.Fatalf("public revision lost: %v", status)
	}
	stored, err := h.s.DB.GetPlan(id)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := runtimedb.DecodeDeploymentPlan(stored.PlanJSON)
	if err != nil {
		t.Fatal(err)
	}
	publicReview := out["deployment"].(map[string]any)
	if publicReview["encryptedSecretRevision"] != decoded.Deployment.Review.EncryptedSecretRevision || publicReview["projectionDigest"] != decoded.Deployment.Review.ProjectionDigest || publicReview["expectedDeploymentRevision"] != decoded.Deployment.Review.ExpectedDeploymentRevision {
		t.Fatalf("public review lost metadata: %v", publicReview)
	}
	// Lists carry only a type flag; the full review is Owner-only.
	var list []map[string]any
	if res := h.do(runtimedb.RoleOperator, "GET", "/api/v1/plans", nil, &list); res.StatusCode != http.StatusOK {
		t.Fatal(res.StatusCode)
	}
	if len(list) != 1 || list[0]["deploymentPlan"] != true || list[0]["deployment"] != nil {
		t.Fatalf("list review leak/type loss: %v", list)
	}
	if res := h.do(runtimedb.RoleOperator, "GET", "/api/v1/plans/"+id, nil, nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("operator read review: %d", res.StatusCode)
	}
	if res := h.do(runtimedb.RoleButler, "GET", "/api/v1/deployments", nil, nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("Agent read inventory: %d", res.StatusCode)
	}
}

func TestDeploymentPublicPlanRevokeAndSecretChangeRefusal(t *testing.T) {
	h, cfg := deploymentHarness(t)
	fresh := sessionCookie(h.s.StartConsoleSession("example-owner", time.Now()))
	propose := func() string {
		t.Helper()
		code, out := h.sessionRequest("POST", "/api/v1/plans", deploymentProposal, "example-owner", fresh)
		if code != http.StatusCreated {
			t.Fatalf("proposal: %d %v", code, out)
		}
		return out["id"].(string)
	}
	id := propose()
	if code, out := h.sessionRequest("POST", "/api/v1/plans/"+id+"/approve", "", "example-owner", fresh); code != http.StatusOK {
		t.Fatalf("approve: %d %v", code, out)
	}
	if code, out := h.sessionRequest("POST", "/api/v1/plans/"+id+"/reject", "", "example-owner", fresh); code != http.StatusOK || out["state"] != "superseded" {
		t.Fatalf("revoke: %d %v", code, out)
	}
	h.s.Deployments.Resume()
	if status := h.s.Deployments.Status(id); status.Phase != "superseded" || status.PromotedRevision != "" {
		t.Fatal(status)
	}
	id = propose()
	raw, err := os.ReadFile(cfg.EncryptedSecretFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.EncryptedSecretFile, append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if code, _ := h.sessionRequest("POST", "/api/v1/plans/"+id+"/approve", "", "example-owner", fresh); code != http.StatusConflict {
		t.Fatalf("changed encrypted version approved: %d", code)
	}
}

func TestGenericPlanCannotEditOrDeleteHelmDesiredInput(t *testing.T) {
	h, _ := deploymentHarness(t)
	want := resource.Resource{ApiVersion: resource.ApiVersion, Kind: resource.KindHelmDeployment, Metadata: resource.Metadata{Name: "example-app", Owner: "capability/audiobooks", Package: "audiobooks-audiobookshelf"}, HelmDeployment: &resource.HelmDeploymentSpec{Package: "audiobooks-audiobookshelf", Version: "1.0.0", Timezone: "UTC", Placement: strings.Repeat("a", 64)}}
	raw, err := resource.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []runtimedb.Role{runtimedb.RoleAdmin, runtimedb.RoleOperator, runtimedb.RoleButler} {
		for _, body := range []map[string]any{{"summary": "bypass", "put": []string{string(raw)}}, {"summary": "bypass", "delete": []map[string]string{{"kind": "HelmDeployment", "name": "example-app"}}}} {
			res := h.do(role, "POST", "/api/v1/plans", body, nil)
			if res.StatusCode < 400 {
				t.Fatalf("generic %s mutation accepted: %d", role, res.StatusCode)
			}
		}
	}
}

func TestDeploymentObservationUsesAuthorizedAPIAndRedactsDependencyErrors(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != "GET" || r.URL.Path != "/apis/source.toolkit.fluxcd.io/v1/namespaces/example-flux/gitrepositories/example-source" {
			t.Errorf("unscoped request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer example-test-observer" {
			t.Error("missing separate observer identity")
		}
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"private diagnostic must not escape"}`)
	}))
	defer server.Close()
	root := t.TempDir()
	ca := filepath.Join(root, "ca.pem")
	token := filepath.Join(root, "token")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.TLS.Certificates[0].Certificate[0]}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(token, []byte("example-test-observer"), 0600); err != nil {
		t.Fatal(err)
	}
	h, _ := deploymentHarness(t, &deployment.ObservationConfig{Server: server.URL, CAFile: ca, TokenFile: token, SourceNamespace: "example-flux", SourceName: "example-source", KustomizationNamespace: "example-flux", KustomizationName: "example-app"})
	fresh := sessionCookie(h.s.StartConsoleSession("example-owner", time.Now()))
	code, out := h.sessionRequest("POST", "/api/v1/plans", deploymentProposal, "example-owner", fresh)
	if code != http.StatusCreated {
		t.Fatal(code, out)
	}
	id := out["id"].(string)
	if code, out := h.sessionRequest("POST", "/api/v1/plans/"+id+"/approve", "", "example-owner", fresh); code != http.StatusOK {
		t.Fatal(code, out)
	}
	h.s.Deployments.Resume()
	if requests.Load() != 0 {
		t.Fatal("cluster queried before explicit authorized status read")
	}
	for _, role := range []runtimedb.Role{runtimedb.RoleOperator, runtimedb.RoleButler} {
		if res := h.do(role, "GET", "/api/v1/plans/"+id, nil, nil); res.StatusCode != http.StatusForbidden {
			t.Fatal(res.StatusCode)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("denied reader triggered cluster access")
	}
	code, out = h.sessionRequest("GET", "/api/v1/plans/"+id, "", "example-owner", fresh)
	if code != http.StatusOK {
		t.Fatal(code, out)
	}
	status := out["deploymentStatus"].(map[string]any)
	if status["reconciliation"] != "dependency_unavailable" || status["application"] != "unobserved" || requests.Load() != 1 {
		t.Fatal(status, requests.Load())
	}
	raw, _ := json.Marshal(out)
	for _, private := range []string{"private diagnostic", server.URL, token, "example-test-observer"} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("private dependency detail leaked")
		}
	}
}
