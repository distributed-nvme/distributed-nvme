OUT_DIR := ./_out
GOPATH := $(shell go env GOPATH)
PATH := $(PATH):$(GOPATH)/bin

.PHONY: proto
proto:
	protoc --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative pkg/proto/nodeagent/nodeagent.proto
	protoc --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative pkg/proto/controlplane/controlplane.proto

.PHONY: clean
clean:
	rm -f pkg/proto/nodeagent/*.go
	rm -f pkg/proto/controlplane/*.go
	rm -rf $(OUT_DIR)

.PHONY: compile
compile:
	mkdir -p $(OUT_DIR)/linux_amd64/
	env GOOS=linux GOARCH=amd64 go build -o $(OUT_DIR)/linux_amd64/dnvapi ./cmd/dnvapi
	env GOOS=linux GOARCH=amd64 go build -o $(OUT_DIR)/linux_amd64/dnvworker ./cmd/dnvworker
	env GOOS=linux GOARCH=amd64 go build -o $(OUT_DIR)/linux_amd64/dnvagent ./cmd/dnvagent
	env GOOS=linux GOARCH=amd64 go build -o $(OUT_DIR)/linux_amd64/dnvctl ./cmd/dnvctl
	cp ./bin/linux_amd64/nvme $(OUT_DIR)/linux_amd64/nvme

.PHONY: fmt
fmt:
	@gofmt -s -w .

.PHONY: check
check:
	@test -z $(shell gofmt -l . | tee /dev/stderr) || echo "[WARN] Fix formatting issues with 'make fmt'"

.PHONY: build
build: proto compile check

.DEFAULT_GOAL := build
