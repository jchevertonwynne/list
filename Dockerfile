FROM --platform=$BUILDPLATFORM golang:1.26 AS build
WORKDIR /src
# Copy go.mod first so the download layer caches across source edits. If the
# app gains dependencies, add go.sum here too.
COPY go.mod ./
COPY . .
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/app .

# scratch: a static binary needs nothing else. No shell, no libc, nothing to
# patch. If the app makes outbound HTTPS calls it needs CA certificates —
# switch this stage to gcr.io/distroless/static-debian12, which has them.
#
# If it reads or writes local wall-clock time, add `import _ "time/tzdata"` to
# main.go and set TZ in the deployment. A scratch image has no zone database
# and Go falls back to UTC without complaining.
FROM scratch
COPY --from=build /out/app /app
USER 65532:65532
ENTRYPOINT ["/app"]
