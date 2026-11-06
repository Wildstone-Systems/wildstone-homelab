package deployment

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/capability"
	"gopkg.in/yaml.v3"
)

const secretContractFixture = "testdata/secret-custody"

type secretContractVersions struct {
	Schema              int    `json:"schema"`
	FluxDistribution    string `json:"fluxDistribution"`
	KustomizeController string `json:"kustomizeController"`
	SOPSCLI             string `json:"sopsCLI"`
	AgeCLI              string `json:"ageCLI"`
}

type secretDeliveryContract struct {
	Generation             uint64 `json:"generation"`
	OutputPath             string `json:"outputPath"`
	KustomizationNamespace string `json:"kustomizationNamespace"`
	KustomizationName      string `json:"kustomizationName"`
	DecryptionSecretName   string `json:"decryptionSecretName"`
	RestartPolicy          string `json:"restartPolicy"`
}

type childAInventory struct {
	Instance            string                  `json:"instance"`
	ProfileFile         string                  `json:"profileFile"`
	EncryptedSecretFile string                  `json:"encryptedSecretFile"`
	Repository          string                  `json:"repository"`
	MirrorRepository    string                  `json:"mirrorRepository"`
	Bindings            capability.HelmBindings `json:"bindings"`
	SecretDelivery      secretDeliveryContract  `json:"secretDelivery"`
}

type secretReviewTarget struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Key       string `json:"key"`
	Type      string `json:"type"`
}

type secretReviewKustomization struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type safeSecretReview struct {
	Validation            string                    `json:"validation"`
	Target                secretReviewTarget        `json:"target"`
	SecretGeneration      uint64                    `json:"secretGeneration"`
	CiphertextRevision    string                    `json:"ciphertextRevision"`
	RecipientFingerprints []string                  `json:"recipientFingerprints"`
	OutputPath            string                    `json:"outputPath"`
	OuterKustomization    secretReviewKustomization `json:"outerKustomization"`
	DecryptionSecretName  string                    `json:"decryptionSecretName"`
	RestartPolicy         string                    `json:"restartPolicy"`
	RestartEffect         string                    `json:"restartEffect"`
	PrivateFileCount      int                       `json:"privateFileCount"`
}

func contractFile(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(secretContractFixture, name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func strictContractJSON(t *testing.T, name string, out any) []byte {
	t.Helper()
	raw := contractFile(t, name)
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		t.Fatalf("%s: trailing JSON", name)
	}
	return raw
}

func TestSecretCustodyVersionsAreExact(t *testing.T) {
	var got secretContractVersions
	strictContractJSON(t, "versions.json", &got)
	want := secretContractVersions{
		Schema:              1,
		FluxDistribution:    "v2.10.0",
		KustomizeController: "v1.10.1",
		SOPSCLI:             "v3.13.3",
		AgeCLI:              "v1.3.2",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("secret-custody implementation versions changed without contract review: got %+v", got)
	}
	for _, version := range []string{got.FluxDistribution, got.KustomizeController, got.SOPSCLI, got.AgeCLI} {
		if strings.Contains(strings.ToLower(version), "latest") || !regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(version) {
			t.Fatalf("floating or invalid implementation version %q", version)
		}
	}
}

func TestSyntheticSecretCustodyReviewIsBoundAndRedacted(t *testing.T) {
	var inventory childAInventory
	strictContractJSON(t, "privileged-inventory.json", &inventory)
	var versions secretContractVersions
	strictContractJSON(t, "versions.json", &versions)
	var review safeSecretReview
	reviewRaw := strictContractJSON(t, "safe-review.json", &review)
	secretRaw := contractFile(t, "secret.sops.yaml")

	if err := encryptedSecret(secretRaw, inventory.Bindings); err != nil {
		t.Fatalf("synthetic encrypted shape no longer matches the closed validator: %v", err)
	}
	if inventory.Bindings.SecretRevision != "" {
		t.Fatal("privileged inventory selected its own derived Secret revision")
	}
	if inventory.SecretDelivery.Generation == 0 || inventory.SecretDelivery.OutputPath != "secret.sops.yaml" || filepath.IsAbs(inventory.SecretDelivery.OutputPath) || strings.Contains(inventory.SecretDelivery.OutputPath, "..") {
		t.Fatal("private output is not one fixed relative file")
	}
	if inventory.SecretDelivery.RestartPolicy != "recreate-on-ciphertext-revision" {
		t.Fatal("unsupported or missing deterministic restart policy")
	}

	wantRevision := digest(secretRaw)
	if review.Validation != "encrypted-reference-validated" || review.CiphertextRevision != wantRevision || review.SecretGeneration != inventory.SecretDelivery.Generation {
		t.Fatalf("safe review lost validation/generation/exact-byte binding: %+v", review)
	}
	wantTarget := secretReviewTarget{Namespace: inventory.Bindings.Namespace, Name: inventory.Bindings.SecretName, Key: "jwt-secret", Type: "Opaque"}
	if review.Target != wantTarget || review.OutputPath != inventory.SecretDelivery.OutputPath {
		t.Fatalf("safe review target/output differs from privileged inventory: %+v", review)
	}
	wantOuter := secretReviewKustomization{Namespace: inventory.SecretDelivery.KustomizationNamespace, Name: inventory.SecretDelivery.KustomizationName}
	if review.OuterKustomization != wantOuter || review.DecryptionSecretName != inventory.SecretDelivery.DecryptionSecretName || review.RestartPolicy != inventory.SecretDelivery.RestartPolicy {
		t.Fatalf("safe review lost privileged Flux/restart binding: %+v", review)
	}
	if review.RestartEffect != "singleton-recreate-bounded-outage" || review.PrivateFileCount != 1 {
		t.Fatal("safe review omitted the expected effect or exact private-file count")
	}

	var secretDoc struct {
		SOPS struct {
			Age []struct {
				Recipient string `yaml:"recipient"`
			} `yaml:"age"`
			Version string `yaml:"version"`
		} `yaml:"sops"`
	}
	if err := yaml.Unmarshal(secretRaw, &secretDoc); err != nil || len(secretDoc.SOPS.Age) != 1 {
		t.Fatal("synthetic SOPS recipient shape unavailable", err)
	}
	if "v"+secretDoc.SOPS.Version != versions.SOPSCLI {
		t.Fatalf("fixture SOPS version %q differs from pin %q", secretDoc.SOPS.Version, versions.SOPSCLI)
	}
	recipient := strings.TrimSpace(secretDoc.SOPS.Age[0].Recipient)
	h := sha256.Sum256([]byte(recipient))
	wantFingerprint := "sha256:" + hex.EncodeToString(h[:])
	if !reflect.DeepEqual(review.RecipientFingerprints, []string{wantFingerprint}) {
		t.Fatalf("safe review recipient fingerprint mismatch: %v", review.RecipientFingerprints)
	}

	privateValues := []string{
		inventory.ProfileFile,
		inventory.EncryptedSecretFile,
		inventory.Repository,
		inventory.MirrorRepository,
		recipient,
		"REPLACED-synthetic-payload",
		"REPLACED-synthetic-mac",
		"REPLACED-synthetic-age-stanza",
		"ENC[",
		"BEGIN AGE ENCRYPTED FILE",
		"AGE-SECRET-KEY-",
	}
	for _, private := range privateValues {
		if private != "" && bytes.Contains(reviewRaw, []byte(private)) {
			t.Fatalf("safe review exposed private/ciphertext material %q", private)
		}
	}
	if bytes.Contains(secretRaw, []byte("AGE-SECRET-KEY-")) {
		t.Fatal("synthetic public fixture contains an age private identity")
	}
}

func TestSyntheticSecretCustodyFixtureRefusesUnsafeShapes(t *testing.T) {
	var inventory childAInventory
	strictContractJSON(t, "privileged-inventory.json", &inventory)
	raw := contractFile(t, "secret.sops.yaml")

	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"plaintext", func(doc map[string]any) { doc["stringData"].(map[string]any)["jwt-secret"] = "plaintext-value" }},
		{"extra-key", func(doc map[string]any) { doc["stringData"].(map[string]any)["other"] = "ENC[REPLACED-extra]" }},
		{"wrong-name", func(doc map[string]any) { doc["metadata"].(map[string]any)["name"] = "example-other" }},
		{"wrong-namespace", func(doc map[string]any) { doc["metadata"].(map[string]any)["namespace"] = "example-other" }},
		{"missing-mac", func(doc map[string]any) { delete(doc["sops"].(map[string]any), "mac") }},
		{"missing-payload", func(doc map[string]any) { delete(doc, "stringData") }},
		{"extra-document-field", func(doc map[string]any) { doc["immutable"] = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var doc map[string]any
			if err := yaml.Unmarshal(raw, &doc); err != nil {
				t.Fatal(err)
			}
			tc.change(doc)
			changed, err := yaml.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			if err := encryptedSecret(changed, inventory.Bindings); err == nil {
				t.Fatal("unsafe synthetic Secret shape accepted")
			}
		})
	}
}
