# Matrix homeserver

Wildstone can run a private Matrix homeserver as the optional
`matrix-synapse` package. It is separate from `chat-matrix`: the homeserver
stores and serves rooms, while each instanced `chat-matrix` package connects
one Agent identity to any Matrix homeserver.

## What the first release installs

- Synapse with a dedicated PostgreSQL database.
- A private-ingress route at the stable `matrix.<public domain>` origin.
- Wildstone OIDC sign-in with the existing household identity and two-factor
  policy. Password registration and guest access are off.
- Client and media APIs only. Incoming federation is not mounted and outbound
  federation has an empty domain allowlist.
- Daily encrypted backups of Synapse data, signing material, uploaded media,
  and the PostgreSQL volume.

The Synapse image carries its own `/health` check. Wildstone also observes the
route's `matrix-synapse:8008` upstream, so the service appears in normal
capability and dashboard status.

## Install

The interactive first-install flow offers **Matrix — private household
messaging**. It is off by default. Matrix needs the same private ingress and
identity packages as the rest of the household web applications, so select a
tailnet ingress during that install.

For an existing installation, enable `matrix-synapse` through the Console's
package form or with the capability API. The normal parameters are:

- `node`: the node that will hold the database and media.
- `public_domain`: the deployment's service domain.
- `server_name`: optional permanent domain used in Matrix IDs. If omitted it
  is `matrix.<public_domain>`.
- `timezone`: UTC by default.

Choose `server_name` carefully. It becomes the domain in every Matrix user ID
and room identity and cannot be changed safely after the homeserver contains
data.

## Try it from a native client

1. Wait until `matrix-postgres`, `matrix-config-generate`, and
   `matrix-synapse` have converged.
2. In Element or another Matrix client that supports SSO, choose a custom
   homeserver and enter the Matrix route shown by Wildstone. Tailnet installs
   include the assigned HTTPS port in that origin.
3. Choose **Continue with Wildstone** and sign in with a household account.
4. Create an encrypted room, sign in from a second client/account, and exchange
   messages.

Synapse creates the Matrix account on first successful OIDC login. There is no
separate Matrix password to maintain.

## Connect an Agent

Install an instance of `chat-matrix` for the Agent and use this homeserver's
route as `homeserver_url`. The adapter still requires an access token for a
dedicated Matrix account. Create/sign in to that service identity through the
normal Wildstone identity flow, obtain an access token with a Matrix client,
and provide it in the connection form. Keep that client session active: logging
it out revokes the token the adapter uses. Invite the bot to a **new room with
encryption disabled**; the adapter accepts Matrix room invitations
automatically, while Keystone's normal invite-key and account-binding checks
still decide who may speak to the Agent. The adapter boundary remains the same
whether the homeserver is local or external.

`chat-matrix` does not implement Matrix end-to-end encryption. It ignores
encrypted event contents and emits a clear adapter diagnostic naming the room,
instead of silently appearing to work. Encryption cannot be disabled after it
is enabled on a room, so create a separate unencrypted room for Agent chat.

## Security and current limits

Matrix carries messages; it is not Wildstone's secrets store. Agent actions
still pass through Keystone authorization, and credentials remain node
secrets. Avoid sending reusable credentials into room history even when a room
uses end-to-end encryption.

This first slice deliberately does not include:

- federation or public Matrix ingress;
- an embedded Element Web client;
- TURN for voice/video calls;
- automatic bot-account or access-token provisioning;
- database-consistent pre/post backup hooks (the current package backup
  contract snapshots declared volumes, as it does for other PostgreSQL-backed
  packages).

Public access and federation need a separate profile and explicit review of
DNS delegation, TLS, registration, abuse controls, TURN, and restore testing.
