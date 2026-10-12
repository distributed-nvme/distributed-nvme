# dnv build entry points (layout.md, Protobuf generation; layout.md,
# `cmd/` wiring).
GO ?= go
PROTOC ?= protoc

# The cmd/ directories that already contain Go sources.
CMDS := $(sort $(patsubst cmd/%/,%,$(dir $(wildcard cmd/*/*.go))))

.PHONY: all gen fmt build vet test fetch-etcd clean

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

# test runs every package's tests. The tests of etcdutil, model, gateway and
# worker run against a real etcd (dnv-worker.md EU7). An ETCD_BIN that is not
# empty is passed on unchecked, a relative path taken from this directory,
# since each test binary runs in its own package's directory; the test
# binaries refuse one that names no file. With ETCD_BIN empty or unset the
# target takes etcd on the PATH, else the one `make fetch-etcd` leaves in
# integtest/bin/cache, and with neither it fails before go test runs.
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

clean:
	rm -rf bin
