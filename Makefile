# dnv build entry points (layout.md, Protobuf generation; layout.md,
# `cmd/` wiring; the release archive, layout.md, Directory tree).
GO ?= go
PROTOC ?= protoc

# The cmd/ directories that already contain Go sources.
CMDS := $(sort $(patsubst cmd/%/,%,$(dir $(wildcard cmd/*/*.go))))

.PHONY: all gen fmt build vet test fetch-etcd release clean

all: build

# Regenerate pb/schema.pb.go and pb/schema_grpc.pb.go. Only needed when
# pb/schema.proto changes; the generated files are committed, so plain
# build/test never require protoc.
# Requires protoc, protoc-gen-go and protoc-gen-go-grpc on PATH.
gen:
	$(PROTOC) \
		--go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		pb/schema.proto

# Format sources: gofmt for Go, clang-format for the proto (settings in
# .clang-format; the binary comes from `pip install clang-format`).
CLANG_FORMAT ?= clang-format

fmt:
	gofmt -w .
	$(CLANG_FORMAT) -i pb/schema.proto

build:
	@for c in $(CMDS); do \
		echo "  build bin/$$c"; \
		$(GO) build -o bin/$$c ./cmd/$$c || exit 1; \
	done

vet:
	$(GO) vet ./...

# test runs every package's tests. The tests of etcdutil, model, gateway,
# worker, cdc and integtest/plant run against a real etcd (dnv-worker.md
# EU7). An ETCD_BIN that is not empty is passed on unchecked, a relative path
# taken from this directory, since each test binary runs in its own package's
# directory; the test binaries refuse one that names no file. With ETCD_BIN
# empty or unset the target takes etcd on the PATH, else the one `make
# fetch-etcd` leaves in integtest/bin/cache, and with neither it fails before
# go test runs.
test:
	@bin="$$ETCD_BIN"; \
	if [ -z "$$bin" ]; then \
		bin=$$(command -v etcd) || \
		bin=$$(bash integtest/fetch_etcd.sh path) || { \
			echo "make test: no etcd binary: set ETCD_BIN, put etcd on" \
				"the PATH, or run 'make fetch-etcd' to fetch it into" \
				"integtest/bin/cache" >&2; \
			exit 1; \
		}; \
	fi; \
	case "$$bin" in /*) ;; *) bin="$(CURDIR)/$$bin" ;; esac; \
	echo "make test: etcd binary $$bin"; \
	ETCD_BIN="$$bin" $(GO) test ./...

# fetch-etcd fetches the etcd release the integration suites pin into
# integtest/bin/cache, where test finds it (dnv-worker.md EU7), and checks
# its digest, through fetch_etcd (integtest/fetch_etcd.sh), the suites' own
# path.
fetch-etcd:
	bash integtest/fetch_etcd.sh

# release builds the six programs for Linux x64 into one archive with a
# checksum file under _out/ (layout.md, Directory tree). VERSION is the exact
# tag on HEAD; an untagged HEAD or a tree that is not clean is refused, unless
# VERSION is given on the command line, which overrides both refusals; an
# environment VERSION is ignored. The binaries are static (cgo off), free of
# build paths (-trimpath) and stripped (-s -w); Go stamps the module version
# from the tag, so `go version -m` on each binary still prints it. The archive
# is reproducible: a fixed owner, sorted entries, the commit time of HEAD as
# every entry's time, and gzip without a timestamp, so a rebuild on the same
# commit gives the same checksum.
ifneq ($(origin VERSION),command line)
VERSION := $(shell git describe --tags --exact-match 2>/dev/null)
endif
RELEASE_DIR := dnv-$(VERSION)-linux-amd64

release:
	@if [ -z "$(VERSION)" ]; then \
		echo "make release: HEAD carries no tag; tag it, or pass VERSION=<version>" >&2; \
		exit 1; \
	fi
	@if [ "$(origin VERSION)" != "command line" ] && [ -n "$$(git status --porcelain)" ]; then \
		echo "make release: the tree is not clean; commit or stash, or pass VERSION=<version>" >&2; \
		exit 1; \
	fi
	rm -rf _out/$(RELEASE_DIR) _out/$(RELEASE_DIR).tar.gz _out/SHA256SUMS
	mkdir -p _out/$(RELEASE_DIR)
	@for c in $(CMDS); do \
		echo "  build _out/$(RELEASE_DIR)/$$c"; \
		CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags '-s -w' \
			-o _out/$(RELEASE_DIR)/$$c ./cmd/$$c || exit 1; \
	done
	cp LICENSE _out/$(RELEASE_DIR)/
	cp -r deploy _out/$(RELEASE_DIR)/
	tar -C _out --sort=name --owner=0 --group=0 --numeric-owner \
		--mtime=@$$(git log -1 --format=%ct) -cf - $(RELEASE_DIR) | gzip -n > _out/$(RELEASE_DIR).tar.gz
	cd _out && sha256sum $(RELEASE_DIR).tar.gz > SHA256SUMS
	@echo "release: _out/$(RELEASE_DIR).tar.gz and _out/SHA256SUMS"

clean:
	rm -rf bin _out
