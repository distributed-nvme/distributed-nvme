# Direct dependencies

Current (must match `go.mod`):

* google.golang.org/grpc
* google.golang.org/protobuf
* golang.org/x/sync
* github.com/spf13/cobra
* github.com/spf13/viper
* go.etcd.io/etcd/client/v3 v3.6.14 — the v3.6 line; entered `go.mod` with
  `etcdutil/` (`layout.md` §3, `dnv-worker.md` §3) and linked into
  `cmd/dnv-worker` only (`layout.md` §7 item 4). The worker integration suite
  runs an etcd server of the same minor.
* go.etcd.io/etcd/api/v3 — the same version; `v3rpc/rpctypes` for the
  `ErrCompacted` report of `dnv-worker.md` EU3; arrives with the client and
  cannot be avoided.
* go.uber.org/zap — only for `zap.NewNop()`, the silenced etcd client logger of
  EU1, which keeps the JSON stdout log one record per line; arrives with the
  client and cannot be avoided.
