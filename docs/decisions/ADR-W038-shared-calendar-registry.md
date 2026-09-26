# ADR-W038: Declared shared calendars

- **Status:** accepted; implemented (`keystone/sharedcalendar`, `api/calendar_registry.go`, migration 0080)
- **Governed by:** ADR-W033 where they overlap.

## Declaration and compatibility

The `calendar-radicale` package's `calendars` parameter declares a bounded JSON
registry. The approved package Plan renders it into ConfigFile/calendar-registry
(`calendars.json`). It contains identifiers, display names, colors, read/write
people and directory groups, one default, and a booking target. Credentials,
host paths and URLs are not declaration fields. A registry change is a package
Plan change: an Agent may propose it, and only an Owner approves the exact
version (ADR-W033 E2). No direct-write configuration endpoint bypasses that.

The registry declares 1–32 calendars and must retain `household`. Its collection
is `-calendar-url`, which preserves stored event links and mobile profiles.
Additional collection paths are derived from stable IDs at that configured
origin. An installation without the registry gets a single `household` calendar
(readable and writable by `calendar_users`) that is both default and booking
target. An Owner may change the default and booking target independently;
changing either does not change existing bookings' collection identifiers.
Photos, files and personal CalDAV collections are outside this registry.

The adapter configuration supplies the credential and trusted origin; a
per-collection factory serves the portal and bookings. Neither the browser nor
a Butler chooses an origin or sends a filesystem path.

## Access and visibility

The registry is a resource-specific rule (ADR-W033 A0 layer 2): it can narrow
what ADR-W033 A10 lets a reader see on the household agenda, never widen it.
Read/write groups belong to the one household group namespace (ADR-W033 B8.1);
direct member grants name a Person (ADR-W033 A2). Write implies read.
The portal uses the identity
proxy's proven member and group list. Butler calls resolve declared directory
membership and current Wildstone entitlement projections for their acting
person; they cannot supply groups. Groups maintained only outside Wildstone's
declarations/entitlements are not inferred by a Butler from a user's claim.
_Code not yet conformant: members are keyed by directory username and groups
are only checked for syntax (`^[a-z][a-z0-9_-]{0,47}$`), not existence; a
rename detaches the grant._

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

The Butler calendar tools (`get_agenda`, `search_events`, `check_availability`,
`create_event`, `update_event`, `delete_event`, `how_to_add`) accept an optional
`calendar` ID. Results identify the selected calendar. An omitted ID
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

This follows the pinned upstream
[3.8.0 discovery](https://github.com/Kozea/Radicale/blob/v3.8.0/radicale/storage/multifilesystem/discover.py)
and [rights](https://github.com/Kozea/Radicale/blob/v3.8.0/radicale/rights/from_file.py)
interfaces, exercised by an integration test against real Radicale 3.8.0.
Named client apps (Apple Calendar, Thunderbird/DAVx5) are not claimed tested.

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
