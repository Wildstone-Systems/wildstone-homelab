package api

import (
	"encoding/json"
	"regexp"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/redact"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// Deployment review/status use a typed serialization exception, like artifact
// selectors. Schema keys are fixed Go JSON tags, not secret values. Only
// server-generated revisions/digests at these exact DTO fields survive the
// value heuristics; prose, YAML, map keys and all other values still redact.
// No new shelter is added to redact.String or to arbitrary JSON/tool replies.
var deploymentCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)
var deploymentDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
var deploymentFluxRevision = regexp.MustCompile(`^deploy@sha1:[0-9a-f]{40}$`)
var deploymentKubeName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
var deploymentRecipientFingerprint = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func safeDeploymentSecretReview(raw json.RawMessage) bool {
	var value runtimedb.DeploymentSecretReview
	if json.Unmarshal(raw, &value) != nil || value.Validation != "encrypted-reference-validated" || value.SecretGeneration == 0 || !deploymentDigest.MatchString(value.CiphertextRevision) || value.OutputPath != "secret.sops.yaml" || value.RestartPolicy != "recreate-on-ciphertext-revision" || value.RestartEffect != "singleton-recreate-bounded-outage" || value.PrivateFileCount != 1 {
		return false
	}
	if !deploymentKubeName.MatchString(value.Target.Namespace) || !deploymentKubeName.MatchString(value.Target.Name) || value.Target.Key != "jwt-secret" || value.Target.Type != "Opaque" || !deploymentKubeName.MatchString(value.OuterKustomization.Namespace) || !deploymentKubeName.MatchString(value.OuterKustomization.Name) || !deploymentKubeName.MatchString(value.DecryptionSecretName) {
		return false
	}
	if len(value.RecipientFingerprints) == 0 {
		return false
	}
	for _, fingerprint := range value.RecipientFingerprints {
		if !deploymentRecipientFingerprint.MatchString(fingerprint) {
			return false
		}
	}
	return true
}

func redactDeploymentFields(value any, revisions map[string]*regexp.Regexp) (json.RawMessage, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	for key, original := range fields {
		fields[key] = json.RawMessage(redact.String(string(original)))
		if key == "secret" && safeDeploymentSecretReview(original) {
			fields[key] = original
			continue
		}
		if pattern := revisions[key]; pattern != nil {
			var value string
			if json.Unmarshal(original, &value) == nil && pattern.MatchString(value) {
				fields[key] = original
			}
		}
	}
	return json.Marshal(fields)
}

func redactedPlanView(view planView) ([]byte, error) {
	if !view.DeploymentPlan {
		raw, err := json.Marshal(view)
		return []byte(redact.String(string(raw))), err
	}
	base := view
	base.Deployment = nil
	base.DeploymentStatus = nil
	raw, err := json.Marshal(base)
	if err != nil {
		return nil, err
	}
	var out map[string]json.RawMessage
	if err = json.Unmarshal([]byte(redact.String(string(raw))), &out); err != nil {
		return nil, err
	}
	if view.Deployment != nil {
		out["deployment"], err = redactDeploymentFields(view.Deployment, map[string]*regexp.Regexp{
			"previousProductRevision": deploymentCommit, "productRevision": deploymentCommit,
			"expectedDeploymentRevision": deploymentCommit, "chartCommit": deploymentCommit,
			"projectionDigest": deploymentDigest, "profileDigest": deploymentDigest,
			"inventoryDigest": deploymentDigest, "encryptedSecretRevision": deploymentDigest,
		})
		if err != nil {
			return nil, err
		}
	}
	if view.DeploymentStatus != nil {
		out["deploymentStatus"], err = redactDeploymentFields(view.DeploymentStatus, map[string]*regexp.Regexp{
			"productRevision": deploymentCommit, "preparedRevision": deploymentCommit,
			"promotedRevision": deploymentCommit, "mirrorRevision": deploymentCommit,
			"sourceRevision": deploymentFluxRevision, "reconciledRevision": deploymentFluxRevision,
		})
		if err != nil {
			return nil, err
		}
	}
	return json.Marshal(out)
}
func redactedPlanViews(views []planView) ([]byte, error) {
	if views == nil {
		return []byte("null"), nil
	}
	out := make([]json.RawMessage, 0, len(views))
	for _, view := range views {
		raw, err := redactedPlanView(view)
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return json.Marshal(out)
}
