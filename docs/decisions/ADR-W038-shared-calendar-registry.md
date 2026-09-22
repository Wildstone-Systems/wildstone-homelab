# ADR-W038: Declared shared calendars

Status: proposed (#324)

## Declaration and compatibility

The `calendar-radicale` package's `calendars` parameter declares a bounded JSON
registry. The approved package Plan renders it into ConfigFile/calendar-registry
(`calendars.json`). It contains identifiers, display names, colors, read/write
people and directory groups, one default, and a booking target. Credentials,
host paths and URLs are not declaration fields. Owners use the existing
capability configuration and reviewed Plan flow; no new direct-write configuration
endpoint bypasses that process.

`household` is retained. Its collection is the existing `-calendar-url`, preserving
stored event links and mobile profiles. Additional collection paths are derived
from stable IDs at that configured origin. An installation without the registry
uses the original Household definition. The initial default and booking target
are Household; an Owner may change them independently. Changing either does not
change existing bookings' collection identifiers. Photos, files and personal
CalDAV collections are outside this registry.

The original adapter configuration supplies a credential and trusted origin;
a per-collection factory replaces its singular use by the portal and bookings.
The legacy fields remain as a compatibility seam. Neither the browser nor a
Butler chooses an origin or sends a filesystem path.

## Access and visibility

Read/write groups name existing directory groups. Direct member grants name
existing directory usernames. Write implies read. The portal uses the identity
proxy's proven member and group list. Butler calls resolve declared directory
membership and current Wildstone entitlement projections for their acting
person; they cannot supply groups. Groups maintained only outside Wildstone's
declarations/entitlements are not inferred by a Butler from a user's claim.

Radicale's LDAP authentication still requires `calendar_users`, independently
of the collection grant. Give authorized people that application entitlement
as well as any narrower per-calendar group. Declaring a group name in this
registry does not create or populate an LDAP group.

Every event mutation checks the selected calendar's write access. A cross-calendar
edit checks both calendars before touching either. Reads enumerate only readable
calendars. Visibility is a runtime preference keyed by member and calendar ID;
it cannot grant access or hide a calendar for another person. Dashboard and
agenda reads filter by the reader, even when the agenda is displaying somebody
else's day. Calendar identifiers, names and colors accompany displayed events.

Butler tools keep their existing names (`get_agenda`, `search_events`,
`check_availability`, `create_event`, `update_event`, `delete_event`) and accept
an optional `calendar` ID. Results identify the selected calendar. An omitted ID
selects the default; an explicitly selected calendar is not filtered by the
member's dashboard visibility. Destructive calls resolve defaults before their
confirmation key is constructed, and check current access again when executing.
Changing the default cannot redirect a previously approved deletion.

## CalDAV discovery and storage

Radicale 3.8.0's built-in group discovery maps one collection to each encoded
group name. That cannot represent two calendars readable by the same group
without extra discovery handling. A small storage subclass extends principal
listings with synthetic collection group names derived from the registry. It
does not grant permissions. A generated `from_file` rights configuration applies
the same read/write grants to every returned collection and subsequent request.
The service account receives access only to declared shared collections; existing
personal-principal rights stay bound to the authenticated username.

Household keeps `GROUPS/Y2FsZW5kYXJfdXNlcnM=`. Additional collections use
`GROUPS/<base64(wildstone_calendar_<id>)>`. The IDs cannot contain paths or URL
syntax. Startup provisions these collections under the storage write lock,
preserving events and unrelated collection metadata while updating names/colors.
The registry also participates in the Container spec so an approved change
restarts Radicale to adopt its discovery and rights configuration together.
Removed declarations withdraw access but do not erase stored calendar data.
The existing backup still includes the complete Radicale collections volume.

This follows the pinned upstream interfaces:
[3.8.0 discovery](https://github.com/Kozea/Radicale/blob/v3.8.0/radicale/storage/multifilesystem/discover.py),
[3.8.0 rights](https://github.com/Kozea/Radicale/blob/v3.8.0/radicale/rights/from_file.py).
The integration test runs real Radicale 3.8.0 with test-only authentication and
its real group, rights, storage and WSGI request handling. It exercises discovery,
read-only refusal, hidden collections, MOVE and personal-calendar isolation.
Real Apple Calendar and Thunderbird/DAVx5 checks remain device acceptance work;
a successful protocol test is not a claim that a particular client was tested.

## Event identity and failure behavior

All event hrefs, including ones returned by CalDAV, are confined to one selected
collection. HTTP redirects are refused so credentials cannot follow an event to
another origin. Native CalDAV MOVE with `Overwrite: F` preserves the complete
stored iCalendar object, including recurrence, attendees and alarms.

An edit that also changes calendar saves field edits before moving the event.
If MOVE fails, the response explicitly says that edits were saved and the move
failed. No copy/delete fallback discards unknown iCalendar properties. A successful
move updates live booking links; the booking sync loop is serialized with portal
moves and rebooking so it cannot mistake that transition for a cancellation.
A database failure after an upstream move is reported as a booking-link failure;
there is no distributed transaction between SQLite and Radicale.

Migration 0080 adds a `calendar_id` with default `household` to existing booking
rows and a per-member visibility table. Synchronization and replacement use each
booking's stored calendar, not today's default. An undeclared or unreachable
collection does not cancel a booking merely because it cannot be read.
