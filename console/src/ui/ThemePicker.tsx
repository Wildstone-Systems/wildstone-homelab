import { useEffect, useRef, useState } from "react";
import { Image as ImageIcon, Palette } from "@phosphor-icons/react";
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

  const selected = state.themes.find((t) => t.name === state.theme);

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
            picture={!!t.background}
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

      {canImport && state.theme !== "" && selected && (
        <PictureRow
          theme={selected}
          onDone={() => void load()}
          onError={(m) => setError(m)}
        />
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
  picture,
  on,
  onPick,
}: {
  name: string;
  label: string;
  note?: string;
  // The theme brings a wallpaper. Marked rather than drawn: the
  // Console does not wear one, and fetching a megabyte of art to
  // decorate a 104px tile on a screen that will never show it is not a
  // trade worth making. The mark says where it does appear.
  picture?: boolean;
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
      <span className="theme-name">
        {label}
        {picture && (
          <ImageIcon
            size={12}
            weight="fill"
            className="theme-haspic"
            aria-label="brings a picture for the dashboard"
          />
        )}
      </span>
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

// The picture behind a theme, for whoever administers the deployment.
//
// PER THEME, NOT PER PERSON. The wallpaper belongs to the skin: adding
// one to Gruvbox gives it to everybody in the house who reads on
// Gruvbox, and to nobody who does not. It attaches by name, so a theme
// that ships in the binary takes one as readily as a pasted one.
//
// It is uploaded rather than fetched, for the reason the palette is
// pasted rather than cloned (ADR-W036 A2): the control plane runs the
// house, and it should not be reaching out to somebody's repository for
// a megabyte of art. The wallpapers are in the theme's `backgrounds/`
// directory; the household downloads the one it likes and puts it here.
function PictureRow({
  theme,
  onDone,
  onError,
}: {
  theme: ThemeSummary;
  onDone: () => void;
  onError: (message: string) => void;
}) {
  const [busy, setBusy] = useState(false);
  const input = useRef<HTMLInputElement>(null);

  const upload = async (file: File) => {
    setBusy(true);
    try {
      await api.setThemeBackground(theme.name, file);
      onDone();
    } catch (e) {
      onError((e as Error).message);
    } finally {
      setBusy(false);
      // So choosing the same file again still fires a change event.
      if (input.current) input.current.value = "";
    }
  };

  const clear = async () => {
    setBusy(true);
    try {
      await api.clearThemeBackground(theme.name);
      onDone();
    } catch (e) {
      onError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="card" style={{ marginTop: "var(--space-6)" }}>
      <div className="row" style={{ alignItems: "baseline" }}>
        <strong>{theme.label}</strong>
        <span className="muted">
          {theme.background
            ? "brings a picture — it is the background of the household dashboard"
            : "no picture; the dashboard uses the theme's flat ground"}
        </span>
      </div>
      <input
        ref={input}
        type="file"
        accept="image/*"
        hidden
        onChange={(e) => {
          const file = e.target.files?.[0];
          if (file) void upload(file);
        }}
      />
      <div className="row">
        <button
          type="button"
          className="btn btn-secondary"
          disabled={busy}
          onClick={() => input.current?.click()}
        >
          {busy ? "Working…" : theme.background ? "Replace picture" : "Add a picture"}
        </button>
        {theme.background && (
          <button type="button" className="btn btn-ghost" disabled={busy} onClick={() => void clear()}>
            Remove picture
          </button>
        )}
      </div>
    </div>
  );
}
