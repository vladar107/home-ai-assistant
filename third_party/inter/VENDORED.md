# Vendored: Inter

- Source: Inter by Rasmus Andersson, release `v4.1` of `github.com/rsms/inter`,
  downloaded from
  `https://github.com/rsms/inter/releases/download/v4.1/Inter-4.1.zip`.
- Integrity: SHA-256 of the archive
  `9883fdd4a49d4fb66bd8177ba6625ef9a64aa45899767dde3d36aa425756b11e`.
- Licence: SIL Open Font License 1.1; `LICENSE` is `LICENSE.txt` from the
  archive, unmodified.
- Vendored: 2026-09-28, for plan
  [0028](../../docs/plans/0028-day-card.md).

`internal/daycard/assets/fonts/` holds `extras/ttf/Inter-Regular.ttf` and
`extras/ttf/Inter-SemiBold.ttf` from the archive, unmodified. They are the
static TrueType instances, which cover Latin and Cyrillic; the variable font
is not used because `golang.org/x/image/font/opentype` renders only the
default instance.

| File | SHA-256 |
| --- | --- |
| `Inter-Regular.ttf` | `40d692fce188e4471e2b3cba937be967878f631ad3ebbbdcd587687c7ebe0c82` |
| `Inter-SemiBold.ttf` | `78a843fade9d4612a5567302fb595b56976eb5fcebf4fea5a5912d638bafcde3` |

To refresh, copy the same two files from a new release, update this file, and
review the sample cards from `internal/daycard` again.
