// Package httpclient is an agentapi.Client for a runtime that runs
// OUTSIDE Keystone (ADR-W039 §3.8; ADR-W040 "REST and long-poll
// first"): the same routes keystone/api/handlers_agent.go serves under
// /api/v1/agent/..., called with nothing but net/http and
// encoding/json.
//
// It exists to be exactly what an external runtime — a Hermes bridge, an
// OpenClaw gateway, anything else ADR-W039 §5–§6 name — actually has: a
// base URL, a bearer token, and the keystone/agentapi wire shapes. It
// must never import a Wildstone internal (the runtime database, the
// store, the API, a driver), because the moment it did it would no
// longer be proof that the contract is reachable from outside the
// process — it would be Keystone code standing in for that proof. The
// structural test pins the same guarantee keystone/agentapi's own does,
// one layer out.
//
// Every method here is a thin, uninterpreted transcription of
// ADR-W040 §2 and §7: the request marshals as the identical agentapi.*
// type the in-process client's method takes, the bearer travels only in
// Authorization, no handle ever reaches a URL or a query string, and a
// gateway refusal (denied, waiting, rejected) comes back as an ordinary
// 200 body — not an error — because that is what agentapi.Client's own
// contract promises a caller. An error here is a transport or protocol
// failure: a non-runtime credential or a refused handle (403, wrapped as
// ErrRefused), an unimplemented method naming its phase (501, as
// *agentapi.NotImplementedError), or anything else (*StatusError).
package httpclient
