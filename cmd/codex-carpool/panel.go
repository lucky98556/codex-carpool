//go:build linux && cgo

package main

import (
	_ "embed"
	"html"
	"strings"
)

// panelTemplate is maintained with the other browser assets under web/. Go
// only embeds and serves that frontend; it does not own the panel layout.
//
//go:embed web/index.html
var panelTemplate string

// panelBootstrap runs before the panel's application script. It contains only
// browser-bound safeguards that must also cover the initial page refresh.
//
//go:embed web/bootstrap.js
var panelBootstrap string

// panelStyles and panelApp are maintained by the frontend as standalone
// sources. The plugin embeds them only at release time, so CPA still needs one
// resource route while frontend work remains independently editable.
//
//go:embed web/styles.css
var panelStyles string

//go:embed web/app.js
var panelApp string

const panelCoreBridgeMarker = `const api=(path,opt)=>call(base,path,opt),host=(path,opt)=>call('/v0/management',path,opt);`

const panelCoreBridge = panelCoreBridgeMarker + `window.__ccPanelBridge?.attach?.({state,render,renderLogs,renderOperationalLogs,cpaLocale,uiText,showToast,say});`

// An empty opposite-language value makes String.split("") match between every
// character during a Chinese locale pass. Use an invisible, reversible marker
// for the English page-suffix omission instead.
const panelUnsafePageSuffixLocalePair = `[' 页','']`

const panelSafePageSuffixLocalePair = `[' 页','\u200B']`

// The generic "To" translation must not rewrite the leading characters of
// words such as "Token" while the page is in Chinese.
const panelUnsafeLogTranslator = `const translate=value=>{let output=String(value??'');for(const [zh,en] of pairs){const from=english()?zh:en,to=english()?en:zh;if(from!==to)output=output.split(from).join(to)}return output};`

const panelSafeLogTranslator = `const translate=value=>{let output=String(value??'');for(const [zh,en] of pairs){const from=english()?zh:en,to=english()?en:zh;if(!from||from===to)continue;if(from==='To')output=output.replace(/\bTo\b/g,to);else output=output.split(from).join(to)}return output};`

func panelHTML() string {
	page := strings.ReplaceAll(panelTemplate, "__CODEX_CARPOOL_VERSION__", html.EscapeString(pluginVersion))
	// Keep the visible source link and CPA plugin metadata on one repository
	// constant so releases cannot silently advertise different project URLs.
	page = strings.ReplaceAll(page, "__CODEX_CARPOOL_GITHUB_REPOSITORY__", html.EscapeString(pluginGitHubRepository))
	// The app keeps its state in a closure. Patch the documented compatibility
	// seams in the frontend artifact before it is served, rather than putting
	// browser logic back into Go.
	app := strings.Replace(panelApp, panelCoreBridgeMarker, panelCoreBridge, 1)
	app = strings.Replace(app, panelUnsafePageSuffixLocalePair, panelSafePageSuffixLocalePair, 1)
	app = strings.Replace(app, panelUnsafeLogTranslator, panelSafeLogTranslator, 1)
	page = strings.Replace(page, "</head>", "<script>"+panelBootstrap+"</script><style>"+panelStyles+"</style></head>", 1)
	return strings.Replace(page, "</body>", "<script>"+app+"</script></body>", 1)
}
