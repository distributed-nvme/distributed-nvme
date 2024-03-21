OUT_DIR := ./_out
GOPATH := $(shell go env GOPATH)
PATH := $(PATH):$(GOPATH)/bin

.PHONY: proto
proto:
	protoc --go_out=. --go_opt=paths=source_relative pkg/proto/dataschema/dataschema.proto
	protoc --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative pkg/proto/nodeapi/nodeapi.proto
	protoc --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative pkg/proto/cpapi/cpapi.proto

.PHONY: clean
clean:
	rm -f pkg/proto/dataschema/*.go
	rm -f pkg/proto/nodeapi/*.go
	rm -f pkg/proto/cpapi/*.go
	rm -rf $(OUT_DIR)

.PHONY: compile
compile:
	mkdir -p $(OUT_DIR)/linux_amd64/
	env GOOS=linux GOARCH=amd64 go build -o $(OUT_DIR)/linux_amd64/dnv_cp ./cmd/controlplane
	env GOOS=linux GOARCH=amd64 go build -o $(OUT_DIR)/linux_amd64/dnv_cli ./cmd/cli

.PHONY: build
build: proto compile

.DEFAULT_GOAL := build
