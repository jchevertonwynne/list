ADDR ?= :8080

.PHONY: run test check fmt vet tidy help

help:
	@echo "make run   - run locally on $(ADDR)"
	@echo "make check - everything CI runs: gofmt, vet, race tests"
	@echo "make test  - go test ./... with the race detector"
	@echo "make fmt   - gofmt all source files"

run:
	go run . -addr $(ADDR)

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
