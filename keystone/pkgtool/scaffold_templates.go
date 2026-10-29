package pkgtool

// The three scaffolds (ADR-W025 Part B item 2).
//
// These are read far more often than they are edited, and by people who
// have not read anything else in this repository — so they carry more
// comment than a hand-written package would, and every comment answers a
// question the author is about to have rather than describing the line
// below it. The TODO markers are the contract: `wild pkg new` counts them
// and tells the author how many decisions are still theirs.
//
// Every one of these lints green as written (TestScaffoldLintsGreen).
// That is not a nicety. A scaffold whose first lint fails teaches the
// reader that failing output is normal, which is the opposite of what a
// contract test is for.

const serviceTemplate = `# __TITLE__ — TODO: replace this paragraph with what this capability IS,
# in the words a household member would use. Not "runs the __NAME__
# container": what the family gets, and why this profile rather than
# another. The Console shows a package by name today, so for now this
# comment is the only place the answer lives.
#
# SHAPE: service. Wildstone runs the app, puts a Route in front of it,
# and registers it with whatever package provides identity:oidc. The
# alternative shapes are for a service that already runs somewhere else:
# see ` + "`wild pkg new`" + ` with no arguments.
#
# TODO: pin a real image digest. The one below is all zeroes — valid
# syntax, no registry will ever serve it — so that this file lints as
# written and fails loudly rather than quietly if it ships unchanged.
# Resolve the real one with:
#
#     wild pkg refresh-digests packages/__NAME__
#
# TODO: lint it, and keep linting it. This package requires identity:oidc,
# which lives in another package, so lint has to be shown the catalog:
#
#     wild pkg lint packages/__NAME__ -catalog packages/identity
apiVersion: wildstone.dev/v1
kind: Package
metadata:
  name: __NAME__
  version: 0.1.0
  capability: __CAPABILITY__
  # The profile is which IMPLEMENTATION of the capability this is. Two
  # packages may serve one capability — the four ingress profiles do —
  # and the profile is what tells them apart.
  profile: __PROFILE__
# TODO: if the image is not published for both supported architectures,
# uncomment this and name exactly what it supports. Omission preserves
# compatibility; a declaration makes install/update fail closed before
# mutation when the selected node does not match.
# platforms: [linux/amd64, linux/arm64]
# What this package needs from the rest of the deployment. Traits, not
# package names: you depend on "something that provides single sign-on",
# and the deployment decides which package that is.
#
# Run ` + "`wild pkg traits -v`" + ` to see every trait, every mode, and the
# .Resolved values each one hands you. Lint refuses one that does not
# exist, and names the ones that do.
requires:
  # Gives you .Resolved.IssuerURL, .Resolved.SessionDomain and
  # .Resolved.DashboardURL. TODO: drop this and the OidcClient below if
  # the app has no OIDC support — but read what that costs first: it
  # means a second set of passwords for the household to keep.
  - { trait: identity, mode: oidc }
  # Structural traits: the system satisfies these, so no package has to
  # provide them. Declaring them is still worth doing — it says what this
  # package assumes about where it lands.
  - { trait: ingress, mode: public }
  - { trait: storage, class: bulk }
policies:
  # TODO: choose honestly. family_public means "reachable from the public
  # internet", and the policy engine will refuse auth: none on a Route
  # carrying it. The full vocabulary is family_public, family_private,
  # admin_tailscale, internal_only, deprecated.
  classifications: [family_public]
  # TODO: how bad is a breach of the data this holds? Photos and
  # documents are high; a dashboard of public transit times is low.
  dataSensitivity: medium
params:
  # source: node and source: domain mean the install form OFFERS the
  # answer from what the deployment already knows, instead of asking.
  # Nothing else should ask for a domain the household has already given.
  - { name: node, required: true, source: node, example: "wildstone", description: "Which machine to install this on." }
  - { name: public_domain, required: true, source: domain, example: "example.com", description: "The domain your services are named under, e.g. home.example.com." }
  # A defaulted param is a question the operator does not have to answer.
  - { name: subdomain, default: "__NAME__", description: "The name this service is reached at, under your domain." }
  #
  # NEVER put a password, token, or key in a param. A param's value is
  # recorded in the plan and the audit trail; lint refuses the obvious
  # names. Secrets are Secret resources — generated on the node, or
  # ` + "`provided: true`" + ` when only a person can supply the right value.
templates:
  # ---- Storage. Declared before the Container that mounts it, so the
  # volume exists when the container starts.
  - |
    apiVersion: wildstone.dev/v1
    kind: Volume
    metadata: { name: __NAME__-data, node: "{{ .Params.node }}" }
    spec: { name: __NAME__-data }
  # ---- The app itself.
  - |
    apiVersion: wildstone.dev/v1
    kind: Container
    metadata: { name: __NAME__, node: "{{ .Params.node }}" }
    spec:
      image: "ghcr.io/example/__NAME__@__DIGEST__"
      env:
        # The OIDC wiring, and the reason this package requires identity.
        # .Resolved.IssuerURL is where the portal is actually SERVED,
        # which under a tailnet is not the host it declares — never
        # rebuild it by hand from public_domain.
        OIDC_ISSUER_URL: "{{ .Resolved.IssuerURL }}"
        OIDC_CLIENT_ID: "__NAME__"
        # A secret REFERENCE, not a value. The node substitutes it when
        # the container is materialised; Wildstone never holds the secret
        # itself. The engine creates the matching Secret for you, because
        # the OidcClient below names the same reference.
        OIDC_CLIENT_SECRET: "{{ .Resolved.OidcSecretRef }}"
        # TODO: the app's own configuration. Containers on the Wildstone
        # network reach each other by container-name DNS, so a value
        # naming another container is just its name — "__NAME__-db", not
        # "container/__NAME__-db". Lint refuses the prefixed form: it is
        # only resolved in Route.upstream, and six shipped occurrences of
        # it were six containers that could never have started.
      mounts:
        - { source: __NAME__-data, target: /data }
      restart: unless-stopped
  # ---- How the household reaches it.
  - |
    apiVersion: wildstone.dev/v1
    kind: Route
    metadata: { name: __NAME__, node: "{{ .Params.node }}" }
    spec:
      host: "{{ .Params.subdomain }}.{{ .Params.public_domain }}"
      # container/<name>:<port> IS the right form here — Route.upstream
      # is the one place the reconciler resolves it.
      # TODO: the port the app listens on inside its container.
      upstream: "container/__NAME__:8080"
      # app_oidc: the app runs its own login against the identity
      # provider, so the proxy passes through. This is NOT auth: none —
      # the surface is authenticated, and the distinction is what the
      # policy engine reads. Use authelia_oidc instead when the app has
      # no login of its own and the proxy must enforce one.
      auth: app_oidc
      classification: family_public
  # ---- Registering with the identity provider. Keystone merges this
  # into the provider's client list; nothing here edits Authelia by hand.
  - |
    apiVersion: wildstone.dev/v1
    kind: OidcClient
    metadata: { name: __NAME__, node: "{{ .Params.node }}" }
    spec:
      clientId: __NAME__
      clientName: "__TITLE__"
      # TODO: the app's real callback path — this is per-app and getting
      # it wrong produces a login that fails at the last step.
      redirectUris:
        - "https://{{ .Params.subdomain }}.{{ .Params.public_domain }}/oidc/callback"
      authPolicy: two_factor
      secretRef: "{{ .Resolved.OidcSecretRef }}"
# TODO: does this capability offer the household an ACTION — something a
# rule or the Butler could ask it to do? Declare it here and the domestic
# gateway will dispatch it; leave it out and nothing can. Risk tiers are
# read, low, medium, high, domestic.
#
# agentTools:
#   - { name: TODO, risk: domestic }
`

const sidecarTemplate = `# __TITLE__ — TODO: replace this paragraph with what this capability IS
# and which service it adapts.
#
# SHAPE: sidecar. The service itself runs somewhere Wildstone does not
# manage, and cannot call a webhook on its own — so this package runs a
# small adapter container that watches it and posts household events to
# the intake on its behalf.
#
# CHECK THE CHEAPER SHAPE FIRST. If the service can call a URL itself —
# most can, under some name like "webhook", "notification target" or
# "REST command" — then ` + "`wild pkg new <name> -shape config`" + ` needs no
# adapter, no image, and no code. This shape is for when it genuinely
# cannot.
#
# DELIBERATELY NOT a package that runs the service. A household that has
# this service already has it, configured the way that household needs;
# adopting it is a capability package of its own, not a side effect of
# wiring up events.
#
# TODO: pin a real image digest — the one below is all zeroes so that
# this file lints as written, and no registry will ever serve it.
apiVersion: wildstone.dev/v1
kind: Package
metadata:
  name: __NAME__
  version: 0.1.0
  capability: __CAPABILITY__
  profile: __PROFILE__
# TODO: declare only the architectures for which this adapter image is
# published. Omit the field only when compatibility is intentionally
# unchanged; declared values are linux/amd64 and linux/arm64, and a
# mismatch must fail closed before mutation.
# platforms: [linux/amd64, linux/arm64]
params:
  - { name: node, required: true, source: node, example: "wildstone", description: "Which machine runs the adapter." }
  # As the ADAPTER can reach it. The Keystone API binds loopback and the
  # tailnet by default, so this is usually a tailnet address rather than
  # 127.0.0.1 — the adapter is a container, and loopback is its own.
  - { name: intake_url, required: true, example: "https://wildstone.tailnet.ts.net/api/v1/household/events", description: "Where the adapter posts household events." }
  # TODO: whatever the adapter needs to reach the service it watches.
  - { name: service_url, required: true, example: "http://192.168.1.10:8080", description: "The service this adapter watches." }
# The household events this capability emits (ADR-W024 A10). Declaring
# them is what earns the capability its publisher credential: the engine
# creates a provided secret named <capability>-publisher, and the intake
# refuses anything without it.
#
# Before installing, mint the token and place it on the node:
#
#     wild household publisher __CAPABILITY__ -node <node>
#
# The reconciler refuses to materialise this container until that secret
# exists, and names it if you forget — which is the right way round. An
# adapter carrying the literal text "secret/..." where a token belongs
# posts 401s forever with nothing in any log to explain it.
publishes:
  shape: sidecar
  events:
    # TODO: the events this service actually produces. A type is
    # namespace.name in lowercase [a-z0-9_]. Subjects are member, device,
    # household or none — "none" is the honest answer for a
    # household-wide fact, and a wrong subject is worse than no subject.
    - type: __EVENTNS__.something_happened
      subject: none
      description: TODO — what this event MEANS, for whoever writes a rule on it.
      fields:
        # Keep a payload to the few fields a handler needs. It is not the
        # record. Types are string, number, bool, timestamp.
        - { name: detail, type: string, required: true }
templates:
  - |
    apiVersion: wildstone.dev/v1
    kind: Container
    metadata: { name: __NAME__-adapter, node: "{{ .Params.node }}" }
    spec:
      image: "ghcr.io/example/__NAME__-adapter@__DIGEST__"
      env:
        WILDSTONE_INTAKE_URL: "{{ .Params.intake_url }}"
        # The publisher credential, as a reference. The node resolves it
        # when the container is materialised; Wildstone never holds the
        # value. The engine creates the Secret because this package
        # declares ` + "`publishes`" + ` — you do not declare it yourself.
        WILDSTONE_PUBLISHER_TOKEN: "{{ .Resolved.PublisherSecretRef }}"
        # TODO: how the adapter reaches the service it watches. If the
        # service ALSO runs as a Wildstone container, use its plain name
        # here — container-name DNS resolves it — never "container/<name>".
        SERVICE_URL: "{{ .Params.service_url }}"
      restart: unless-stopped
`

const configTemplate = `# __TITLE__ — TODO: replace this paragraph with what this capability IS
# and which service it configures.
#
# SHAPE: config, and this is the cheapest integration there is. The
# service runs somewhere Wildstone does not manage — bare metal, a VM, a
# Pi with a radio hanging off it — and it already knows how to call a
# webhook. So this package runs NOTHING. It renders one managed file that
# tells the service where to POST and what token to present, and the
# operator points the service at that file.
#
# DELIBERATELY NOT a package that runs the service. A household that has
# this service already has it; adopting it is a capability package of its
# own, not a side effect of wiring up events.
#
# INSTALLING (two steps, in this order):
#
#   1. Mint the publisher credential and place it on the node:
#
#          wild household publisher __CAPABILITY__ -node <node>
#
#      The reconciler refuses to write the file until that secret exists,
#      and names it if you forget. A config carrying the literal text
#      "secret/..." where a token belongs is a service that posts 401s
#      forever with nothing in any log to explain it.
#
#   2. Enable the capability, then point the service at the rendered
#      file. TODO: say exactly how, for THIS service — the line the
#      operator pastes, and where they paste it.
apiVersion: wildstone.dev/v1
kind: Package
metadata:
  name: __NAME__
  version: 0.1.0
  capability: __CAPABILITY__
  profile: __PROFILE__
params:
  - { name: node, required: true, source: node, example: "wildstone", description: "Which machine holds the rendered file." }
  # As the SERVICE can reach it — the Keystone API binds loopback and the
  # tailnet by default, so this is usually a tailnet address or a Route
  # in front of the API, not 127.0.0.1.
  - { name: intake_url, required: true, example: "https://wildstone.tailnet.ts.net/api/v1/household/events", description: "Where this service posts household events." }
# The household events this capability emits (ADR-W024 A10). Declaring
# them earns the capability its publisher credential: the engine creates a
# provided secret named <capability>-publisher, referenced below.
publishes:
  shape: config
  events:
    # TODO: the events this service actually produces. A type is
    # namespace.name in lowercase [a-z0-9_]. Subjects are member, device,
    # household or none.
    #
    # Say which real identifier the subject carries, and say it in the
    # header comment above. "The subject is this service's own object id"
    # is a fine answer; leaving a rule author to guess is not.
    - type: __EVENTNS__.something_happened
      subject: none
      description: TODO — what this event MEANS, for whoever writes a rule on it.
      fields:
        - { name: detail, type: string, required: true }
# The rendered file is read by something Wildstone does not run, so the
# standing invariant that every rendered config is mounted into a
# container of its own package cannot hold here. Saying so is the point:
# undeclared, this file is indistinguishable from the Authelia bug that
# invariant exists to catch — a correct config on disk that the
# application never reads.
#
# A stale declaration fails just as loudly as a missing one, so keep the
# filename in step with the ConfigFile below.
externalConfig:
  - filename: wildstone.yaml
    consumer: TODO — the service that reads this file, e.g. Home Assistant.
    reason: >-
      TODO — why that service is outside Wildstone, and how this file
      reaches it. Write it for a reviewer who has never seen this
      service: the point is that a human weighed the hand-off, not that
      the check was silenced.
templates:
  - |
    apiVersion: wildstone.dev/v1
    kind: ConfigFile
    metadata: { name: __NAME__-config, node: "{{ .Params.node }}" }
    spec:
      filename: wildstone.yaml
      content: |
        # Managed by Wildstone — do not edit. Changes are reverted on the
        # next reconcile tick.
        #
        # TODO: replace everything below with the configuration THIS
        # service actually wants. What it must end up saying is only:
        #   - POST to the intake URL,
        #   - with this Authorization header,
        #   - a JSON body carrying eventId, occurredAt, type, subject and
        #     a payload of the fields declared above.
        #
        # An eventId derived from the event itself, not from the clock,
        # is what makes a retry safe: the intake recognises the delivery
        # and records it once.
        #
        # IF THIS SERVICE HAS ITS OWN TEMPLATE SYNTAX and it also spells
        # an action {{` + "`" + `{{ ... }}` + "`" + `}}, wrap each of its expressions in a Go raw
        # string so it survives expansion literally. packages/home-automation
        # does this for Home Assistant's Jinja and is the worked example.
        url: "{{ .Params.intake_url }}"
        method: POST
        headers:
          # Resolved on the node when this file is written; Wildstone
          # itself never holds the value.
          Authorization: "Bearer {{ .Resolved.PublisherSecretRef }}"
`
