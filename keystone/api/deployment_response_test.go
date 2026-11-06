package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

func TestDeploymentResponsePreservesTypedEvidenceAndRedactsSecrets(t *testing.T) {
	commit := strings.Repeat("ab", 20)
	digest := strings.Repeat("cd", 32)
	secret40 := strings.Repeat("ef", 20)
	secret64 := strings.Repeat("12", 32)
	secretReview := &runtimedb.DeploymentSecretReview{Validation: "encrypted-reference-validated", Target: runtimedb.DeploymentSecretTarget{Namespace: "example-app", Name: "example-jwt", Key: "jwt-secret", Type: "Opaque"}, SecretGeneration: 7, CiphertextRevision: digest, RecipientFingerprints: []string{"sha256:" + digest}, OutputPath: "secret.sops.yaml", OuterKustomization: runtimedb.DeploymentSecretKustomization{Namespace: "example-flux", Name: "example-app"}, DecryptionSecretName: "example-age-identity", RestartPolicy: "recreate-on-ciphertext-revision", RestartEffect: "singleton-recreate-bounded-outage", PrivateFileCount: 1}
	view := planView{DeploymentPlan: true, PlanRecord: runtimedb.PlanRecord{ID: "plan-example", Summary: "private " + secret40},
		Deployment:       &runtimedb.DeploymentReview{ProductRevision: commit, PreviousProductRevision: commit, ExpectedDeploymentRevision: commit, ChartCommit: commit, ProjectionDigest: digest, ProfileDigest: digest, InventoryDigest: digest, EncryptedSecretRevision: digest, DesiredResource: "private " + secret64, Files: map[string]string{"release.yaml": "private " + secret40, secret64: "private " + secret64}, Secret: secretReview},
		DeploymentStatus: &runtimedb.DeploymentStatus{ProductRevision: commit, PreparedRevision: commit, PromotedRevision: commit, MirrorRevision: commit, SourceRevision: "deploy@sha1:" + commit, ReconciledRevision: "deploy@sha1:" + commit, Detail: "private " + secret64, ControllerDetail: "private " + secret40, SourceGeneration: 2, SourceObservedGeneration: 2, KustomizationGeneration: 3, KustomizationObservedGeneration: 3, HelmGeneration: 4, HelmObservedGeneration: 4, WorkloadGeneration: 5, WorkloadObservedGeneration: 5, ObservedAt: time.Now().UTC()},
	}
	for _, list := range []bool{false, true} {
		t.Run(map[bool]string{false: "single", true: "review-queue"}[list], func(t *testing.T) {
			recorder := httptest.NewRecorder()
			var body any = view
			if list {
				body = []planView{view}
			}
			(&Server{}).writeJSON(recorder, http.StatusOK, body)
			raw := recorder.Body.Bytes()
			if !json.Valid(raw) {
				t.Fatal("invalid JSON")
			}
			if strings.Contains(string(raw), secret40) || strings.Contains(string(raw), secret64) {
				t.Fatal("secret escaped typed response")
			}
			var item map[string]any
			if list {
				var items []map[string]any
				if err := json.Unmarshal(raw, &items); err != nil {
					t.Fatal(err)
				}
				item = items[0]
			} else {
				if err := json.Unmarshal(raw, &item); err != nil {
					t.Fatal(err)
				}
			}
			review := item["deployment"].(map[string]any)
			status := item["deploymentStatus"].(map[string]any)
			for _, field := range []string{"productRevision", "previousProductRevision", "expectedDeploymentRevision", "chartCommit"} {
				if review[field] != commit {
					t.Errorf("review %s corrupted: %v", field, review[field])
				}
			}
			for _, field := range []string{"projectionDigest", "profileDigest", "inventoryDigest", "encryptedSecretRevision"} {
				if review[field] != digest {
					t.Errorf("review %s corrupted: %v", field, review[field])
				}
			}
			secret := review["secret"].(map[string]any)
			if secret["validation"] != "encrypted-reference-validated" || secret["ciphertextRevision"] != digest || secret["outputPath"] != "secret.sops.yaml" || secret["secretGeneration"] != float64(7) {
				t.Errorf("safe Secret review corrupted: %v", secret)
			}
			for _, field := range []string{"productRevision", "preparedRevision", "promotedRevision", "mirrorRevision"} {
				if status[field] != commit {
					t.Errorf("status %s corrupted: %v", field, status[field])
				}
			}
			for _, field := range []string{"sourceRevision", "reconciledRevision"} {
				if status[field] != "deploy@sha1:"+commit {
					t.Errorf("status %s corrupted", field)
				}
			}
			for field, want := range map[string]float64{"sourceObservedGeneration": 2, "kustomizationObservedGeneration": 3, "helmObservedGeneration": 4, "workloadObservedGeneration": 5} {
				if status[field] != want {
					t.Errorf("schema/evidence %s lost: %v", field, status)
				}
			}
		})
	}
}
func TestDeploymentMetadataExceptionDoesNotApplyToArbitraryReplies(t *testing.T) {
	secret := strings.Repeat("ef", 20)
	bodies := []any{
		map[string]any{"deploymentStatus": map[string]any{"promotedRevision": secret}},
		planView{DeploymentPlan: true, DeploymentStatus: &runtimedb.DeploymentStatus{SourceRevision: secret, Detail: secret}},
		planView{DeploymentPlan: true, Deployment: &runtimedb.DeploymentReview{ProfileDigest: strings.Repeat("a", 40), Files: map[string]string{"promotedRevision": secret}}},
	}
	for _, body := range bodies {
		w := httptest.NewRecorder()
		(&Server{}).writeJSON(w, http.StatusOK, body)
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("untyped/free-text metadata bypassed redaction")
		}
	}
}

func TestDeploymentSecretReviewExceptionRejectsUnsafeMetadata(t *testing.T) {
	private := "REPLACED-private-recipient-or-path"
	valid := runtimedb.DeploymentSecretReview{Validation: "encrypted-reference-validated", Target: runtimedb.DeploymentSecretTarget{Namespace: "example-app", Name: "example-jwt", Key: "jwt-secret", Type: "Opaque"}, SecretGeneration: 1, CiphertextRevision: strings.Repeat("a", 64), RecipientFingerprints: []string{"sha256:" + strings.Repeat("b", 64)}, OutputPath: "secret.sops.yaml", OuterKustomization: runtimedb.DeploymentSecretKustomization{Namespace: "example-flux", Name: "example-app"}, DecryptionSecretName: "example-age-identity", RestartPolicy: "recreate-on-ciphertext-revision", RestartEffect: "singleton-recreate-bounded-outage", PrivateFileCount: 1}
	for _, mutate := range []func(*runtimedb.DeploymentSecretReview){
		func(v *runtimedb.DeploymentSecretReview) { v.OutputPath = "/private/" + private },
		func(v *runtimedb.DeploymentSecretReview) { v.RecipientFingerprints = []string{private} },
		func(v *runtimedb.DeploymentSecretReview) { v.DecryptionSecretName = private },
		func(v *runtimedb.DeploymentSecretReview) { v.PrivateFileCount = 2 },
	} {
		value := valid
		value.RecipientFingerprints = append([]string(nil), valid.RecipientFingerprints...)
		mutate(&value)
		w := httptest.NewRecorder()
		(&Server{}).writeJSON(w, http.StatusOK, planView{DeploymentPlan: true, Deployment: &runtimedb.DeploymentReview{Secret: &value}})
		if strings.Contains(w.Body.String(), private) {
			t.Fatalf("unsafe Secret metadata bypassed redaction: %s", w.Body.String())
		}
	}
}
func TestObservedEmptyPublicCollectionIsJSONArray(t *testing.T) {
	h := newHarness(t)
	req, _ := http.NewRequest("GET", h.srv.URL+"/api/v1/observed", nil)
	req.Header.Set("Authorization", "Bearer "+h.tokens[runtimedb.RoleOperator])
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || strings.TrimSpace(string(raw)) != "[]" {
		t.Fatalf("expected empty array, got %d %s", response.StatusCode, raw)
	}
}
