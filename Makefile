ADDR ?= :8094
DB ?= /tmp/list.db
# There is no Cloudflare Access in front of a local server, so a local run
# needs an identity supplied by hand or every request is refused.
DEV_USER ?= dev@example.com

.PHONY: run test check fmt vet tidy help

help:
	@echo "make run   - run locally on $(ADDR) as $(DEV_USER)"
	@echo "make check - everything CI runs: gofmt, vet, race tests"
	@echo "make test  - go test ./... with the race detector"
	@echo "make fmt   - gofmt all source files"

run:
	go run . -addr $(ADDR) -db $(DB) -dev-user $(DEV_USER)

test:
	go test -race -cover ./...

# Mirrors the CI workflow, so a green 'make check' locally means a green CI.
check:
	@unformatted=`gofmt -l .`; \
	if [ -n "$$unformatted" ]; then echo "these files need gofmt:"; echo "$$unformatted"; exit 1; fi
	go vet ./...
	go test -race -cover ./...

fmt:
	gofmt -l -w .

vet:
	go vet ./...

tidy:
	go mod tidy
