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

## Routes

| Route | |
|---|---|
| `GET /` | Collections you own or have been invited to |
| `POST /collections` | Create one |
| `GET /collections/{id}` | Items, people, and owner-only settings |
| `POST /collections/{id}/rename` · `DELETE /collections/{id}` | Owner only |
| `POST /collections/{id}/members` · `DELETE /.../members/{user}` | Owner only |
| `GET/POST/PUT/DELETE /collections/{id}/items/...` | Any member |
| `GET /healthz` | Liveness. No auth, no database |

Everything except `/healthz` and `/static/` is behind authentication, wired as
a single wrapper around one mux so a route added later cannot forget it.

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

There is no shell in the image — it is `FROM scratch`, so `kubectl exec` is
not an option and neither is `sqlite3`. Back it up from the node instead: the
PVC is a directory under `/var/lib/rancher/k3s/storage` on the Pi.

```sh
ssh jcw@jcwpi 'sudo find /var/lib/rancher/k3s/storage -name list.db'
```

Copy it while the Deployment is scaled to zero. WAL mode means a live copy of
`list.db` alone can miss recent commits sitting in `list.db-wal`.
