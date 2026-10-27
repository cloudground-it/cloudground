# The panel's demo

A public instance of the real panel is out of the question: its file manager, SQL console and cron
run code. The interface running against sample data is not, and `ui/demo` is that: the same pages,
their API answered in the browser, linked from <https://cloudground.it/>.

It is served at **<https://get.cloudground.it/demo/>**. The release workflow's `pages` job builds it
from the tag of the newest stable release and deploys it next to `install.sh`, in the Pages
deployment this repository already has (`.github/RELEASING.md`): no new repository, domain, secret or
server. The other way, deploying into the documentation site's Pages, needs a token to write across
repositories or a build of this repository inside the documentation's workflow, which then shows
an old demo until that site is next deployed.

## A build of its own

`npm run build:demo` (`make docker-demo`) is `CG_DEMO=1 nuxt generate`. With `CG_DEMO` unset nothing
changes: `nuxt.config.ts` adds the demo's pieces only for it, so `ui/dist`, the bundle in the binary,
is byte for byte what it was (CI builds the demo and then checks that the committed `ui/dist` is
still what the commit builds). The demo goes to `ui/dist-demo`, which is not committed.

| What the demo build adds | Why |
| --- | --- |
| the plugin `ui/demo/plugin.client.ts`, first of all | installs the demo's API before the panel's first request, and shows the strip with the demo's words, and keeps the layout above it (its height is `--cg-demo-bar`, so nothing sits under it, wrapped or not) |
| `app.baseURL: /demo/` and a hash-mode router | the pages work from `/demo/#/sites/1` on a static host that rewrites nothing |
| a `Content-Security-Policy` meta tag | the page may load and talk to its own origin only, and post no form |

The demo's own words (the strip, the refusal) are in `ui/demo/messages.ts`, in the panel's five
languages, and not in `ui/i18n/locales`, so the real bundle does not carry them.

## No write ever leaves the browser

`ui/demo/install.ts` replaces every way the page has of making a request, before the panel runs:

- `fetch`: `/api/*` is answered by `ui/demo/api.ts`; a GET of a file of the site itself (scripts,
  styles, fonts) goes to the real `fetch`; anything else, another origin or a method that is not GET or
  HEAD, is refused with a `TypeError` and never sent.
- `XMLHttpRequest` (the uploads' way): answered with the same refusal, never opened.
- `EventSource`: only `/api/events` opens, and it stays quiet. `WebSocket` throws; `sendBeacon` returns `false`.

`api.ts` answers a GET from sample data (seven sites: WooCommerce, WordPress, static, Laravel, a staging copy, a Node.js and a Python one;
backups, usage, traffic, redirects, functions, git deploy, external database servers, an AI assistant,
logs, services, users, the blocklist, …) and refuses every other method with **403** and the demo's words, except what
concerns this browser only: signing in and out and the language (`PUT /api/account/profile`), which
change nothing that survives a reload. A page with no sample data gets a 404 that says so.

`ui/tests/demo.test.mjs` (part of `npm test`, so of CI) checks it with a window whose real `fetch` is a
spy: no API request reaches it, every write and every other origin is refused, uploads and the event
stream go nowhere. A run in a real browser (load every page, record every request) is how it was
checked when it was written: none of them left the page's own files.

## Adding a page's data

A page that shows an error toast in the demo has no sample data: add its GET to the table in
`ui/demo/api.ts`, in the shape the page's types say (`ui/app/types`), and a line to the test if it matters.
Sample timestamps are relative to the moment the demo is opened.
