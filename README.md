# app-template

Template for a service on the k3s cluster described in
[homelab](https://github.com/jchevertonwynne/homelab). Not meant to be run as
is — it is the shape the cluster expects, with the parts that are easy to
forget already in place.

Create a new app with `make new-app` in the homelab repo, which uses this
template and renames the module for you.

## What is here, and why

**`/healthz`** — the readiness and liveness probes hit this several times a
minute forever. Keep it cheap and dependency-free; pointing probes at `/`
means whatever `/` does, it does permanently.

**Graceful shutdown on SIGTERM** — Kubernetes sends SIGTERM and waits before
SIGKILL. Anything that must be flushed on the way out goes after `Shutdown`
returns.

**HTTP timeouts** — without `ReadHeaderTimeout` one client can hold a
connection open indefinitely by dribbling out headers.

**`FROM scratch`** — no shell, no libc, nothing to patch. Two things it does
not have, both noted in the Dockerfile: CA certificates (needed for outbound
HTTPS) and a timezone database (needed by anything touching local wall-clock
time, along with `import _ "time/tzdata"` and `TZ` in the deployment — the
failure is silent and shifts times by up to an hour).

**Four-line image workflow** — the build itself lives once in
`jchevertonwynne/workflows`, so every app shares it.
