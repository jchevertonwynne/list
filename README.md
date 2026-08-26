# list

Shared to-do lists, at [list.jchevertonwynne.uk](https://list.jchevertonwynne.uk).
A collection holds items; an item has a title, an optional note, a creator and
a done flag. The owner of a collection can invite other people to work on it.

- Go + `net/http`, stdlib routing, `html/template`
- htmx for the interactions, Beer CSS (Material 3) for the look — both vendored
- One dependency: `modernc.org/sqlite`
- Runs on a Raspberry Pi in a k3s cluster, behind a Cloudflare tunnel

## There is no login

Cloudflare Access authenticates every request at its edge, before anything
reaches the Pi, and passes the address on in
`Cf-Access-Authenticated-User-Email`. This app reads that header and treats it
as the identity. It stores no passwords, issues no sessions and has no login
page, because it never sees an unauthenticated request.

Trusting a plain header is only reasonable because of where this runs. The
Service is a ClusterIP and the only thing that can reach it is the cloudflared
pod; there is no path to the origin that does not pass through Access first.
**If that ever stops being true — a second hostname routed to this Service, an
Ingress, a NodePort, anything — the header becomes caller-controlled and this
app becomes a login-as-anyone form.** The fix at that point is to verify
`Cf-Access-Jwt-Assertion` against the team's JWKS, checking the signature
*and* the per-application `aud`, because a JWT minted for any other app in the
same Access team is otherwise signature-valid here.

The one thing the middleware will not do is guess. A request with no header
and no `-dev-user` is rejected with 403 rather than falling through to an
empty-string identity — that would silently merge every visitor into one
shared account and show everyone the same lists.

## Who can do what

`internal/web/authz.go` is the entire authorisation model, and every handler
that touches a collection goes through it. The ids in the URLs are just
integers anyone can type; nothing else stands between them and the data.

| | owner | member | anyone else |
|---|---|---|---|
| see the collection | yes | yes | **404** |
| add, edit, delete, tick items | yes | yes | 404 |
| set, replace or remove the cover | yes | yes | 404 |
| rename or delete the collection | yes | 403 | 404 |
| invite and remove people | yes | 403 | 404 |

Non-members get **404, not 403**, deliberately. A 403 confirms that a
collection with that id exists, which turns the URL space into a directory of
other people's lists. A member who is merely not the owner does get 403,
because they already know it exists. Items are checked twice: membership of
the collection in the URL, and then that the item actually belongs to *that*
collection — without the second check, membership of any one collection would
be enough to edit an item in any other by pairing an id you can see with an id
you cannot.

Members can edit items they did not create. `creator` is displayed, not
enforced; it is a label, not a permission.

## Adding a person

Two steps, in this order, and both are needed:

```sh
# 1. let them past Access (in the homelab repo). Reconciles to exactly
#    these addresses, so list every person who should have access.
./scripts/access-app.sh --host list.jchevertonwynne.uk \
  --email jchevertonwynne@gmail.com --email them@example.com

# 2. invite them to a collection, in the app
```

Inviting an address that Access does not allow is permitted and simply waits —
the membership row exists and the collection appears for them the first time
they successfully sign in. The invite form says so, because otherwise it looks
broken.

## Why SQLite, when the counter next door is a file

`homepage` keeps its visit count in a plain file and that is the right call
there: one writer, one integer, no relationships. None of that holds here.
Collections, items and memberships refer to each other, several people write
concurrently, and accepting an invite has to add a user and a membership as
one atomic step. A JSON file would mean hand-rolling transactions and
rewriting the whole document on every tick of a checkbox.

`modernc.org/sqlite` is a pure-Go implementation, so `CGO_ENABLED=0` still
holds and the arm64 build needs no cross-compiler — the property that made the
zero-dependency rule worth having in the first place survives the dependency.

Notes for anyone editing the store:

- **`PRAGMA foreign_keys` is OFF by default in SQLite.** Every `ON DELETE
  CASCADE` is silently ignored without it, so deleting a collection would
  leave its items behind forever. It is set in the DSN rather than after
  `Open` because `database/sql` pools connections and a pragma set once
  applies only to whichever connection happened to run it. A test deletes a
  collection and asserts the items are gone; that test is what proves the
  pragma is really on.
- `temp_store=memory`, because the container is `FROM scratch` with a
  read-only root filesystem. There is no `/tmp` to spill into.
- Creating a collection inserts the owner's membership row in the same
  transaction. An owner without one vanishes from their own index page.

## CSRF

Access authenticates with a cookie, so a `POST` from another site arrives
with valid credentials attached — authenticated is not the same as intended,
and Access does nothing about the difference.

Mutations therefore require an `HX-Request: true` header. It is not a
CORS-safelisted header, so a cross-origin caller must pass a preflight first,
and this server answers no preflight at all; a plain HTML form, the classic
vector, cannot set custom headers under any circumstances. The header's
presence is the proof, and nothing has to be stored to check it — which suits
an app that keeps no session state of its own. A mismatched `Origin` is
refused as well.

## Links in item text

An item's title and note are free text, so a URL pasted into either one
renders as a clickable link rather than dead text you have to select and
copy. Three schemes count: `http://`, `https://`, and a bare `www.`, which
is linked as `https://` since nobody types the scheme when they say
"www.example.com". Bare domains (`example.com` with no `www.`) and email
addresses deliberately do not become links. A bare domain can't be
detected without false positives — too much ordinary prose looks like
`word.word`. An email address could be made a `mailto:` link, but on a
phone that opens a mail client, which is a surprise when the address was
only ever a note to self, not an invitation to write to someone.

Links open in a new tab with `rel="noopener noreferrer"`. `noreferrer` is
doing real work rather than duplicating a header: there is no
`Referrer-Policy` anywhere in the app, so without it every click hands the
destination site this app's origin. Current browsers default to
`strict-origin-when-cross-origin`, so what leaks is the hostname rather than
which list you were looking at — but on a private app behind Access the
hostname is the interesting half, and `noreferrer` suppresses it outright.

The linkifier itself never touches HTML. It splits an item's text into
segments and hands the template plain strings — a run of text, or a URL —
and the template does every bit of escaping: `{{.Text}}` renders in HTML
text context, and `{{.URL}}` renders inside an `href`, which is URL
context and gets `html/template`'s URL filter as an independent second
gate on the scheme, on top of whatever the linkifier already decided was
safe. Nothing in this feature hand-assembles a string of HTML out of text
a user typed, which is the same property [CSRF](#csrf) above and
[No external requests](#no-external-requests) below are both examples of:
this app is fairly conservative about what it lets itself do with input it
doesn't control.

## No external requests

Beer CSS, htmx and the Material Symbols icon font are vendored into
`internal/assets` and compiled into the binary. Beer CSS ships pointing at
jsdelivr for its font; those fallbacks are stripped, and the icon font is
served from the same URL directory as the stylesheet because the `@font-face`
path is relative — split them up and every icon renders as the literal word
`check_box`.

This matters beyond neatness. It is a private app, and a page that fetches a
font from a third party hands that third party the IP of everyone who opens
it.

## Cover images

Any member may paste or pick one photo per collection, shown as a banner at
the top of that collection's page — see
[Who can do what](#who-can-do-what) above; there is no owner-only carve-out
here, the same as any other item-level action. There is no thumbnail on the
index list, deliberately: that keeps `CollectionsForUser` untouched and
keeps covers off the index's live-update path entirely.

**Storage.** The bytes live in SQLite, in their own `collection_images`
table, not a column on `collections`. `CollectionsForUser` and `Items` both
`SELECT` every column of the rows they touch on every page load, so a blob
column on `collections` would push those rows onto overflow pages and drag
image bytes through the page cache just to render a list of names.
`collection_id` is the table's sole primary key, which expresses "one cover
per collection" in the schema itself rather than in handler discipline, and
turns a replace into a plain `INSERT ... ON CONFLICT DO UPDATE`. Deleting the
collection reclaims its cover with no extra code, via the same
`ON DELETE CASCADE` and `foreign_keys(1)` pragma that already covers items
and memberships. Storing the bytes in SQLite, rather than as files on the PVC,
also keeps the [backup story](#backups) intact: scale to zero, copy
`list.db`. A file on disk would need its own cleanup path, and could orphan
on a crash between the database write and the unlink.

**Uploading with no writable `/tmp`.** The container is `FROM scratch` with
`readOnlyRootFilesystem: true`, so there is nowhere for anything to spill to.
That rules out `ParseMultipartForm`/`FormFile`: past its memory limit,
`mime/multipart`'s `ReadForm` spills the remainder to `os.CreateTemp`, and a
cover here is capped at only a couple of megabytes on a Pi with no writable
filesystem to catch that write. So the handler reads the request with
`r.MultipartReader()` instead, takes the single part named `cover`, and
copies it into memory through a length-limited reader. The streaming reader
has no disk-spill code path at all — "nothing ever spills" is a property of
which function got called, not of a byte constant that has to stay in sync
with reality. A later "simplification" back to `r.FormFile` would silently
reintroduce the crash, and worse, would silently fall back to Go's 32MB
default the moment nobody remembered to pass a limit — quietly
disconnecting the cap from the code that is supposed to enforce it.

**Serving: content-addressed and immutable.**
`GET /collections/{id}/cover/{etag}` serves the bytes. The etag in the URL is a
SHA-256 prefix of the stored bytes, and the handler 404s on any mismatch —
which is what makes `Cache-Control: private, max-age=31536000, immutable`
true rather than aspirational: the URL cannot go stale under a client that
already has it, because a change in the bytes is a change in the URL. That
header is the one named exception to a rule set everywhere else:
`authenticate` sets `Cache-Control: no-store` as the *default* on every
response, because an authenticated page is specific to whoever asked for it
and must never sit in a shared cache — Cloudflare's edge in particular,
which sits in front of this app. A cover response is the one thing allowed
to override that default, through a helper named `cacheImmutable`, and only
because the URL is content-addressed and the caller has already been
through the same membership check as every other collection route. `private`
in that header is load-bearing, not decorative: `public` would let
Cloudflare's edge cache a member-only image for anyone behind it to
receive. Content-Type comes from the server's own record of what it decided
to store, never from anything the client claimed, and ships alongside
`X-Content-Type-Options: nosniff`.

**Nothing survives the round trip.** A phone photo carries EXIF, and EXIF
carries GPS coordinates. The browser's own canvas step already discards all
of it before upload (see `cover.js`), but the client is not a trust
boundary — anyone with a session can `curl` a `PUT` straight past it with an
untouched photo. So the server decodes every upload and re-encodes it with
`image/jpeg` before it is ever written to disk, and Go's JPEG encoder emits
**zero** `APPn` segments of any kind — only SOI, DQT, SOF0, DHT, SOS and
EOI. EXIF, an ICC profile, XMP, an embedded thumbnail: none of it can reach
the database, regardless of what arrived, because the format the server
writes has no field left to carry it in. This sits next to [No external
requests](#no-external-requests) above for the same reason: both are about
what this app refuses to let leak, one outbound and one in an uploaded
file.

The decode step is guarded before it runs, not just after:
`image.DecodeConfig` reads the header first, and a request is rejected if it
claims an edge over 8000px or more than 40 megapixels, before `image.Decode`
ever runs. A two-megabyte JPEG can quite legally declare a 30000×30000
image, and `image.Decode` allocates roughly `width * height * 4` bytes for
whatever the header claims — a decompression bomb is a real way to get a Pi
OOM-killed, and the byte cap on the request body alone does not bound that.
Only PNG and JPEG are accepted in the first place: the server re-encodes
everything, and the stdlib has no encoder for WebP or GIF, so accepting
either would mean shipping it back out unstripped. SVG falls out for free —
it sniffs as `text/xml`, never as an image — which matters because an SVG
served from this origin would execute script *in* this origin.

## Live updates

Every interaction used to be a request/response htmx swap scoped to the
browser that made it, which was wrong the moment two people had the same
collection open: neither saw the other's changes, and an owner who removed
someone — or deleted the collection outright — left that person's page
looking fine right up until their next click 404s with no explanation. Now a
change made in one browser appears in every other browser with that
collection or the index page open, within a second, and losing access to a
collection you're looking at bounces you to `/` instead of stranding you on a
page that's quietly gone dead.

The transport is Server-Sent Events, not WebSockets, because the traffic is
one-directional: nothing a browser does needs to travel back upstream outside
the htmx request that already carries it, so the duplex half of a WebSocket
would sit unused. `EventSource` also reconnects on its own, which is the
single largest chunk of client code a WebSocket would have required us to
write and get right by hand. The README's "One dependency:
`modernc.org/sqlite`" line above is still accurate — `internal/live` and
`internal/assets/static/live.js` are both hand-rolled against the stdlib and
the browser's own `EventSource`.

What travels over the stream is not HTML but a small JSON hint —
`{"kind":"item","id":42,"action":"updated"}` and the like — and the client
reacts by re-fetching the affected piece over an ordinary `GET`. Two reasons
that's the right split rather than broadcasting rendered markup directly:

- The People list renders differently depending on who's looking —
  `{{if $.IsOwner}}` in `fragments.html` gates the remove button — so one
  broadcast blob of HTML cannot be correct for both the owner and a plain
  member at the same time. A hint plus a re-fetch means each viewer's copy is
  rendered for *them*.
- The re-fetch goes back in through `collectionFor`, the same function every
  ordinary page load and htmx swap already goes through. Authorisation is
  re-checked at delivery time by the code that already owns it, rather than
  the hub having to know who's allowed to see what.

Item events are scoped to a single item rather than triggering a whole-list
refresh, for a more specific reason: if you have an item's edit form open and
someone else ticks a *different* item, a wholesale `#items` swap would
overwrite the form and throw away what you'd typed. A per-item event lets the
client patch just the row that changed and leave the rest of the page alone.

The hub is lossy on purpose. A publish is a non-blocking send to each
subscriber's small buffered channel; a subscriber that isn't reading fast
enough gets dropped rather than allowed to stall the publisher — which
matters because the publisher is an HTTP handler, and blocking it means
blocking whoever's waiting on that response. A dropped subscriber's
`EventSource` just reconnects, and reconnecting triggers a full re-sync of
whatever the page shows, so a missed event is a temporary gap rather than a
permanent inconsistency. The same self-healing property is what makes the
buffer safe to keep small.

The hub itself is in-process, with no pub/sub broker behind it, because the
[Deployment](#deployment) below already guarantees there's only ever one
process to fan events out from — `Recreate` on a ReadWriteOnce SQLite volume
means a second replica was never possible in the first place. An in-memory
map of subscribers isn't a stopgap here; it's the whole correct answer for as
long as that deployment shape holds.

Two things worth knowing if you're touching this next:

- `main.go`'s `http.Server` sets a `WriteTimeout`, same as before live
  updates existed, and it's still in force for every ordinary request. A
  stream would be killed by it well before an idle client noticed anything
  wrong, so the live handler clears the read/write deadlines on that one
  connection via `http.NewResponseController`, rather than loosening the
  timeout globally and losing that protection everywhere else.
- `Server.Close()` — which closes the hub — runs *before* `srv.Shutdown` in
  `main.go`, not after. An SSE response is an ordinary handler as far as
  `net/http` is concerned; nothing is hijacked the way a WebSocket upgrade
  is. Left open, `Shutdown` would sit and wait for every live connection to
  end on its own, which on a real rollout means burning the full shutdown
  timeout on every single deploy. Closing the hub first closes every
  subscriber's channel, so each stream handler's read loop sees that and
  returns immediately — shutdown measured at 107ms with the hub closed
  first, against the full 10s timeout with the ordering reversed.

## Routes

| Route | |
|---|---|
| `GET /` | Collections you own or have been invited to |
| `POST /collections` | Create one |
| `GET /collections` | The `#collections` fragment, re-fetched on a live update |
| `GET /collections/{id}` | Items, people, and owner-only settings |
| `POST /collections/{id}/rename` · `DELETE /collections/{id}` | Owner only |
| `POST /collections/{id}/members` · `DELETE /.../members/{user}` | Owner only |
| `GET /collections/{id}/members` | The `#members` fragment, rendered per viewer |
| `GET/POST/PUT/DELETE /collections/{id}/items/...` | Any member |
| `GET /collections/{id}/items` | The `#items` fragment, re-fetched on a live update |
| `GET /collections/{id}/cover/{etag}` | The banner image, content-addressed and cached hard — see [Cover images](#cover-images) |
| `PUT /collections/{id}/cover` · `DELETE /.../cover` | Set/replace or remove the cover. Any member |
| `GET /live` | SSE stream for the index page — see [Live updates](#live-updates) |
| `GET /collections/{id}/live` | SSE stream for a collection page |
| `GET /healthz` | Liveness. No auth, no database |
| `GET /backup.db` | A consistent snapshot for the backup CronJob. No auth — see [Backups](#backups) |

Everything except `/healthz`, `/static/`, `/metrics` and `/backup.db` is behind
authentication, wired as a single wrapper around one mux so a route added later
cannot forget it. The four exceptions are registered on the outer mux instead,
which is what puts them outside that wrapper — each for a reason given where it
is defined.

## Run locally

```sh
make run     # :8094, a database in /tmp, and a stand-in identity
make check   # gofmt, vet, race tests — what CI runs
```

There is no Access in front of a local server, so `make run` passes
`-dev-user`. Override it to be someone else:

```sh
make run DEV_USER=someone@example.com
```

`-dev-user` is empty in production and the app logs a warning whenever it is
set. With it empty and no header present, every request is refused.

## Deployment

Push to `main`: CI builds an arm64 image, Flux notices the tag, commits it to
[homelab](https://github.com/jchevertonwynne/homelab) and rolls the pod.

The database is a `local-path` PersistentVolumeClaim mounted at
`/var/lib/list`. The Deployment uses `Recreate` rather than `RollingUpdate`
because SQLite on a ReadWriteOnce volume is single-writer — two overlapping
pods cannot both hold it. The cost is a few seconds of 502 per deploy.

## Backups

`GET /backup.db` streams a consistent snapshot, and the hourly backup CronJob
in [homelab](https://github.com/jchevertonwynne/homelab) calls it and commits
the result to `homelab-backups`. Nothing manual is required.

```sh
kubectl -n apps port-forward deploy/list 8094:8094
curl -sO http://localhost:8094/backup.db
```

It is a `VACUUM INTO` through the running process, which is the only way to
get a consistent copy without stopping the app — and stopping the app is what
every other option requires. There is no shell in the image (`FROM scratch`),
so `kubectl exec` is not available and neither is `sqlite3`. Copying the files
off the PVC on the node looks like the obvious alternative and quietly loses
data: under WAL mode recent commits sit in `list.db-wal`, which here is
routinely *larger* than `list.db` itself, so a copy of the database alone can
come back as a valid, openable, empty database — which is a far worse outcome
than an error. Copying all three files together is a torn read unless the pod
is stopped first, which turns an hourly backup into hourly downtime.

Two things about that endpoint are worth knowing rather than rediscovering.
It is unauthenticated at the application level, like `/metrics`, because the
CronJob calls it from inside the cluster where there is no Cloudflare Access
session to present — so what protects it is Access on the hostname and
nothing else, and anyone the allowlist admits can fetch the whole database in
one request. And it does real work proportional to the database size, with no
rate limit, which is acceptable only because that same sentence bounds who
can call it. See `internal/web/backup.go`.

`list.db` grows with cover image data, not just rows of text, and
`list.db-wal` will routinely run to several megabytes: upserting a
few-hundred-kilobyte blob rewrites that row's overflow pages, and WAL mode
defers folding those pages back into the main file until a checkpoint. A
removed cover frees its pages, but SQLite never hands them back to the
filesystem on its own. Nothing compacts `list.db` in place: the `VACUUM INTO`
behind `/backup.db` writes a *new* file and leaves the original exactly as
large as it was, which is why the snapshot in `homelab-backups` can be
noticeably smaller than the database it came from. And `auto_vacuum` cannot be
turned on for a file that already exists without running a plain `VACUUM`,
since it only takes effect on the next database created from scratch. So
`list.db` only grows over time, even as covers are replaced and removed; that is an accepted trade-off for a household list on a Pi with
disk to spare, not an oversight.
