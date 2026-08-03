---
name: cpa-panel-style-isolation
description: Protect codex-carpool panel UI from CPA host CSS and runtime layout interference. Use when creating, changing, reviewing, or debugging plugin HTML, CSS, dialogs, buttons, tables, inputs, responsive layouts, localization, or light/dark theme behavior under cmd/codex-carpool/web.
---

# CPA Panel Style Isolation

## Workflow

1. Read `../../rules/cpa-panel-style-isolation.md` before editing UI files.
2. Inspect `web/index.html`, `web/styles.css`, `web/app.js`, and `panelHTML()` assembly. Treat the assembled production panel as the source to validate.
3. Keep controls in stable HTML positions. Do not append management buttons after startup when they can exist statically.
4. Scope ordinary components under `.cc-panel`. Root every top-layer dialog rule at its unique ID, including backdrop, header, body, footer, buttons, and inputs.
5. Preserve CPA theme variables for colors. Own dimensions, spacing, overflow, positioning, and interaction states inside the plugin.
6. Add narrow static guards for new component roots and known host-conflict properties.
7. Verify JavaScript syntax, Go tests, `git diff --check`, Chinese and English labels, light and dark themes, and the actual CPA-hosted page.

## Guardrails

- Do not add naked global selectors such as `button`, `input`, `header`, `footer`, `.form`, or `table` for new production UI.
- Do not use `all: initial` or reset the whole page.
- Use `!important` only on layout-critical properties whose CPA collision is confirmed; keep the scope at the component or dialog ID.
- Do not accept preview-only fidelity. Compare the embedded production output and the hosted CPA result.
- Do not change quota, routing, database, or API behavior while fixing presentation unless the user explicitly requests it.

