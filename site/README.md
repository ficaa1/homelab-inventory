# Project page

The page published at www.filipbiljic.com/homelab-inventory/ by `.github/workflows/pages.yml`.

- `index.html`: the page. Its search box holds a copy of the dashboard's search index for the example fleet and ranks results the way the dashboard's palette does.
- `img/`: screenshots of the dashboard on the example fleet, light and dark.
- `fonts/`: Bitcount Prop Single and Schibsted Grotesk, self-hosted, with their OFL licences.
- `demo/`: a read-only copy of the dashboard. `index.html` is the dashboard page as the API serves it. `data.js` is a recording of the API's GET responses for an invented six-host fleet. `shim.js` answers the page's requests from that recording, moves timestamps forward so times stay current, and refuses anything that would change state.

All hosts, services and data on the page and in the demo are invented.
