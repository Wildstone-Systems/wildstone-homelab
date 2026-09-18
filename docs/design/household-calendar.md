# The household calendar widget, and why it is not built

*Status: the decision was taken — see [ADR-W034](../decisions/ADR-W034-the-household-keeps-its-own-calendar.md).
Way forward 3 below is what shipped: `packages/calendar-radicale` is a
household calendar that is Wildstone's own, so the control plane needs no
member's credential to offer one. Tracked as
[#230](https://github.com/chickenrat17/wildstone-homelab/issues/230).
2026-09-18.*

**What changed, and what did not.** The card is no longer blocked on a
household running `calendar-radicale`: Keystone holds a service
credential for the SHARED calendar, and the card now carries the
hand-off that matters — a configuration profile that puts that calendar
on a phone, which is the one thing a person cannot do from iOS's own
settings. Everything below still holds for a household whose calendars
come from **davis**, and that is still the case the card must answer
honestly. A member's PERSONAL calendar remains unreadable by this
surface on either server, for the reason this note gives: the credential
is theirs.

The implementation package specifies a Calendar widget on the household
homepage (§7) and a full Calendar page (§12.3): read-only, the next
three days on the card, a month grid and an agenda on the page, each
event showing whose calendar it is on. Everything else the package asks
for is built. This is not, and the reason is worth writing down because
it is not a thing that can be fixed by trying harder.

## What blocks it

The household's CalDAV server is **davis**, shipped inside the mail
capability's roundcube package, and it is configured to authenticate
against IMAP:

```yaml
AUTH_METHOD: IMAP
IMAP_AUTH_URL: "{{ .Params.imap_host }}:{{ .Params.imap_port }}"
IMAP_AUTH_USER_AUTOCREATE: "true"
```

On the reference deployment that resolves to `imap.gmail.com:993`, so the
credential a member's calendar needs is their Gmail password.

Two things were checked against the running container rather than
reasoned about, because the first draft of this note asserted them:

    unauth  /dav/                    401
    admin   /dav/                    401      <- the admin credential is refused
    admin   /dav/principals/admin/   404  "Principal with name admin not found"

`ADMIN_LOGIN`/`ADMIN_PASSWORD` govern davis's web admin panel, not
CalDAV. And the calendar is empty anyway — one principal (`mika`,
auto-created on first sign-in), one calendar instance, zero calendar
objects — so even a solved credential would render "Nothing in the next
three days" today.

A member's calendar is therefore readable only by presenting **that
member's own mail password**. Keystone does not hold it, and the whole
premise of this surface is that it never will: the portal holds no
storage credential, no model key, no tool credential and no member
secret. There is no service account with cross-user read — sabre/dav
does not have one — and there is no forwarded-header mode davis would
accept the way Keystone's own portal door does.

So the widget cannot be made to work by writing a CalDAV client. The
client is the easy part. The credential is the whole problem.

## What the page does instead

The card says what is true and offers the hand-off:

> Your calendars need your mail password, which this dashboard doesn't
> hold. **Open Calendars →**

Deliberately not "Can't reach the calendars right now", which is the
copy §7 specifies for a transient failure. Implying that a permanent
architectural fact will clear on its own is the kind of small lie the
rest of this surface exists to avoid, and a member would keep coming
back to a card that was never going to fill in.

The link is computed through the same helper the launcher's links use,
so under the tailnet posture it is the node's certified name and the
Route's port — a hand-off that cannot be clicked would be worse than no
hand-off.

## The ways forward, in the order I would consider them

**1. A per-member app password, held as a Secret.** The member creates
one in the mail service; an operator stores it as a Wildstone Secret
scoped to that member; Keystone binds to davis with it to render that
member's card. Honest, incremental, and it keeps "the portal holds no
credentials" true — the *control plane* holds it, which it already does
for every other secret in the house. The cost is a setup step per
person, and a new question about what happens to that secret at
offboarding (ADR-W030's offboarding path would need to revoke it).

**2. Move davis to LDAP auth.** The household already runs LLDAP and
every other service authenticates against it. davis supports
`AUTH_METHOD: LDAP`. That does not by itself give Keystone a read —
sabre/dav still authorises per principal — but it puts every account in
one directory, which is the precondition for a service principal with
delegated read, and it removes the oddity of calendar access being tied
to a mailbox password. This is a change to the mail capability, so it
belongs to that capability's owner, not to the dashboard.

**3. A household calendar that is Wildstone's own.** The events a
family dashboard actually wants — bin day, the boiler service, who is
out on Thursday — are household state, and the household event stream
already carries things of that shape. A Wildstone-owned calendar would
need no third-party credential at all, and davis would stay what it is
good at: syncing personal calendars to phones over CalDAV. The largest
option, and the only one that makes the widget work with no per-member
setup.

## What is NOT a way forward

**Reading davis's SQLite database directly.** The data is right there in
the `davis-data` volume, and Wardstone could be given an op to read a
file inside a container's volume. That op is the problem: a control
plane that can read any path in any container's storage is a far larger
capability than a calendar widget is worth, and the package's own rule
is that the household surface gets no privileged host access (#96). A
widget is not a reason to add a primitive that reads everybody's data.

**Asking the member for their mail password on the dashboard.** It would
work, and it is exactly what §3 forbids. A family surface that collects
a password is a family surface that has taught everyone in the house to
type their password into a page.
