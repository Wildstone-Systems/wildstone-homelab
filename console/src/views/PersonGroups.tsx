import { api, type PersonGroup } from "../api";
import { ErrorLine, useAction, useLoad } from "../lib";

// The household's one principal namespace (ADR-W033 design §4; slices 6
// and 8).
//
// NOT THE DIRECTORY GROUPS above it on this screen, and the difference
// is worth keeping visible: a directory group like `docs_users` is
// where a membership is projected TO, and these are what the household
// decided. Editing one believing you edited the other is the confusion
// this section is laid out to prevent.
//
// MEMBERSHIP GRANTS NOTHING BY ITSELF. Being in `Adults` is an identity
// primitive; what it reaches is decided per service, on the Services
// screen. The copy says so, because a list of groups with an Add button
// looks exactly like a permissions screen and is not one.
export function PersonGroups({ members }: { members: string[] }) {
  const groups = useLoad(() => api.personGroups(), []);
  if (groups.error) return <ErrorLine error={groups.error} />;
  return (
    <PersonGroupsView
      groups={groups.data ?? []}
      members={members}
      onChanged={() => groups.reload()}
    />
  );
}

// Separated from its fetch so the render check can put it in a state:
// the seeded three, a group with nobody in it, and a household group.
export function PersonGroupsView({
  groups,
  members,
  onChanged,
}: {
  groups: PersonGroup[];
  members: string[];
  onChanged: () => void;
}) {
  const action = useAction();
  const busy = action.state.phase === "busy";
  const failure = action.state.phase === "error" ? action.state.message ?? null : null;

  const toggle = (group: PersonGroup, member: string, inGroup: boolean) =>
    action.run(
      async () => {
        await api.setPersonGroup(group.id, member, inGroup);
        onChanged();
      },
      inGroup ? `${member} added to ${group.name}` : `${member} taken out of ${group.name}`,
    );

  return (
    <>
      <div className="sect" style={{ marginTop: "var(--space-8)" }}>
        <span className="eyebrow">Groups</span>
        <span className="fade" />
      </div>
      <div className="sub">
        Who the household counts as what. Being in a group grants nothing on its own — what each
        group may reach is decided per service, under Services. The same groups are used for what
        the assistant may remember and for secrets, so a person is in one list rather than three.
      </div>
      {failure && <ErrorLine error={failure} />}

      <table>
        <thead>
          <tr>
            <th>group</th>
            <th>who is in it</th>
          </tr>
        </thead>
        <tbody>
          {groups.map((g) => (
            <tr key={g.id}>
              <td>
                {g.name}
                {/* The seed key, not the name, is what rules reference.
                    Shown because renaming `Adults` is allowed and does
                    not re-point anything — which is reassuring only if
                    somebody can see that it is true. */}
                {g.seedKey && <div className="timeline-src">{`role: ${g.seedKey}`}</div>}
              </td>
              <td>
                <div className="chips">
                  {members.map((m) => {
                    const inGroup = g.members.includes(m);
                    return (
                      <button
                        key={m}
                        className={inGroup ? "chip chip-btn chip-on" : "chip chip-btn"}
                        disabled={busy}
                        aria-pressed={inGroup}
                        onClick={() => toggle(g, m, !inGroup)}
                      >
                        {m}
                      </button>
                    );
                  })}
                  {members.length === 0 && (
                    <span className="muted">No active people to put in a group yet.</span>
                  )}
                </div>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </>
  );
}
