# codex-carpool UI preview

This directory is a standalone visual review surface. It has no CPA, Go, or
database dependency and intentionally uses mock data only.

Open `index.html` directly in a modern browser to review the desktop layout.
The preview imports `../cmd/codex-carpool/web/styles.css`, which is the same
stylesheet embedded into `codex-carpool.so`. Changes to that production
stylesheet, `index.html`, or `app.js` take effect after a browser refresh;
rebuilding `codex-carpool.so` is not required for this review step.

There is intentionally no preview-only palette file. This keeps the approved
preview colors and the packaged CPA panel on the same source of truth.
