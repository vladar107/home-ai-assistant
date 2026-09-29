# Vendored: Meteocons

- Source: Meteocons by Bas Milius, release `v2.0.0` of
  `github.com/basmilius/meteocons` (formerly `basmilius/weather-icons`), commit
  `fdd19749b9389f0b4e00d55584b083a7ab2deded`, downloaded from
  `https://codeload.github.com/basmilius/meteocons/tar.gz/refs/tags/v2.0.0`.
- Integrity: SHA-256 of the tarball
  `999aebb34a562a6f2ab7ced54166392d045f472270f7cb73f19f71e601b4f4c5`.
- Licence: MIT; `LICENSE` is the file from the tarball, unmodified.
- Vendored: 2026-09-28, for plan
  [0028](../../docs/plans/0028-day-card.md).

Only rasterised copies are committed, under `internal/daycard/assets/icons/`.
They come from the static "fill" set in `design/fill/export/`, which has no
animation, one file per icon: `clear-day`, `clear-night`,
`partly-cloudy-day`, `partly-cloudy-night`, `cloudy`, `fog-day`, `fog-night`,
`drizzle`, `rain`, `snow`, and `thunderstorms-rain`. Each was rendered at two
sizes with `rsvg-convert` 2.63.2 (Homebrew librsvg):

```sh
rsvg-convert -w 320 -h 320 -o internal/daycard/assets/icons/large/<name>.png design/fill/export/wi_<name>.svg
rsvg-convert -w 128 -h 128 -o internal/daycard/assets/icons/small/<name>.png design/fill/export/wi_<name>.svg
```

The renderer crops each icon to its visible pixels and scales it to its
layout box when it loads, so the PNGs are the unmodified conversions.

To refresh, download the new release, render the same files with the same
command, update this file, and review the sample cards from
`internal/daycard` again.
