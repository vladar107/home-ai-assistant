# Prompt variants

A variant is a JSON file here, `<name>.json` (lowercase letters, digits and dashes), that the eval
harness runs with `home-agent-eval run --variant <name>`. Production never reads these files.

- `description` (required): what the variant tries.
- `sections`: a system-prompt section name (`intro`, `no_tools`, `tools`, `web_search`, `outro`,
  `initiative`) mapped to its replacement text.
- `tool_guidance`: a tool name mapped to text that replaces the guidance of the connector offering
  that tool.
- `model`, `effort`: replace the run's defaults; a case that pins its own model keeps it. A
  `model` also replaces the task and Heads-up models, with the `effort` when one is set.

Compare a variant run with a baseline run with `home-agent-eval compare <baseline> <variant>`. A
variant that wins ships as an ordinary change to the default prompt with its golden files, and its
file is removed. Variants hold made-up text only, and `home-agent-eval scan` checks them.

`weak-model` is kept for sensitivity runs: it names a clearly weaker model, and its comparison
with a baseline lists under "Not sensitive to the model" the cases that passed every baseline
trial and did not drop, with at least three trials in each run; a case that pins its own model is
not counted. Such a case may not be testing the model; tighten its checks.
