import { useEffect, useRef, useState, type InputHTMLAttributes } from "react";
import { Eye, EyeSlash } from "@phosphor-icons/react";

// A secret's input with a Show/Hide toggle, so a person can check what
// they TYPED before it is stored.
//
// It shows only what is in the field on this page. Nothing here reads a
// stored value back — the store is write-only (ADR-W012) and stays so —
// and nothing extra leaves the page: toggling changes the input's type
// and nothing else.
//
// It conceals itself again, because a value left on screen is a value on
// screen: after 30 seconds, when the window loses focus or the page is
// hidden, and when the field is emptied (which is what a successful save
// does). Spell-checking, autocorrect and autocapitalise stay off, since a
// visible field is one some browsers send to a spelling service.
const REVEAL_FOR_MS = 30_000;

type Props = Omit<InputHTMLAttributes<HTMLInputElement>, "type"> & {
  value: string;
};

export function SecretInput({ value, style, ...rest }: Props) {
  const [shown, setShown] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);

  const conceal = () => {
    if (timer.current) clearTimeout(timer.current);
    timer.current = null;
    setShown(false);
  };

  useEffect(() => {
    if (!value) conceal();
  }, [value]);

  useEffect(() => {
    const onHide = () => conceal();
    const onVisibility = () => document.hidden && conceal();
    window.addEventListener("blur", onHide);
    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      window.removeEventListener("blur", onHide);
      document.removeEventListener("visibilitychange", onVisibility);
      if (timer.current) clearTimeout(timer.current);
    };
  }, []);

  const toggle = () => {
    if (shown) return conceal();
    setShown(true);
    if (timer.current) clearTimeout(timer.current);
    timer.current = setTimeout(conceal, REVEAL_FOR_MS);
  };

  const what = rest["aria-label"] ? String(rest["aria-label"]) : "the value";
  return (
    <span style={{ display: "inline-flex", alignItems: "center", gap: "var(--space-1)", ...style }}>
      <input
        {...rest}
        type={shown ? "text" : "password"}
        value={value}
        spellCheck={false}
        autoCorrect="off"
        autoCapitalize="off"
      />
      <button
        type="button"
        className="btn btn-ghost"
        onClick={toggle}
        aria-pressed={shown}
        aria-label={shown ? `Hide ${what}` : `Show ${what}`}
        title={shown ? "Hide" : "Show what you typed"}
      >
        {shown ? <EyeSlash size={16} /> : <Eye size={16} />}
      </button>
    </span>
  );
}
