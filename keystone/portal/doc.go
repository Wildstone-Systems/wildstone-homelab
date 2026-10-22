// Package portal is the household's own surface: the two things a
// family member can DO from the dashboard, as opposed to the
// twenty-odd applications they can be sent to.
//
// It exists because of the shape both #197 and #198 arrive in. The
// dashboard is Homepage, and Homepage presents; making it upload a
// photo or hold a conversation means either embedding somebody else's
// whole application in a card, or making the family dashboard
// responsible for storage and authority. Both were rejected, and the
// remaining answer is the one this package is: Wildstone owns a small
// surface of its own, Homepage embeds it, and the boundary — who may
// write, where it may land, which assistant may be spoken to — lives
// here rather than in a dashboard's configuration file.
//
// Two rules hold everything in this package together.
//
// A CALLER NEVER NAMES A PATH. An upload names a NAMESPACE — "shared",
// "photos" — and the namespace is mapped to a directory by the
// administrator-configured root the capability declares. A household
// member cannot express /etc, cannot express ../, and cannot express
// somebody else's subtree, because there is no field in which to say
// it. Normalisation of the one caller-supplied string (the filename)
// is a second line of defence, not the first.
//
// NOTHING HERE AUTHENTICATES. The identity arrives from the forwarded
// headers the ingress sets and keystone/api verifies (auth_portal.go);
// this package is handed a member name that has already been proven.
// Splitting it that way keeps the one file that turns headers into a
// person short enough to read in a sitting, which is the property that
// made ADR-W032 reviewable.
package portal
