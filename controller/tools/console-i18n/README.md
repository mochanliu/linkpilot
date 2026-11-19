# console-i18n

Tooling for the console's Chinese/English switch.

The console renders views by assigning HTML strings, so translations are applied
at runtime: `console.html` stays Chinese, and `cmd/sr-ctrl/assets/i18n-en.js` maps
each Chinese string to English. That design has one failure mode — a string with
no entry silently stays Chinese — so this directory holds the only honest check:
render every view in a real browser and list what is still Chinese.

## Run it

```bash
# a controller with a reachable console, and a Chrome that speaks CDP
node check.mjs --base http://127.0.0.1:8088 --token <admin-token>
node check.mjs --base http://127.0.0.1:8088 --user vm-tenant --pass <pw>

# non-default browser location
CHROME=/usr/bin/chromium node check.mjs --base http://127.0.0.1:8443
```

It logs in, clicks every nav entry plus every module tab inside a page, and prints
the text nodes and attributes that still contain Chinese. Exit code 1 means the
dictionary is incomplete.

`中文` is allowed explicitly: the language switch is labelled in its own language,
and text inside `<pre>`/`<code>` is never translated — translating a command line
would break the command.

## Adding a translation

1. Add the English to `cmd/sr-ctrl/assets/i18n-en.js`:
   - exact string → `SR_I18N_DICT`
   - sentence assembled at runtime (`"3 tunnels"`, `"22 hours 6 min"`) → a regex in
     `SR_I18N_RULES`, with `$1…$9` for the captured numbers
2. `go test ./cmd/sr-ctrl/ -run I18n` — checks the assets are embedded and that no
   English value is still Chinese.
3. `node check.mjs …` — checks it actually renders.

## How the runtime pass works

- Exact dictionary hit, then the rule list, then give up and leave the Chinese.
- Runs on load and on every `MutationObserver` batch, so re-renders (the console
  polls every 5s) are covered without touching any render function.
- The Chinese original of every replaced node is kept in a `WeakMap`, which is what
  makes switching back lossless — there is no "translate the English back" step.
- Never touches input values, `<pre>`, `<code>`, or elements marked
  `data-i18n-skip`.
