// This file holds the volume-shaped subcommands of gatewayctl: the thin
// devices of architecture.md §8.7, the subsystems and namespaces of §8.8 and
// the transfers of §8.10 (gateway.md §5.6, §5.7, §5.9, driver table §10.8).
//
// Every RPC in the group is SP-scoped and pure etcd, so each subcommand is the
// same shape: `--sp` names the storage pool, every mutator carries the `--rev`
// token the gateway compares against the stored SpRev (GW6), and the readers
// carry neither. `--rev` is always explicit and 0 is a legal value the B4
// stage sends on purpose, so it is never defaulted from a prior read.
//
// The names here are the §10.8 spellings, which are shorter than the proto
// field names they fill: `--name` is td_name / xfer_name, `--idx` is ns_idx and
// `--td` is td_name, because within a subcommand there is only ever one of
// each and the script types them constantly.
package main

import (
	"context"
	"flag"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// td — architecture.md §8.7
// ---------------------------------------------------------------------------

// setupCreateTd drives CreateThinDevice.
//
// --ori is what makes the call a SNAPSHOT rather than a fresh device: it names
// an existing thin device of the same SP, and it is also the only way --size
// may stay 0, because a snapshot inherits the origin's size ([D-H]). It is
// left empty by default so the plain case sends the "no origin" sentinel.
func setupCreateTd(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name that owns the thin device")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev token")
	tdName := fs.String("name", "", "td_name")
	oriName := fs.String("ori", "",
		"ori_name: snapshot this existing thin device instead of "+
			"creating a fresh one")
	size := fs.Uint64("size", 0,
		"size in bytes; may be 0 only together with --ori")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.CreateThinDevice(ctx, &pb.CreateThinDeviceRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
			SpRev:       &pb.SpRev{Revision: uint64(rev)},
			TdName:      *tdName,
			OriName:     *oriName,
			Size:        *size,
		})
	}
}

// setupDeleteTd drives DeleteThinDevice.
func setupDeleteTd(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name that owns the thin device")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev token")
	tdName := fs.String("name", "", "td_name")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.DeleteThinDevice(ctx, &pb.DeleteThinDeviceRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
			SpRev:       &pb.SpRev{Revision: uint64(rev)},
			TdName:      *tdName,
		})
	}
}

// setupListTds drives ListThinDevices, the read the §10.11 checks poll for
// `created`: the gateway always writes it false and only the sp-worker flips
// it, so this is how the script learns an origin may be snapshotted. It never
// bumps, hence no --rev.
func setupListTds(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name whose thin devices are listed")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.ListThinDevices(ctx, &pb.ListThinDevicesRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
		})
	}
}

// ---------------------------------------------------------------------------
// ss / ns — architecture.md §8.8
// ---------------------------------------------------------------------------

// setupCreateSs drives CreateSubsystem. --hosts is allowed_hosts, the host
// NQNs that may connect; it is optional because a subsystem with no host is
// legal and is what the suite creates before it grants access.
func setupCreateSs(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name that owns the subsystem")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev token")
	nqn := fs.String("nqn", "", "subsystem nqn")
	var hosts stringList
	fs.Var(&hosts, "hosts", "allowed_hosts, comma-separated host nqns")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.CreateSubsystem(ctx, &pb.CreateSubsystemRequest{
			ClusterName:  g.cluster,
			SpName:       *spName,
			SpRev:        &pb.SpRev{Revision: uint64(rev)},
			Nqn:          *nqn,
			AllowedHosts: hosts,
		})
	}
}

// setupDeleteSs drives DeleteSubsystem.
func setupDeleteSs(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name that owns the subsystem")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev token")
	nqn := fs.String("nqn", "", "subsystem nqn")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.DeleteSubsystem(ctx, &pb.DeleteSubsystemRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
			SpRev:       &pb.SpRev{Revision: uint64(rev)},
			Nqn:         *nqn,
		})
	}
}

// setupListSss drives ListSubsystems, the read-back that carries the
// namespaces too — a namespace is a field of its subsystem, not a key of its
// own, so this is the only gateway read that shows one.
func setupListSss(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name whose subsystems are listed")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.ListSubsystems(ctx, &pb.ListSubsystemsRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
		})
	}
}

// setupSetSsHosts drives UpdateSubsystemHosts. --hosts is the FULL replacement
// list, matching the stringList flag's replace-on-Set semantics: an empty
// --hosts= revokes every host rather than leaving the stored list alone.
func setupSetSsHosts(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name that owns the subsystem")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev token")
	nqn := fs.String("nqn", "", "subsystem nqn")
	var hosts stringList
	fs.Var(&hosts, "hosts",
		"allowed_hosts, comma-separated host nqns — the whole new list")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.UpdateSubsystemHosts(
			ctx, &pb.UpdateSubsystemHostsRequest{
				ClusterName:  g.cluster,
				SpName:       *spName,
				SpRev:        &pb.SpRev{Revision: uint64(rev)},
				Nqn:          *nqn,
				AllowedHosts: hosts,
			})
	}
}

// setupCreateNs drives CreateNamespace.
//
// --idx is the NVMe NSID the host will see and is the user's to choose, so it
// has no useful default; 0 is reserved and the gateway refuses it, which is
// what the §10 invalid-argument stage sends.
//
// --uuid and --nguid stay empty by default because an empty dev_uuid /
// dev_nguid is the documented "mint one" request (§8.8 Defaults) and the
// §10.11 check asserts the generated forms; passing them explicitly is how the
// cross-SP cases give two SPs' namespaces the SAME identity.
//
// --suspended creates the namespace already retired. It is a bool, so it must
// be written --suspended / --suspended=false and never `--suspended false`.
func setupCreateNs(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name that owns the subsystem")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev token")
	nqn := fs.String("nqn", "", "subsystem nqn the namespace lives in")
	nsIdx := fs.Uint("idx", 0, "ns_idx, the host-visible NSID")
	tdName := fs.String("td", "", "td_name the namespace exports")
	devUuid := fs.String("uuid", "",
		"dev_uuid; empty asks the gateway to mint one")
	devNguid := fs.String("nguid", "",
		"dev_nguid; empty asks the gateway to mint one")
	suspended := fs.Bool("suspended", false,
		"create the namespace suspended (inaccessible ANA group)")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.CreateNamespace(ctx, &pb.CreateNamespaceRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
			SpRev:       &pb.SpRev{Revision: uint64(rev)},
			Nqn:         *nqn,
			NsIdx:       uint32(*nsIdx),
			DevUuid:     *devUuid,
			DevNguid:    *devNguid,
			Suspended:   *suspended,
			TdName:      *tdName,
		})
	}
}

// setupDeleteNs drives DeleteNamespace. The namespace is addressed by its
// subsystem's --nqn plus --idx, the same pair every namespace RPC uses.
func setupDeleteNs(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name that owns the subsystem")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev token")
	nqn := fs.String("nqn", "", "subsystem nqn the namespace lives in")
	nsIdx := fs.Uint("idx", 0, "ns_idx, the host-visible NSID")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.DeleteNamespace(ctx, &pb.DeleteNamespaceRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
			SpRev:       &pb.SpRev{Revision: uint64(rev)},
			Nqn:         *nqn,
			NsIdx:       uint32(*nsIdx),
		})
	}
}

// setupSetNsDev drives UpdateNamespaceDev: it repoints a live namespace at
// another thin device by name, which the gateway resolves to a td_id before
// storing it.
func setupSetNsDev(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name that owns the subsystem")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev token")
	nqn := fs.String("nqn", "", "subsystem nqn the namespace lives in")
	nsIdx := fs.Uint("idx", 0, "ns_idx, the host-visible NSID")
	tdName := fs.String("td", "", "td_name the namespace now exports")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.UpdateNamespaceDev(ctx, &pb.UpdateNamespaceDevRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
			SpRev:       &pb.SpRev{Revision: uint64(rev)},
			Nqn:         *nqn,
			NsIdx:       uint32(*nsIdx),
			TdName:      *tdName,
		})
	}
}

// setupSetNsSuspended drives UpdateNamespaceSuspended, the retire/resume flip
// of the §11.3 choreography.
//
// --suspended defaults to TRUE because retiring is the direction the suite
// asks for; resuming is the explicit --suspended=false, which is the only
// spelling Go's flag package accepts for a false bool. The RPC writes and
// bumps even when the stored flag already matches, so re-sending the same
// value is a deliberate way to produce a bump.
func setupSetNsSuspended(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name that owns the subsystem")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev token")
	nqn := fs.String("nqn", "", "subsystem nqn the namespace lives in")
	nsIdx := fs.Uint("idx", 0, "ns_idx, the host-visible NSID")
	suspended := fs.Bool("suspended", true,
		"the new suspended flag; pass --suspended=false to resume")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.UpdateNamespaceSuspended(
			ctx, &pb.UpdateNamespaceSuspendedRequest{
				ClusterName: g.cluster,
				SpName:      *spName,
				SpRev:       &pb.SpRev{Revision: uint64(rev)},
				Nqn:         *nqn,
				NsIdx:       uint32(*nsIdx),
				Suspended:   *suspended,
			})
	}
}

// ---------------------------------------------------------------------------
// xfer — architecture.md §8.10
// ---------------------------------------------------------------------------

// setupCreateXfer drives CreateTransfer, the source half of the §11.3 cross-SP
// live migration.
//
// --ori-nqn and --ori-idx point at an existing namespace of THIS SP, the one
// whose bytes the destination clone will read; both are resolved inside the
// transaction, so a wrong pair is NOT_FOUND rather than a stored dangling
// pointer.
//
// --hosts is allowed_hosts, which here carries the DESTINATION cntlrs' host
// NQNs — the xfer subsystem is reached directly and never advertised through a
// CdcEntry.
//
// --auto-suspend asks the primary cntlr to retire the origin namespace itself
// as it builds the transfer stack, instead of the operator sending a separate
// set-ns-suspended.
func setupCreateXfer(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name that owns the transfer")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev token")
	xferName := fs.String("name", "", "xfer_name")
	oriNqn := fs.String("ori-nqn", "", "ori_nqn, the origin subsystem")
	oriNsIdx := fs.Uint("ori-idx", 0, "ori_ns_idx, the origin ns_idx")
	var hosts stringList
	fs.Var(&hosts, "hosts",
		"allowed_hosts, comma-separated destination cntlr host nqns")
	autoSuspend := fs.Bool("auto-suspend", false,
		"suspend the origin namespace as part of building the transfer")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.CreateTransfer(ctx, &pb.CreateTransferRequest{
			ClusterName:  g.cluster,
			SpName:       *spName,
			SpRev:        &pb.SpRev{Revision: uint64(rev)},
			XferName:     *xferName,
			OriNqn:       *oriNqn,
			OriNsIdx:     uint32(*oriNsIdx),
			AllowedHosts: hosts,
			AutoSuspend:  *autoSuspend,
		})
	}
}

// setupDeleteXfer drives DeleteTransfer.
//
// --force is the abort path: the default finalize path suspends the origin
// namespace on the way out, because the destination now owns the data, while
// --force leaves the origin exactly as it is so the source SP can go back to
// serving it.
func setupDeleteXfer(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name that owns the transfer")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev token")
	xferName := fs.String("name", "", "xfer_name")
	force := fs.Bool("force", false,
		"abort path: delete without suspending the origin namespace")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.DeleteTransfer(ctx, &pb.DeleteTransferRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
			SpRev:       &pb.SpRev{Revision: uint64(rev)},
			XferName:    *xferName,
			Force:       *force,
		})
	}
}

// setupGetXfer drives GetTransfer, the read-back of one transfer record. It
// never bumps, hence no --rev.
func setupGetXfer(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name that owns the transfer")
	xferName := fs.String("name", "", "xfer_name")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.GetTransfer(ctx, &pb.GetTransferRequest{
			ClusterName: g.cluster,
			SpName:      *spName,
			XferName:    *xferName,
		})
	}
}

// setupSetXferHosts drives UpdateTransferHosts. Like --hosts everywhere else
// in this file, the value REPLACES the stored allowed_hosts rather than adding
// to it.
func setupSetXferHosts(fs *flag.FlagSet) job {
	spName := fs.String("sp", "", "sp_name that owns the transfer")
	var rev hexUint
	fs.Var(&rev, "rev", "sp_rev token")
	xferName := fs.String("name", "", "xfer_name")
	var hosts stringList
	fs.Var(&hosts, "hosts",
		"allowed_hosts, comma-separated host nqns — the whole new list")
	return func(
		ctx context.Context,
		client pb.GatewayClient,
		g *globals,
	) (any, error) {
		return client.UpdateTransferHosts(
			ctx, &pb.UpdateTransferHostsRequest{
				ClusterName:  g.cluster,
				SpName:       *spName,
				SpRev:        &pb.SpRev{Revision: uint64(rev)},
				XferName:     *xferName,
				AllowedHosts: hosts,
			})
	}
}
