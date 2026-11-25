# dnv build entry points (layout.md, Protobuf generation; layout.md,
# `cmd/` wiring; the release archive, layout.md, Directory tree).
GO ?= go
PROTOC ?= protoc

# The cmd/ directories that already contain Go sources.
CMDS := $(sort $(patsubst cmd/%/,%,$(dir $(wildcard cmd/*/*.go))))

.PHONY: all gen fmt build vet test test-race fetch-etcd release clean

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
# go test runs. test-race picks the etcd binary the same way.
ETCD_PICK = bin="$$ETCD_BIN"; \
	if [ -z "$$bin" ]; then \
		bin=$$(command -v etcd) || \
		bin=$$(bash integtest/fetch_etcd.sh path) || { \
			echo "make $@: no etcd binary: set ETCD_BIN, put etcd on" \
				"the PATH, or run 'make fetch-etcd' to fetch it into" \
				"integtest/bin/cache" >&2; \
			exit 1; \
		}; \
	fi; \
	case "$$bin" in /*) ;; *) bin="$(CURDIR)/$$bin" ;; esac; \
	echo "make $@: etcd binary $$bin"

test:
	@$(ETCD_PICK); \
	ETCD_BIN="$$bin" $(GO) test ./...

# test-race runs the short tests (go test -short skips the full-shape tests
# and the slowest timing tests) of every package under the race detector,
# which needs cgo and so a C compiler on the machine. The detector
# multiplies the memory of each test binary, so RACE_P packages run at once
# and RACE_PARALLEL tests at once within a package; a machine with less
# memory lowers them. A test that fails now and then under it has its line
# in integtest/flakes.txt (layout.md, Directory tree). The hosted CI does not
# run this target: its image has no C compiler, and the target's memory was
# measured only on a laptop under a 12 GiB cap.
RACE_P ?= 4
RACE_PARALLEL ?= 2

test-race:
	@$(ETCD_PICK); \
	ETCD_BIN="$$bin" $(GO) test -race -short -p $(RACE_P) -parallel $(RACE_PARALLEL) ./...

# fetch-etcd fetches the etcd release the integration suites pin into
# integtest/bin/cache, where test finds it (dnv-worker.md EU7), and checks
# its digest, through fetch_etcd (integtest/fetch_etcd.sh), the suites' own
# path.
fetch-etcd:
	bash integtest/fetch_etcd.sh

# release builds the six programs for Linux x64 into one archive with a
# checksum file under RELEASE_OUT, _out/ by default (layout.md, Directory
# tree). VERSION is the exact
# tag on HEAD; an untagged HEAD or a tree that is not clean is refused, unless
# VERSION is given on the command line, which overrides both refusals; an
# environment VERSION is ignored. The CSI driver's manifests in the
# archive's deploy/ name its image by the version, a leading v dropped, so
# that a release names its own image (csi.md, Deployment); the tree's
# manifests keep the tag the csi suite builds. The binaries are static (cgo off), free of
# build paths (-trimpath) and stripped (-s -w); Go stamps the module version
# from the tag, so `go version -m` on each binary still prints it. Go takes
# the version from the nearest directory, at or above the tree, whose .git
# is a directory: a checkout stamps its own commit, but a git worktree, whose
# .git is a file, would stamp the commit of the checkout whose folder holds
# it, or nothing, so the target refuses a tree whose .git is not a
# directory, whatever VERSION says. The archive is
# reproducible: a fixed owner, fixed modes (0644 for a file, 0755 for a
# directory or a program, whatever the builder's umask, as long as it leaves
# the owner read, write and execute, and with no setuid or setgid bit), sorted
# entries, the commit time of HEAD as every entry's time, and gzip without a
# timestamp, so a rebuild on the same commit with the same history and tags,
# the same Go and Go settings, tar and gzip, and no file git ignores under
# deploy/, gives the same checksum. Once a release is tagged, its schema's layout joins
# pb/testdata/<tag>/ (layout.md, Protobuf generation).
ifneq ($(origin VERSION),command line)
VERSION := $(shell git describe --tags --exact-match 2>/dev/null)
endif
RELEASE_DIR := dnv-$(VERSION)-linux-amd64
RELEASE_OUT ?= _out
# IMAGE_TAG is the tag the release's CSI manifests give the driver's image:
# the release's version, with the v of a version such as v0.2.0 dropped,
# as docker tags are written; the release step refuses a tag docker would
# not take.
IMAGE_TAG := $(if $(filter v0% v1% v2% v3% v4% v5% v6% v7% v8% v9%,$(VERSION)),$(patsubst v%,%,$(VERSION)),$(VERSION))

release:
	@if [ ! -d "$(CURDIR)/.git" ]; then \
		echo "make release: $(CURDIR)/.git is not a directory (a git worktree, a submodule or a tree with no git), so the binaries would carry another checkout's commit or none; run make release in a clone" >&2; \
		exit 1; \
	fi
	@if [ -z "$(VERSION)" ]; then \
		echo "make release: HEAD carries no tag; tag it, or pass VERSION=<version>" >&2; \
		exit 1; \
	fi
	@if [ "$(origin VERSION)" != "command line" ] && [ -n "$$(git status --porcelain)" ]; then \
		echo "make release: the tree is not clean; commit or stash, or pass VERSION=<version>" >&2; \
		exit 1; \
	fi
	rm -rf $(RELEASE_OUT)/$(RELEASE_DIR) $(RELEASE_OUT)/$(RELEASE_DIR).tar.gz $(RELEASE_OUT)/SHA256SUMS
	mkdir -p $(RELEASE_OUT)/$(RELEASE_DIR)
	@for c in $(CMDS); do \
		echo "  build $(RELEASE_OUT)/$(RELEASE_DIR)/$$c"; \
		CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags '-s -w' \
			-o $(RELEASE_OUT)/$(RELEASE_DIR)/$$c ./cmd/$$c || exit 1; \
	done
	cp LICENSE $(RELEASE_OUT)/$(RELEASE_DIR)/
	cp -r deploy $(RELEASE_OUT)/$(RELEASE_DIR)/
	@printf '%s\n' '$(IMAGE_TAG)' | grep -Eqx '[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}' || { \
		echo "make release: '$(IMAGE_TAG)', from VERSION $(VERSION), is no docker tag" >&2; exit 1; }
	@m=$$(grep -l 'dnv-csi:' $(RELEASE_OUT)/$(RELEASE_DIR)/deploy/*.yaml); \
	[ -n "$$m" ] || { echo "make release: no manifest of deploy/ names the image dnv-csi" >&2; exit 1; }; \
	for f in $$m; do \
		grep -q '^ *image: dnv-csi:' $$f || { \
			echo "make release: $$f names dnv-csi but in no plain image line" >&2; exit 1; }; \
		sed -i 's|^\( *image: dnv-csi:\).*|\1$(IMAGE_TAG)|' $$f || exit 1; \
	done
	tar -C $(RELEASE_OUT) --sort=name --owner=0 --group=0 --numeric-owner \
		--mode=go=rX,u+rw,a-st --mtime=@$$(git log -1 --format=%ct) \
		-cf - $(RELEASE_DIR) | gzip -n > $(RELEASE_OUT)/$(RELEASE_DIR).tar.gz
	cd $(RELEASE_OUT) && sha256sum $(RELEASE_DIR).tar.gz > SHA256SUMS
	@echo "release: $(RELEASE_OUT)/$(RELEASE_DIR).tar.gz and $(RELEASE_OUT)/SHA256SUMS"

clean:
	rm -rf bin _out
