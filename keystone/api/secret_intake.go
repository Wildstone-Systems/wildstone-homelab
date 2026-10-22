package api

import (
	"fmt"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/redact"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"regexp"
	"strings"
)

// Explicit syntax avoids asking a model to extract a value before it is safe
// to send to a model. Ambiguous prose redirects to the protected entry form.
var secretCommand = regexp.MustCompile(`(?s)^/secret (personal|household) ([A-Za-z][A-Za-z0-9 ._-]{0,63}) = (.+)$`)

const secretEntryNote = "Use the Secrets page on Home (/home/secrets/) or the Console to store or reveal a value securely. It will not be included in this conversation."

func (s *Server) prepareSecretInput(actor, text string, connection int64) (string, bool, error) {
	match := secretCommand.FindStringSubmatch(strings.TrimSpace(text))
	if match == nil && !redact.SecretSubmission(text) && !strings.HasPrefix(strings.TrimSpace(text), "/secret ") {
		return text, false, nil
	}
	notice := secretEntryNote
	if s.SecretVault != nil && connection != 0 {
		if err := s.SecretVault.RecordDisclosure(actor, connection); err != nil {
			return "", true, fmt.Errorf("cannot record secret disclosure safely")
		}
	}
	if connection != 0 {
		notice = "If you entered a secret here, it has already been disclosed to this messaging platform. I will not repeat it. " + notice
	}
	if match == nil || s.SecretVault == nil {
		return notice, true, nil
	}
	obj, err := s.SecretVault.Create(actor, runtimedb.SecretInput{Scope: match[1], Label: match[2], Value: match[3], Submit: match[1] == "household", OriginConnection: connection})
	if err != nil {
		return "", true, fmt.Errorf("secret storage unavailable; use the protected Secrets page")
	}

	if obj.State == "active" {
		s.rememberSecretReference(actor, obj)
	}

	return "Stored securely. Reference: " + obj.ID + ". " + notice, true, nil
}

// Only an explicitly safe label and opaque reference enter memory/indexing.
// Household submission labels become shared only after Owner approval.
func (s *Server) rememberSecretReference(actor string, obj runtimedb.SecretObject) {
	owner, audience := actor, runtimedb.ButlerAudiencePrivate
	if obj.Scope == "household" {
		owner, audience = runtimedb.HouseholdOwner, runtimedb.ButlerAudienceShared
	}
	content := "Secure item: " + obj.Label + "; reference " + obj.ID
	seq, err := s.DB.RecordMemory(runtimedb.MemoryRecord{Scope: obj.Scope, Kind: "fact", Content: content, Owner: owner, Author: actor, Audience: audience, PermissionLabel: owner, SourceKind: "secret_reference", SourceID: obj.ID})
	if err == nil {
		_, err = s.DB.IndexDocumentInto(runtimedb.IndexDocument{MemorySeq: seq, SourceKind: "secret_reference", SourceID: obj.ID, Content: content, Owner: owner, Audience: audience, PermissionLabel: owner})
	}
	if err != nil {
		s.Log.Warn("secret stored without searchable memory reference")
	}
}
