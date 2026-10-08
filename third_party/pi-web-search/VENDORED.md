# Vendored: pi-web-search

- Source: npm package `pi-web-search` 1.6.0 (upstream
  `github.com/ttttmr/pi-web-search`, git head
  `eb72d5553a47fc0803887c5c7e673a32e04fca57`).
- Integrity: `sha512-UwUDER9JoA/EwoTeJQZKQ23FctfY4o6ygFrahsRKeHBYExiadr4SMzJbqh6uScYtZGWrF1BJQ0A7UUsIj1NuWg==`,
  checked against the downloaded tarball.
- Vendored: 2026-09-27, for plan
  [0026](../../docs/plans/0026-web-weather-and-pdf-tools.md).
- `package.json` and `README.md` are copied from the tarball unmodified.
  Neither the tarball nor the upstream repository ships a licence file;
  `package.json` and the README declare MIT, so `LICENSE` holds the standard
  MIT text with the author named in `package.json`.
- `src/` is modified (2026-10-04, plan
  [0051](../../docs/plans/0051-confirm-actions-after-outside-content.md)):
  the `urls` argument of `web_search` and its code paths in `web_search.ts`
  and `index.ts` are removed; the `url_context` tool, `src/url_context.ts`,
  and the model-scoped tool manager that only existed to deactivate
  `url_context` on non-Gemini models are removed. The model must not hand
  addresses to the search provider; `fetch_page`, with its SSRF guard and
  exposure gate, is the one way to read a given address. The README still
  describes the removed `urls` argument and `url_context`; it is left as
  upstream. `format.ts` keeps its now-unused URL-context helpers.
- `src/` is modified again (2026-10-08, plan
  [0059](../../docs/plans/0059-search-after-outside-content.md)): the new
  `src/search_check.ts` posts each query to the application's search check
  (`HOME_AGENT_SEARCH_CHECK_URL`, with the turn's bearer token) and
  `webSearch` in `web_search.ts` returns the decision's text instead of
  searching unless the application answers `allow`. A missing endpoint, a
  failed or slow request (5 s), or a malformed answer also means no search.
  The hook is that one call at the top of `webSearch`.

The image copies this directory to `/usr/local/lib/home-agent/pi-web-search`.
The application loads `src/index.ts` with `--extension` only for principals
who have web access turned on; no `npm install` runs for it, since Pi's
extension loader resolves its Pi, `pi-ai`, `pi-tui`, and `typebox` imports.

To refresh, vendor the new version the same way, review the whole of `src/`
again, update this file, and re-run the live search check from plan 0026.
