# Issues 322 and 324: Calendar pilot

## Installer resume (#322)

The installer waits for Wardstone before agent-backed Plans and after restarting
Keystone for Calendar. A declared Calendar account with an existing
`/etc/wildstone/calendar.env` is retained and verified through CalDAV, without
setting its password again. An existing Owner's password is not replaced by a
saved installer answer. Directory admin password provisioning is now lazy:
a fully provisioned resume does not rotate that credential either.

On a disposable installer fixture, interrupt after Calendar verification, then
resume with the same Calendar environment and existing Owner. Delay Wardstone's
reconnection. Confirm completion, unchanged Calendar and Owner credentials, and
that the group Plan is proposed only after the node reports connected. An invalid
retained Calendar credential should fail verification without replacing it.

The executable regression test mocks node reconnection and runs the actual
installer resume functions; it does not claim a full VM install was performed.

## Multiple calendars (#324)

Apply the updated `calendar-radicale` 2.1.0 package through its normal reviewed
Plan flow. Replacing Keystone alone does not install the new Radicale discovery
module or declare additional calendars. Keep the current service account,
Calendar environment and existing `-calendar-url`/`-calendar-user` flags.

Example `calendars` package parameter (JSON):

```json
{
  "calendars": [
    {
      "id": "household", "name": "Household", "color": "#3b82f6",
      "default": true,
      "readGroups": ["calendar_users"], "writeGroups": ["calendar_users"]
    },
    {
      "id": "school", "name": "School", "color": "#f59e0b",
      "readGroups": ["calendar_users"], "writeGroups": ["adults"]
    }
  ],
  "bookingCalendar": "household"
}
```

Use an existing directory group in place of `adults` if that group is not declared
on the deployment, or a household group by its id (`grp_…`). `readMembers` and
`writeMembers` accept a person id (`psn_…`) or a handle, and follow that Person
through a rename.
Members still need the Calendar application's `calendar_users` access. Keep one
calendar with `default: true` and retain the `household` identifier.

1. With one calendar, verify existing events, booking links and mobile profiles.
2. With two calendars, verify colors and event order on Calendar, Home and Agenda.
3. Hide School, refresh, and reopen Home. Sign in as a second person and confirm
   their visibility is unchanged. Show School again.
4. Create, edit, move and delete disposable events as a writer. Test a read-only
   account against both the page and direct mutation endpoints. Unauthorized
   move targets must be refused before source event fields change.
5. Verify a calendar unavailable to a member is absent from discovery and cannot
   be read by supplying its ID or another collection's href.
6. Set a booking target, book an appointment, change the target, and verify the
   original booking still follows the original event. Move a linked event between
   calendars through Wildstone and verify its booking follows the new href.
7. Ask Butler for an agenda with and without `calendar: school`, then create and
   update there. Delete must name the calendar and ask for confirmation. Revoke
   write access before retrying an approved deletion; it must refuse.
8. On Apple Calendar and Thunderbird or DAVx5, use one existing account. Discover
   both shared calendars and verify names/colors, read-only behavior and edits.
   Confirm another person cannot see your personal calendars on either client.
   Record client/OS versions and results before marking device acceptance complete.
9. Personal calendars (ADR-W034 C2): open Calendar as a member and confirm their
   own calendars are listed with a person marker, and another member's are not.
   Create one with **New calendar**, add an event to it, and confirm it appears on
   that member's phone. Confirm it is absent from Home, Agenda and every Butler
   answer, that an event cannot move between it and a shared calendar, and that
   household activity does not name the event. Repeat **New calendar** for a
   member who has never signed in to the calendar server.
10. Renames (#372): rename a member, then confirm they keep their shared calendar
    grants, hidden calendars, Files **Mine** folder and commitments.
9. Restart Radicale and verify event data and unrelated collection properties are
   preserved. Change a display name/color through an approved package Plan and
   confirm the client sees the update.

## Automated checks

```sh
go test ./testing/e2e -run TestInstallerCalendarResumePreservesCredentials
go test -race ./keystone/sharedcalendar ./keystone/adapter/caldav ./keystone/api ./keystone/domestic ./keystone/runtimedb ./keystone/pkgtool
cd console
node scripts/calendar-check.mjs
```

For the real server protocol fixture, use an isolated Python environment with
`radicale==3.8.0` installed:

```sh
RADICALE_TEST_PYTHON=/path/to/venv/bin/python \
  go test -v ./keystone/pkgtool -run TestSharedCalendarRadicaleDiscovery
```

No live household calendar is used by these tests. The Python fixture uses
synthetic credentials and real Radicale group, rights and storage code.

This branch is based on main, independently of Filedrop PR #327. Do not replace
a Filedrop pilot binary with this branch's binary expecting unmerged Filedrop
features to remain present; use a combined integration build or merge the earlier
PR first. Back up the control-plane database before piloting migration 0080.
