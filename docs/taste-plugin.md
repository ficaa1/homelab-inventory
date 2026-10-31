# Taste plugin

Taste is compiled in and disabled on fresh installs. Configure Media first, then
set Taste `bindings` to a JSON object mapping household profile IDs to explicit
playback owners, for example `{"household":{"owner":42}}`. Each owner may occur
once. Existing files cannot be rebound to another owner. Dusk uses the paired
device's server-owned household profile; a device cannot supply another identity.

Optional `letterboxd_username` on a binding enables public RSS/watchlist/ratings/
likes synchronization. Supply `letterboxd_url` explicitly. The plugin uses its
guarded HTTP client for all public pages; Media credentials remain in Media.
Import full export ZIPs through Uploads' profile-specific Letterboxd target. Only
ratings, watched, watchlist and film likes are read; reviews and account data are
ignored. Failed/partial scans retain prior preferences; complete watchlist scans
can remove absent entries. Fingerprints, sync checkpoints and import times remain
in private per-profile storage.

The content model retains explicit judgement overrides, watched exclusions,
creator/theme affinities, quality priors, negative evidence, diversity and series
completion signals. The dashboard exposes picks, known, familiar and watchlist;
local actions record or clear verdicts, hide recommendations, refresh and sync.
Models and metadata caches persist; background builds and sync workers cancel and
drain when the plugin closes or settings change.

Taste twins is optional. No datasets or trained artifacts are distributed here.
Users must obtain permission under the source dataset/model license (including
GroupLens/MovieLens restrictions where applicable). Place an approved sparse JSON
artifact in the plugin cache and configure its filename in `twins_artifact`.
The JSON has `Source`, `Films` (`TMDB`, `Title`, `Count`, `Mean`) and matching `Rows`
arrays of `{ "To": 0, "W": 0.5 }` edges. Array indices refer to `Films`. Conversion
from an existing trusted legacy gob must happen offline under the user's control;
restore never decodes gob. The unchanged sparse weights preserve EASE scoring,
interleaving, taste lanes and familiar ordering when supplied.

Migration requires an explicit household ID and legacy numeric owner. Full
Letterboxd state, verdict timestamps, hidden entries, model and caches are copied
byte for byte into that profile. Legacy gob is preserved opaque, awaiting the
licensed JSON conversion. Request receipts are retained separately in Dusk and
consulted as a global duplicate barrier, including unknown outcomes. Import never
creates devices or transfers request authority; pair devices and configure an
explicit destination request policy separately.
