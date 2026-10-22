# Vendored theme palettes

Each `*.toml` here is the `colors.toml` of a theme that ships with
[Omarchy](https://github.com/omacom/omarchy), copied verbatim apart from
hex values being lower-cased and keys this project has no use for
(compositor border colours) being dropped.

Omarchy is MIT licensed, Copyright (c) David Heinemeier Hansson. The
palettes themselves originate with their own authors — Catppuccin,
Everforest, Gruvbox, Kanagawa, Nord, Rosé Pine, Tokyo Night and the rest
are each that project's work, and Omarchy's own credits are the record
of where they came from.

Nothing in this directory is executed. A palette is read by
`ParsePalette`, every value is checked to be a `#rrggbb` colour, and the
result is only ever interpolated into CSS custom property values. The
rest of an Omarchy theme — `alacritty.toml`, `hyprland.conf`,
`neovim.lua`, `waybar.css` — configures programs Wildstone does not run
and is deliberately not vendored.

## Adding one

Drop the theme's `colors.toml` here as `<slug>.toml` and it is in the
next build: `builtin.go` walks the directory. A household can also paste
one in at run time without rebuilding — Console → Appearance → Add a
theme — which is how a theme from <https://omarchythemes.com> gets in
without this directory growing.
