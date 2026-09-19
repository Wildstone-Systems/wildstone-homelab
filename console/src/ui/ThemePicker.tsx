import { useEffect, useState } from "react";
import { Palette } from "@phosphor-icons/react";
import { api, Appearance, ThemeSummary } from "../api";
import { adopt } from "../theme";
import { Drawer } from "./Drawer";
import { ErrorLine } from "../lib";

// Choosing what you read on.
//
// IN THE RAIL RATHER THAN AS A SCREEN. Appearance is a preference, not
// a section of the product: giving it a rail item would put it beside
// Plans and Nodes, which are the things an operator came here to do.
// So it sits in the footer next to the avatar — the two controls that
// are about the person rather than about the house.
//
// APPLIED BEFORE IT IS SAVED. Pressing a swatch paints the Console
// immediately and then records the choice. Waiting for the round trip
// would make every theme feel like it might not have worked, and the
// only thing a failed save costs is the next load, which is exactly
// what the error line says.

export function ThemePicker({ canImport }: { canImport: boolean }) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <button
        type="button"
        className="ri"
        title="Appearance"
        aria-label="Appearance"
        onClick={() => setOpen(true)}
      >
        <Palette size={19} />
      </button>
      <Drawer open={open} onClose={() => setOpen(false)} title="Appearance">
        <AppearancePanel canImport={canImport} />
      </Drawer>
    </>
  );
}

function AppearancePanel({ canImport }: { canImport: boolean }) {
  const [state, setState] = useState<Appearance | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [adding, setAdding] = useState(false);

  const load = () =>
    api
      .appearance()
      .then((a) => {
        setState(a);
        // The server's answer is the truth; the cached block that
        // painted the first frame was a guess.
        adopt(a);
      })
      .catch((e: Error) => setError(e.message));

  useEffect(() => {
    void load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const choose = async (name: string) => {
    if (!state) return;
    const previous = state;
    // Paint first. The theme list does not change, so only the
    // selection and the block move.
    const picked = state.themes.find((t) => t.name === name);
    setState({ ...state, theme: name });
    setError(null);
    try {
      adopt(await api.setTheme(name));
    } catch (e) {
      // Put the screen back the way it was rather than leaving somebody
      // looking at a theme the server did not record.
      setState(previous);
      adopt(previous);
      setError(
        `${(e as Error).message} — ${picked ? picked.label : "that theme"} is showing, but it was not saved.`,
      );
    }
  };

  if (error && !state) return <ErrorLine error={error} />;
  if (!state) return <div className="muted">Reading…</div>;

  return (
    <>
      <div className="drawer-head">
        <Palette size={22} />
        <h3>Appearance</h3>
      </div>
      <p className="muted" style={{ marginTop: 0 }}>
        Your theme, on every Wildstone screen — this Console and the household
        dashboard. It is yours: nobody else in the house sees it.
      </p>

      {error && <ErrorLine error={error} />}

      <div className="theme-grid">
        <ThemeTile
          name=""
          label="Wildstone"
          note="the household's own colour"
          swatch={null}
          on={state.theme === ""}
          onPick={() => void choose("")}
        />
        {state.themes.map((t) => (
          <ThemeTile
            key={t.name}
            name={t.name}
            label={t.label}
            note={t.imported ? "added by this household" : undefined}
            swatch={t.swatch}
            on={state.theme === t.name}
            onPick={() => void choose(t.name)}
          />
        ))}
      </div>

      {state.theme !== "" && !state.themes.some((t) => t.name === state.theme) && (
        <p className="note">
          You are on <strong>{state.theme}</strong>, which this household no
          longer has. Wildstone's own look is showing until it comes back.
        </p>
      )}

      {canImport && (
        <div style={{ marginTop: "var(--space-6)" }}>
          {adding ? (
            <ImportForm
              onDone={() => {
                setAdding(false);
                void load();
              }}
              onCancel={() => setAdding(false)}
            />
          ) : (
            <button type="button" className="btn btn-secondary" onClick={() => setAdding(true)}>
              Add a theme
            </button>
          )}
        </div>
      )}
    </>
  );
}

function ThemeTile({
  name,
  label,
  note,
  swatch,
  on,
  onPick,
}: {
  name: string;
  label: string;
  note?: string;
  // null is Wildstone's own look, which is not a palette and is drawn
  // from the tokens currently in force rather than from a swatch.
  swatch: ThemeSummary["swatch"] | null;
  on: boolean;
  onPick: () => void;
}) {
  const colours = swatch ?? {
    background: "var(--color-bg)",
    surface: "var(--color-surface)",
    text: "var(--color-text)",
    accent: "var(--color-accent)",
  };
  return (
    <button
      type="button"
      className={on ? "theme-tile on" : "theme-tile"}
      onClick={onPick}
      aria-pressed={on}
      data-theme-name={name}
    >
      <span className="theme-swatch" style={{ background: colours.background }} aria-hidden>
        <span className="theme-swatch-card" style={{ background: colours.surface }}>
          <span className="theme-swatch-line" style={{ background: colours.text }} />
          <span className="theme-swatch-line short" style={{ background: colours.accent }} />
        </span>
      </span>
      <span className="theme-name">{label}</span>
      {note && <span className="muted">{note}</span>}
    </button>
  );
}

// Adding a theme: paste the colors.toml.
//
// A PASTE RATHER THAN A URL, deliberately. Omarchy installs a theme by
// cloning a repository, and doing that here would mean the control
// plane fetching and unpacking somebody's git repository on a machine
// that runs the house. The portable part of an Omarchy theme is one
// small text file of colours; everything else in one configures
// programs Wildstone does not run. So the household copies that file
// in, and nothing is fetched and nothing is executed.
function ImportForm({ onDone, onCancel }: { onDone: () => void; onCancel: () => void }) {
  const [name, setName] = useState("");
  const [colors, setColors] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const submit = async () => {
    setBusy(true);
    setError(null);
    try {
      await api.importTheme(name.trim(), colors);
      onDone();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="card">
      <div className="field">
        <label htmlFor="theme-name">
          Name
          <span className="muted">
            lower-case letters, digits and hyphens — the theme's own folder name
          </span>
        </label>
        <input
          id="theme-name"
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="tokyo-night"
          autoComplete="off"
        />
      </div>
      <div className="field">
        <label htmlFor="theme-colors">
          colors.toml
          <span className="muted">
            the whole file, from the theme's repository — omarchythemes.com lists them
          </span>
        </label>
        <textarea
          id="theme-colors"
          rows={10}
          className="mono"
          value={colors}
          onChange={(e) => setColors(e.target.value)}
          placeholder={'mode = "dark"\nbackground = "#1a1b26"\nforeground = "#a9b1d6"\n…'}
        />
      </div>
      {error && <ErrorLine error={error} />}
      <div className="row">
        <button
          type="button"
          className="btn btn-primary"
          disabled={busy || name.trim() === "" || colors.trim() === ""}
          onClick={() => void submit()}
        >
          {busy ? "Adding…" : "Add"}
        </button>
        <button type="button" className="btn btn-ghost" onClick={onCancel} disabled={busy}>
          Cancel
        </button>
      </div>
    </div>
  );
}
