package policy

// Tier is the risk-tier vocabulary (ADR-W009 A4). low/medium/high mirror
// plan.RiskGrade; domestic is the ADR-W001 concierge tier — defined and
// validated from day one, mapped to require-approval, and otherwise
// UNUSED until v2 (no behavior branches on it before then).
type Tier string

const (
	TierLow      Tier = "low"
	TierMedium   Tier = "medium"
	TierHigh     Tier = "high"
	TierDomestic Tier = "domestic"
)

// ValidTier reports whether t is in the closed tier vocabulary.
func ValidTier(t Tier) bool {
	switch t {
	case TierLow, TierMedium, TierHigh, TierDomestic:
		return true
	}
	return false
}

// RequiresApproval is the risk-tier policy table: which tiers may
// auto-apply (drift repair) and which need a human decision. Only low
// auto-applies (ADR-W004 A3); an unknown tier fails safe.
func RequiresApproval(t Tier) bool {
	return t != TierLow
}
