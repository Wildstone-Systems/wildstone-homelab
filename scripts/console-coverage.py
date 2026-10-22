#!/usr/bin/env python3
"""Which mutating API endpoints can a person actually reach from the Console?

Run from the repo root:  python3 scripts/console-coverage.py

The question this answers is narrow and worth stating, because a wider
one would be easy to get wrong. It does NOT ask whether a screen is
good, or whether a control is findable. It asks whether console/src/api.ts
mentions the path at all — so an endpoint listed here has no client
method, which means no screen can be calling it however it is written.

An endpoint appearing here is not automatically a bug. Some routes are
machine-facing by design: a chat adapter posts its own inbound messages
and acks its outbound queue, an integration worker claims work and
reports results, a placement handshake is between Keystone and a node.
Those are listed under MACHINE-FACING and excluded from the count, and
that list is maintained by hand precisely so that adding to it is a
decision somebody makes rather than a regex quietly widening.
"""
import re

# Routes whose caller is a bot, an agent or a node — not a person at a
# Console. Excluded from the gap count, deliberately by hand.
MACHINE_FACING = {
    "/api/v1/chat/adapters/self",
    "/api/v1/chat/messages",
    "/api/v1/chat/voice",
    "/api/v1/chat/outbound/{}/ack",
    "/api/v1/chat/outbound/{}/speech",
    "/api/v1/chat/bindings/redeem",
    "/api/v1/integration-work/claim",
    "/api/v1/integration-work/result",
    "/api/v1/integrations/{}/work",
    "/api/v1/integrations/{}/placement/proof",
    "/api/v1/integrations/{}/placement/ready",
    "/api/v1/household/events",
    "/api/v1/butler/requests",
    "/api/v1/butler/tasks/{}/debug",
    # The Agent API (ADR-W039 Phase 2c, #337; ADR-W040): a runtime's own
    # HTTP surface, authenticated as a runtime principal — never a
    # Console session, and there is no screen for it to be missing from.
    "/api/v1/agent/manifest",
    "/api/v1/agent/invoke",
    "/api/v1/agent/recall",
    "/api/v1/agent/remember",
    "/api/v1/agent/checkpoint",
    "/api/v1/agent/start",
    "/api/v1/agent/wait",
    "/api/v1/agent/complete",
    "/api/v1/agent/fail",
    "/api/v1/agent/propose",
    "/api/v1/agent/notify",
    "/api/v1/agent/heartbeat",
    "/api/v1/agent/deliveries",
    "/api/v1/agent/deliveries/{}/ack",
}


def norm(p):
    return re.sub(r"\{[^}]+\}", "{}", p).rstrip("/")


def main():
    api = open("keystone/api/api.go").read()
    found = re.findall(r'mux\.Handle\("(POST|PUT|DELETE|PATCH) (/api/v1/[^"]+)"', api)
    routes = sorted(set(found), key=lambda x: x[1])

    client = open("console/src/api.ts").read()
    cpaths = set()
    for raw in re.findall(r"[`\"](/api/v1/[^`\"]*)[`\"]", client):
        # A query string is not part of the route: /x?seconds=${n} is /x.
        raw = raw.split("?", 1)[0]
        cpaths.add(re.sub(r"\$\{[^}]*\}", "{}", raw).rstrip("/"))

    gaps, machine = [], []
    for verb, path in routes:
        if norm(path) in cpaths:
            continue
        (machine if norm(path) in MACHINE_FACING else gaps).append((verb, path))

    print("Console coverage of mutating endpoints")
    print("=" * 64)
    print("total mutating endpoints : %d" % len(routes))
    print("reachable from Console   : %d" % (len(routes) - len(gaps) - len(machine)))
    print("machine-facing (excluded): %d" % len(machine))
    print("GAPS                     : %d" % len(gaps))
    print()
    if gaps:
        print("NO CONSOLE CLIENT METHOD")
        print("-" * 64)
        for verb, path in gaps:
            print("  %-6s %s" % (verb, path))
    print()
    print("MACHINE-FACING (excluded by design)")
    print("-" * 64)
    for verb, path in machine:
        print("  %-6s %s" % (verb, path))
    return 1 if gaps else 0


if __name__ == "__main__":
    raise SystemExit(main())
