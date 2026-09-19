# console/

Web UI (TypeScript/React), served by Keystone, speaking the same public API
as `wild` and the Butler. Capability-centric; the plan review/approval screen
is the product's signature interaction.

The visual design is ADR-W027: a dark ground with a light variant that is a
token swap, one accent, and three status colours deliberately disjoint from
it. Nothing is fetched at runtime — the font is bundled, because this is the
screen you open when the house is broken.

That token swap is also the seam a whole theme fits into (ADR-W036). The
palette button in the rail footer sets the theme this person reads on, here
and on the household dashboard both; `src/theme.ts` applies the block
Keystone derives and deliberately computes no colour of its own, because the
contrast floors behind those values are the ones `contrast-check.mjs` holds
this sheet to and two implementations of a rule is one of them being wrong.

## Checks

Neither needs a browser or a running control plane, and both run in CI:

    npm run build           # tsc --noEmit && vite build
    npm run render-check    # every screen and shared component, in each state
    npm run contrast-check  # the token set, against both grounds, at 4.5:1

`npm run ui-check` drives the real screen in a real browser against a live
Keystone. It needs `npx playwright install --with-deps chromium`, `npm i -D
playwright`, and a control plane at the BASE in the script — so it is
deliberately not part of the build or of CI.
