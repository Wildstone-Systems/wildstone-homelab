package runtimedb

import "fmt"

// MemoryDestination is supplied by trusted ingress, never a tool argument.
// BuiltIn is true only for the runtime's Console/Homepage constants.
// A registered Connection is always resolved by its immutable row identity.
type MemoryDestination struct {
	BuiltIn       bool
	ConnectionRef int64
	Group         bool
	// RecipientPerson is the canonical direct-chat recipient for an
	// unattended routine. It never grants Person authority to the routine.
	RecipientPerson string
	// ConsoleOwner carries the existing trusted Console access-list exemption.
	// It never bypasses active Person, audience, scope or Agent restrictions.
	ConsoleOwner bool
}

// MemoryReadContext binds canonical identities; zero values deny model reads.
// The store's older nil-context queries are internal inspection APIs. Model
// seams always bind this context, including when identity resolution fails.
type MemoryReadContext struct {
	person      string
	agent       int64
	destination MemoryDestination
	// routine is unattended work reading on nobody's request: a
	// verified chat is the reader's own conversation, which a routine's
	// delivery is not, so it keeps the classification rule.
	routine bool
}

func (db *DB) BindMemoryRead(person, agent string, destination MemoryDestination) *MemoryReadContext {
	c := &MemoryReadContext{destination: destination}
	p, err := db.GetButlerMember(person)
	if err != nil {
		return c
	}
	b, err := db.GetButler(agent)
	if err != nil {
		return c
	}
	c.person, c.agent = p.PersonID, b.Seq
	return c
}

// memoryReadClause is the one content-candidate policy used by recall and
// assembled context. Audience, inherited constraints, scope and disclosure are
// intersections in SQL, before LIMIT and before content enters the process.
func (db *DB) memoryReadClause(alias, person, agent, indexedQuery string, read *MemoryReadContext) (string, []any, error) {
	clause, args, err := db.memoryContentAuthorityClause(alias, person, agent, read)
	if err != nil {
		return "", nil, err
	}
	promotion, promotionArgs, err := db.memoryPromotionClause(alias, agent, person, indexedQuery)
	if err != nil {
		return "", nil, err
	}
	return clause + " AND " + alias + ".superseded_by IS NULL" + promotion, append(args, promotionArgs...), nil
}

// memoryContentAuthorityClause is shared by recall and retained derivatives.
// Candidate selection (supersession and promotion deduplication) belongs only
// to recall: historical sources still carry their live authority constraints.
func (db *DB) memoryContentAuthorityClause(alias, person, agent string, read *MemoryReadContext) (string, []any, error) {
	audience, args, err := db.visibleAudienceClause(alias+".audience", alias+".owner_person_id", person)
	if err != nil {
		return "", nil, err
	}
	inherited, inheritedArgs, err := db.memoryConstraintClause(alias, person)
	if err != nil {
		return "", nil, err
	}
	scope, scopeArgs := memoryScopeClause(alias, agent, person)
	clause := alias + ".forgotten_at = '' AND " + audience + " AND " + inherited + scope
	args = append(args, inheritedArgs...)
	args = append(args, scopeArgs...)
	boundary, boundaryArgs := memoryBoundaryClause(person, agent, read)
	clause += boundary
	args = append(args, boundaryArgs...)
	disclosure, disclosureArgs := memoryDisclosureClause(alias, read)
	return clause + disclosure, append(args, disclosureArgs...), nil
}

func memoryBoundaryClause(person, agent string, read *MemoryReadContext) (string, []any) {
	if read == nil {
		return "", nil
	} // Explicit internal store queries, not model seams.
	if read.person == "" || read.agent == 0 || read.destination.Group {
		return " AND 0", nil
	}
	ownerAccess := read.destination.BuiltIn && read.destination.ConnectionRef == 0 && read.destination.ConsoleOwner
	return ` AND EXISTS (SELECT 1 FROM butler_members mp
        JOIN butlers mb ON mb.seq=?
        WHERE mp.person_id=? AND mp.person_id=? AND mb.name=?
        AND mp.admitted=1 AND mp.disabled_at='' AND mp.offboarded_at=''
        AND mb.retired_at='' AND mb.enabled=1
        AND (mb.agent_class='member' OR (mb.agent_class='owner' AND mp.role='owner'))
        AND (EXISTS (SELECT 1 FROM butler_access ma WHERE ma.person_id=mp.person_id AND ma.butler=mb.seq AND ma.revoked_at='')
             OR (? AND mp.role='owner')))`, []any{read.agent, read.person, person, agent, ownerAccess}
}

func memoryDisclosureClause(alias string, read *MemoryReadContext) (string, []any) {
	if read == nil {
		return "", nil
	}
	d := read.destination
	if d.Group {
		return " AND 0", nil
	}
	if d.BuiltIn && d.ConnectionRef == 0 {
		return "", nil
	}
	if d.ConnectionRef <= 0 {
		return " AND 0", nil
	}
	verified := `EXISTS (SELECT 1 FROM chat_bindings cb
                      WHERE cb.connection=da.seq AND cb.person_id=recipient.person_id AND cb.revoked_at='')`
	if read.routine {
		verified = "0"
	}
	// UNION (not UNION ALL) terminates even if a malformed lineage contains a
	// cycle. Corrections retain their predecessors as well as derivations.
	return fmt.Sprintf(` AND EXISTS (
      SELECT 1 FROM connection_disclosure dest JOIN butler_addresses da ON da.seq=dest.connection
      JOIN connection_admission admission ON admission.connection=da.seq
      JOIN butler_members recipient ON recipient.person_id=?
      WHERE da.seq=? AND da.butler=? AND da.revoked_at='' AND admission.subject_kind='person'
        AND (admission.admits='household' OR (admission.admits='owners' AND recipient.role='owner'))
        -- A VERIFIED ONE-TO-ONE CHAT: the reader's own live binding on
        -- this Connection (the claim-code flow; a group destination was
        -- refused above). Their memory reaches them here wherever it was
        -- recorded, as their conversations do (agent-system-design §7).
        -- The person's own per-item marks (local-only, trusted-UI-only)
        -- are PrivacyPolicy's and still apply.
        AND ((%s)
        OR (dest.retention IN ('none','provider')
        AND dest.reach IN ('local','external')
        AND dest.encryption IN ('none','transport','end_to_end')
        AND dest.surface IN ('trusted_ui','external_messaging')
        AND NOT EXISTS (
          WITH RECURSIVE ancestors(seq) AS (
            SELECT %s.seq UNION
            SELECT edge.source FROM ancestors a JOIN (
              SELECT derivative,source FROM butler_memory_derivations
              UNION SELECT superseded_by,seq FROM butler_memory WHERE superseded_by IS NOT NULL
            ) edge ON edge.derivative=a.seq
          )
          SELECT 1 FROM ancestors a
          LEFT JOIN memory_disclosure_origins o ON o.memory=a.seq
          LEFT JOIN butler_addresses origin ON origin.seq=o.connection_ref
          LEFT JOIN connection_disclosure live ON live.connection=o.connection_ref
          WHERE o.memory IS NULL OR origin.seq IS NULL OR origin.revoked_at<>''
             OR NOT COALESCE(o.reach=dest.reach AND o.encryption=dest.encryption
                  AND o.retention=dest.retention AND o.surface=dest.surface
                  AND live.reach=dest.reach AND live.encryption=dest.encryption
                  AND live.retention=dest.retention AND live.surface=dest.surface,0)
        ))))`, verified, alias), []any{read.person, d.ConnectionRef, read.agent}
}

// DisclosureAllowsContent is stricter than permission to list neutral session
// metadata: full content requires all four resolved classifications to agree.
func DisclosureAllowsContent(origin, destination Disclosure) bool {
	return (origin.Reach == ReachLocal || origin.Reach == ReachExternal) &&
		(origin.Encryption == EncryptionNone || origin.Encryption == EncryptionTransport || origin.Encryption == EncryptionEndToEnd) &&
		(origin.Retention == RetentionNone || origin.Retention == RetentionProvider) &&
		(origin.Surface == TrustedUI || origin.Surface == TrustExternalMessaging) &&
		origin.Reach == destination.Reach && origin.Encryption == destination.Encryption &&
		origin.Retention == destination.Retention && origin.Surface == destination.Surface
}
