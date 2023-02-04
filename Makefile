export GOBIN ?= $(shell pwd)/bin
export PATH := $(GOBIN):$(PATH)

STATICCHECK = bin/staticcheck
REVIVE = bin/revive
GO_FILES = $(shell find . \
	   -path '*/.*' -prune -o \
	   '(' -type f -a -name '*.go' ')' -print)

.PHONY: all
all: lint test

.PHONY: lint
lint: gofmt staticcheck revive

.PHONY: gofmt
gofmt:
	$(eval FMT_LOG := $(shell mktemp -t gofmt.XXXXX))
	@gofmt -e -s -l $(GO_FILES) > $(FMT_LOG) || true
	@[ ! -s "$(FMT_LOG)" ] || \
		(echo "gofmt failed. Please reformat the following files:" | \
		cat - $(FMT_LOG) && false)

.PHONY: staticcheck
staticcheck: $(STATICCHECK)
	staticcheck ./...

.PHONY: revive
revive: $(REVIVE)
	revive -set_exit_status ./...

.PHONY: test
test:
	go test -v -race ./...

.PHONY: cover
cover:
	go test -race -coverprofile=cover.out -coverpkg=./... ./...
	go tool cover -html=cover.out -o cover.html

$(STATICCHECK): tools/go.mod
	cd tools && go install honnef.co/go/tools/cmd/staticcheck

$(REVIVE): tools/go.mod
	cd tools && go install github.com/mgechev/revive
