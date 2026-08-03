# codex-carpool panel frontend

`index.html`, `styles.css`, and `app.js` are the plugin frontend source of truth.
The Go plugin embeds these files at build time and exposes the finished page at
`/v0/resource/plugins/codex-carpool/panel`. Go owns only the plugin JSON API,
quota ledger, and the small build-time compatibility bridge in `panel.go`.

The frontend calls the existing same-origin management API under
`/v0/management/codex-carpool`. It must not read or write credentials, quota
ledger files, or CPA configuration directly.

For visual work, edit the files in this directory and use `ui-preview/` as the
standalone reference surface. A Linux plugin build is still required to load
the final embedded page in CPA; no CPA source change or additional static-file
deployment is required.
