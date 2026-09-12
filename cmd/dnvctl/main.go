// Command dnvctl is the dnv operator CLI (dnvctl.md): one command per RPC of
// `service Gateway`, printing one canonical JSON document per invocation.
//
// main is deliberately thin (layout.md §5) — the whole CLI lives in ctl/. It
// does exactly two things, in this order:
//
//  1. drops the log level to Warn (log.md R6). Every record the grpc.md §4
//     client interceptors emit is Info, so dnvctl's own gRPC logging is
//     silenced by design; what survives lands on stderr, because common's
//     init() logs there and stdout is reserved for the command result
//     (dnvctl.md CT7).
//  2. runs the command tree and exits with its code: 0 OK, 1 RPC or
//     connection failure, 2 usage error (dnvctl.md §3.2).
//
// dnvctl never links etcd (CT6): its only server-side dependency is pb plus
// common.
package main

import (
	"log/slog"
	"os"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/ctl"
)

func main() {
	common.SetLogLevel(slog.LevelWarn)
	os.Exit(ctl.Execute())
}
