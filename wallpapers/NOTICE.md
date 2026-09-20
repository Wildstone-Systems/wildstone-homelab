# Vendored theme wallpapers

One picture per shipped theme, so a theme arrives looking the way its
authors drew it. Each is copied verbatim from the `backgrounds/`
directory of the same theme in
[Omarchy](https://github.com/omacom/omarchy) (branch `quattro`), which
is where `../keystone/theme/themes/` took the palettes from.

Omarchy is MIT licensed, Copyright (c) David Heinemeier Hansson. The
wallpapers themselves originate with their own authors — Omarchy's
credits are the record of where each came from, and a household that
would rather not ship somebody else's art can delete this directory
and pass no `-wallpaper-dir`: the themes then arrive with palettes
only, exactly as they did before.

## Not in the binary

These are **not** embedded. `keystone/runtimedb/migrations/0061` declined
to vendor wallpapers because one per theme would add fifteen megabytes
to a control-plane binary downloaded onto home servers, and that
objection stands. They travel instead as their own release artifact —
`wallpapers.tar.gz`, sha256-verified like every other — which the
installer unpacks into `/usr/share/wildstone/wallpapers` and Keystone
reads via `-wallpaper-dir`, exactly as it reads `-catalog-dir` and
`-console-dir`. The binary does not grow by a byte, and nothing is
fetched at run time.

## Which picture, and why that one

Omarchy ships several backgrounds per theme (9.5 MB for one each,
54.6 MB for all of them). The rule: the lowest-numbered file, skipping
`omarchy.webp` (the branded logo wall, not the theme's picture), and
taking the next one up when the first is over 1.5 MB — a dashboard
backdrop is not worth four megabytes.

| theme | ships as | taken from | size |
|---|---|---|---|
| `catppuccin` | `catppuccin.webp` | `themes/catppuccin/backgrounds/1-totoro.webp` | 0.05 MB |
| `catppuccin-latte` | `catppuccin-latte.webp` | `themes/catppuccin-latte/backgrounds/1-color-fade.webp` | 0.01 MB |
| `ethereal` | `ethereal.webp` | `themes/ethereal/backgrounds/1-cosmic.webp` | 0.33 MB |
| `everforest` | `everforest.webp` | `themes/everforest/backgrounds/1-tree-tops.webp` | 0.22 MB |
| `flexoki-light` | `flexoki-light.webp` | `themes/flexoki-light/backgrounds/1-orb.webp` | 0.10 MB |
| `gruvbox` | `gruvbox.webp` | `themes/gruvbox/backgrounds/2-flower-basket.webp` | 1.04 MB |
| `hackerman` | `hackerman.webp` | `themes/hackerman/backgrounds/2-geometric.webp` | 0.69 MB |
| `kanagawa` | `kanagawa.jpg` | `themes/kanagawa/backgrounds/1-kanagawa.jpg` | 1.62 MB |
| `last-horizon` | `last-horizon.webp` | `themes/last-horizon/backgrounds/1-eyes-wide.webp` | 0.20 MB |
| `lumon` | `lumon.webp` | `themes/lumon/backgrounds/01-united-in-severance.webp` | 0.04 MB |
| `lupine` | `lupine.webp` | `themes/lupine/backgrounds/01-cherry-blossom-bokeh.webp` | 0.62 MB |
| `matte-black` | `matte-black.webp` | `themes/matte-black/backgrounds/1-dark-waters.webp` | 0.33 MB |
| `miasma` | `miasma.webp` | `themes/miasma/backgrounds/01-nature-of-fear.webp` | 0.06 MB |
| `nord` | `nord.webp` | `themes/nord/backgrounds/1-city-view.webp` | 0.36 MB |
| `osaka-jade` | `osaka-jade.webp` | `themes/osaka-jade/backgrounds/1-glowing-city.webp` | 0.31 MB |
| `retro-82` | `retro-82.webp` | `themes/retro-82/backgrounds/1-in-the-groove.webp` | 0.85 MB |
| `ristretto` | `ristretto.webp` | `themes/ristretto/backgrounds/0-launch.webp` | 0.07 MB |
| `rose-pine` | `rose-pine.webp` | `themes/rose-pine/backgrounds/1-funky-shapes.webp` | 0.13 MB |
| `solitude` | `solitude.webp` | `themes/solitude/backgrounds/1-on-pole.webp` | 1.49 MB |
| `tokyo-night` | `tokyo-night.webp` | `themes/tokyo-night/backgrounds/0-winding-road.webp` | 0.65 MB |
| `vantablack` | `vantablack.webp` | `themes/vantablack/backgrounds/0-dot-hands.webp` | 0.24 MB |
| `white` | `white.webp` | `themes/white/backgrounds/1-white.webp` | 0.10 MB |

## Replacing one

A household's own upload always wins: Console → Appearance → a theme →
**Replace picture** writes a `theme_backgrounds` row, and that row
shadows the file here. Clearing it falls back to this picture rather
than to a flat ground.
