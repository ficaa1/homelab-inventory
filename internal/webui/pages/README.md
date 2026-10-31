# Plugin pages

A built-in plugin can have its own dashboard page here instead of the generic
declarative view. `<id>.js` and `<id>.css` are inlined into the dashboard at
build time (sorted by name; scripts before the main script, so a page only
registers itself and reaches the kit when shown).

```js
(function () {
  "use strict";
  (window.HI_PAGES = window.HI_PAGES || {}).example = {
    show: function (root, ctx) { /* ctx: id, view, params (URLSearchParams), first */ },
    hide: function () { /* stop timers, abort fetches, revoke blob URLs */ }
  };
})();
```

`show` is called when the route changes to a different view or parameters, not
on the dashboard's own refresh, so a page schedules its own polling (pause it
while `HI.visible()` is false). `window.HI` is the kit:

| Helper | Does |
|---|---|
| `h(tag, attrs, kids)`, `iconNode`, `pill`, `stateBox`, `notice`, `skeleton` | DOM and the dashboard's own components |
| `esc`, `str`, `ago`, `fromNow`, `bytes`, `dur` | formatting |
| `head(id, view, trailing)` | page title and the plugin's views as tabs |
| `get(id, path, signal)` | `GET /plugins/<id>/api/<path>` |
| `send(id, path, method, body)` | JSON mutation to the same API |
| `image(id, path, signal)` | a picture as a blob URL |
| `openDrawer(html, label)`, `closeDrawer()` | the side sheet |
| `go(hash)`, `link(id, view, params)` | navigation |
| `upload(id, target, file)` | send a file to one of the plugin's upload targets through the upload sheet |

The views in the tab come from the plugin's manifest, so keep its `Tab.Views`
and the page's sections in step. The dashboard's CSP forbids inline handlers
and injected `<style>` elements: build nodes with `h` and put styles in
`<id>.css`, scoped under a page class. Never put a credential in page data.
