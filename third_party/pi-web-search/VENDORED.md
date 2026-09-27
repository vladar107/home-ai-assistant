# Vendored: pi-web-search

- Source: npm package `pi-web-search` 1.6.0 (upstream
  `github.com/ttttmr/pi-web-search`, git head
  `eb72d5553a47fc0803887c5c7e673a32e04fca57`).
- Integrity: `sha512-UwUDER9JoA/EwoTeJQZKQ23FctfY4o6ygFrahsRKeHBYExiadr4SMzJbqh6uScYtZGWrF1BJQ0A7UUsIj1NuWg==`,
  checked against the downloaded tarball.
- Vendored: 2026-09-27, for plan
  [0026](../../docs/plans/0026-web-weather-and-pdf-tools.md).
- `src/`, `package.json`, and `README.md` are copied from the tarball
  unmodified. Neither the tarball nor the upstream repository ships a licence
  file; `package.json` and the README declare MIT, so `LICENSE` holds the
  standard MIT text with the author named in `package.json`.

The image copies this directory to `/usr/local/lib/home-agent/pi-web-search`.
The application loads `src/index.ts` with `--extension` only for principals
who have web access turned on; no `npm install` runs for it, since Pi's
extension loader resolves its Pi, `pi-ai`, `pi-tui`, and `typebox` imports.

To refresh, vendor the new version the same way, review the whole of `src/`
again, update this file, and re-run the live search check from plan 0026.
