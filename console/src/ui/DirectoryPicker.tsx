import { useEffect, useState } from "react";
import { api, type DirectoryListing } from "../api";

// A folder picker for params declared `source: directory`.
//
// It replaces a free-text box, and the reason is a specific failure: the
// redactor turned a package's own default path into "/[REDACTED]", the
// API served that back as the value to submit, and the operator accepted
// what they were shown — a blank box gives you nothing to check a value
// against. A control that browses the node can only offer folders that
// exist, which makes that class unrepresentable rather than merely fixed.
//
// It browses only the prefixes a bind mount may name. That is not a
// limitation of the picker, it is the truth about the system: the node
// refuses a bind mount anywhere else at apply time, so an operator whose
// media sits on /mnt learns it here, from a picker that does not offer
// it, rather than from a container that fails three ticks later.
export function DirectoryPicker({
  value,
  node,
  onChange,
}: {
  value: string;
  node: string;
  onChange: (v: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const [at, setAt] = useState<string>(value || "");
  const [listing, setListing] = useState<DirectoryListing | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    if (!open || !node) return;
    setLoading(true);
    setError(null);
    api
      .directories(node, at || undefined)
      .then((l) => setListing(l))
      .catch((e: Error) => setError(e.message))
      .finally(() => setLoading(false));
  }, [open, node, at]);

  // A node has to be chosen before there is a filesystem to browse.
  const browsable = node !== "";

  return (
    <div className="dirpicker">
      <div className="dirpicker-row">
        <input
          type="text"
          value={value}
          aria-label="folder path"
          onChange={(e) => onChange(e.target.value)}
        />
        <button
          type="button"
          className="btn btn-secondary"
          disabled={!browsable}
          onClick={() => {
            setAt(value || "");
            setOpen(!open);
          }}
        >
          {open ? "Close" : "Browse…"}
        </button>
      </div>
      {!browsable && <div className="muted">Choose a node first.</div>}

      {open && (
        <div className="dirpicker-panel">
          {error && (
            <div className="callout callout-bad">
              {error}
              <ErrorHint />
            </div>
          )}
          {loading && <div className="muted">Reading…</div>}

          {listing && !loading && (
            <>
              <div className="dirpicker-at">
                {listing.path ? (
                  <code>{listing.path}</code>
                ) : (
                  <span className="muted">
                    A service's data lives under one of these. Anywhere else is refused by the
                    node, so it is not offered here.
                  </span>
                )}
              </div>
              <ul className="dirpicker-list">
                {listing.parent && (
                  <li>
                    <button type="button" className="linklike" onClick={() => setAt(listing.parent!)}>
                      ../
                    </button>
                  </li>
                )}
                {!listing.path &&
                  listing.roots.map((r) => (
                    <li key={r}>
                      <button type="button" className="linklike" onClick={() => setAt(r)}>
                        {r}/
                      </button>
                    </li>
                  ))}
                {listing.directories.map((d) => (
                  <li key={d}>
                    <button
                      type="button"
                      className="linklike"
                      onClick={() => setAt(`${listing.path}/${d}`)}
                    >
                      {d}/
                    </button>
                  </li>
                ))}
                {listing.path && listing.directories.length === 0 && (
                  <li className="muted">
                    No folders here yet. Choosing this one is fine — it is created when the
                    service starts.
                  </li>
                )}
              </ul>
              {listing.path && (
                <button
                  type="button"
                  className="btn"
                  onClick={() => {
                    onChange(listing.path!);
                    setOpen(false);
                  }}
                >
                  Use this folder
                </button>
              )}
            </>
          )}
        </div>
      )}
    </div>
  );
}

function ErrorHint() {
  return (
    <div className="muted">
      The node answers only for the folders a service's data may live in. If this named
      somewhere else, that is why.
    </div>
  );
}
