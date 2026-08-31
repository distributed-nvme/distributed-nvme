# dnv build entry points (layout.md §4, §7).
GO ?= go
PROTOC ?= protoc

# The cmd/ directories that already contain Go sources.
CMDS := $(sort $(patsubst cmd/%/,%,$(dir $(wildcard cmd/*/*.go))))

.PHONY: all gen fmt build vet test clean

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
	gofmt -w ./common
	$(CLANG_FORMAT) -i pb/schema.proto

build:
	@for c in $(CMDS); do \
		echo "  build bin/$$c"; \
		$(GO) build -o bin/$$c ./cmd/$$c || exit 1; \
	done

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

clean:
	rm -rf bin
