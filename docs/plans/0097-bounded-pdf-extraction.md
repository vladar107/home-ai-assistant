# 0097 — PDF text extraction with bounded memory and time

Status: `done`

## Context

`internal/pdftext` is the one place the application parses PDFs: web pages (`fetch_page`), NAS
files, and Telegram attachments all go through `pdftext.Extract`. It uses
`github.com/ledongthuc/pdf`, which decodes a page's content stream as it lexes it, but builds every
token, the operand stack, and the page's text in memory with no limit. Our own caps (the download
size, `MaximumPages`, `MaximumTextBytes`) apply only after a page's text is complete, so a small PDF
whose FlateDecode stream inflates to hundreds of MiB of one string token makes the library allocate
several GiB, far above the container's memory limit. The service is OOM-killed for every user, and
a watch or scheduled job that opens the same document repeats it.

`Extract` runs the library in a goroutine and returns when the context ends, but the goroutine
cannot be interrupted and keeps running, and any number of extractions may run at once.

## Goal

No PDF can push the service past a bounded amount of memory or CPU time. An extraction whose
context ends, or that exceeds its limits, is stopped, not abandoned. Only one extraction runs at a
time. Callers keep the same API and behaviour: the page and text caps with the trailing
"[The text stops here…]" line, `ErrNoText`, `ErrEncrypted`, `ErrMalformed`, and no crash on a
library panic.

## Decisions

- Extraction runs in a child process, not in the service. Two options were weighed:
  - Forking the library under `third_party/` and wrapping its stream readers with limits would
    bound the one known vector, decompressed stream size, but the library has other unbounded
    paths (token and operand-stack growth, recursion through object graphs, which overflows the
    goroutine stack as a fatal, unrecoverable error), and every one would have to be found and
    patched in a fork of about 9,000 lines we would then maintain. It also still could not stop a
    timed-out extraction, because Go cannot kill a goroutine.
  - A child process bounds whatever the library does, including bugs not yet known: the kernel
    enforces its limits, a fatal runtime error kills only the child, and a timed-out extraction is
    killed outright. The cost is one process start per extraction, a few milliseconds, which is
    small beside parsing a PDF.
  The child process is the more robust option.
- The child is the running binary itself, re-executed. `pdftext` recognises the child by a marker
  environment variable in a package `init`, so every binary that links `pdftext` — the service and
  every test binary that reaches `Extract` — can serve as its own worker without changes to `main`
  or to other packages' tests. The marker is internal, not a setting.
- The child gets no environment beyond the marker, so it never sees the service's secrets, and only
  the PDF on stdin. It answers with one JSON object on stdout carrying the text or the refusal.
- Limits in the child, applied before it reads the PDF:
  - a soft Go memory limit (`debug.SetMemoryLimit`) so the collector works hard before the ceiling;
  - a memory watchdog that exits as soon as the runtime's mapped memory passes the ceiling, which is
    the only memory bound macOS enforces;
  - on Linux, `RLIMIT_AS` and `RLIMIT_DATA` set to the child's footprint at start plus the ceiling,
    so the kernel refuses the allocation itself (`RLIMIT_DATA` counts the Go heap's mappings on
    current kernels; `RLIMIT_AS` covers older ones);
  - `RLIMIT_CPU` on every Unix, a smaller maximum goroutine stack, and `GOMAXPROCS` of one.
  The address-space limits are skipped in race-detector builds, whose shadow memory would trip
  them.
- The parent runs one child at a time, waits for its turn only as long as the caller's context
  allows, kills the child when the context ends or a wall-clock limit passes, and caps how much
  output it accepts. A child that is killed, crashes, or answers out of protocol is reported as
  `ErrMalformed`; an ended context is reported as the context's error, as before.

## Approach

1. Split `pdftext` into the parent side (`Extract`: turn-taking, starting and killing the child,
   decoding its answer) and the worker side (the `init` hook, limits, and the existing extraction
   and normalisation code, which no longer needs a context).
2. Keep the exported API and error values unchanged, so the web, NAS, and attachment callers are
   untouched.
3. Add regression tests: a generated PDF whose content stream inflates to far more than the
   ceiling must be refused as `ErrMalformed` quickly, and an extraction cut off by its context must
   not hold up the next one.
4. Document the worker process in the architecture notes.

## Files affected

- `internal/pdftext/` (extraction, worker, limits, tests)
- `docs/architecture.md`
- `docs/plans/0097-bounded-pdf-extraction.md`, `docs/plans/README.md`

## Validation

- Formatting, module, vet, build, race tests, the full suite, and the plan check.
- The bounded-extraction test on macOS and in a Linux container, where the kernel limits apply.

## Risks and open questions

- On macOS only the watchdog, the soft memory limit, and the CPU and wall-clock limits apply; a
  single allocation can overshoot the ceiling before the watchdog sees it. macOS is used only for
  local development.
- A legitimate PDF that needs more than the ceiling is now refused as damaged. The ceiling is far
  above what ordinary documents need, and the text cap already limits what a reader gets.
- Starting the child relies on the binary still being present at its path; on Linux it is started
  through `/proc/self/exe`, which survives the file being replaced.
