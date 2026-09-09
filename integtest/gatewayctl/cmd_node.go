// cmd_node.go holds the subcommands of the three node-shaped scopes of
// §10.8 — the cluster (§8.1), the disk node (§8.2) and the controller node
// (§8.3) — plus the `ping` liveness probe of §10.7 step 8.
//
// The DN and CN halves are exact mirrors: the two proto message families
// differ only in the name and in which revision token they carry (DnRev vs
// CnRev), so the shared flag registration lives in the two node* helpers at
// the top of the file and each subcommand only names its own RPC and message.
// The flags a subcommand of another group also registers (--count,
// --page-token, --rev) are spelled out in place rather than factored into a
// helper, so this file declares no package-level name the sibling command
// files could collide with.
//
// Every revision-token flag here is named exactly "rev": race's stale-token
// retry rewrites the flag by that name (main.go refreshToken), so any other
// spelling would silently disable the §10.8 retry protocol. Its default of 0
// is a legal value that deliberately sends a zero token — case B step 4
// asserts the ABORTED that GW6 answers it with.
package main

import (
	"context"
	"flag"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// Flag registration shared by this file's subcommands
// ---------------------------------------------------------------------------

// clusterNameOf resolves which cluster a cluster-scoped subcommand names: the
// global --cluster, unless the subcommand's own --name overrides it. The
// override exists only for the pagination step of case S (§10.11 step 2),
// which creates, lists and deletes pg0..pg4 beside the suite's own cluster;
// every other stage leaves --name empty and works on --cluster.
func clusterNameOf(g *globals, name string) string {
	if name != "" {
		return name
	}
	return g.cluster
}

// clusterNameFlag registers the --name override described above.
func clusterNameFlag(fs *flag.FlagSet) *string {
	return fs.String("name", "",
		"cluster_name to act on instead of the global --cluster")
}

// nodeAddrFlag registers the addr_port that names a DN or a CN. It has no
// default: an omitted --addr sends an empty addr_port on purpose, so the
// script can watch the gateway refuse it with INVALID_ARGUMENT.
func nodeAddrFlag(fs *flag.FlagSet) *string {
	return fs.String("addr", "", "node addr_port as ip:port")
}

// nodeCreateFlags registers everything CreateDiskNode and
// CreateControllerNode take, which is the same set for both.
//
// --location defaults to the empty string rather than to a rack name: the
// gateway then defaults it to addr_port itself (§8.2), so a create-dn that
// only passes --addr still produces a well-formed DnConf. The four transport
// flags default through trConfFlags to the tcp/ipv4/127.0.0.1/4420 the fake
// agents of §10.3 listen on, for the same reason.
func nodeCreateFlags(fs *flag.FlagSet) (
	addr *string,
	location *string,
	disabled *bool,
	trConf func() *pb.NvmeTrConf,
) {
	addr = nodeAddrFlag(fs)
	location = fs.String("location", "",
		"failure domain; empty lets the gateway default it to addr_port")
	disabled = fs.Bool("disabled", false,
		"create the node already disabled (no capacity key)")
	trConf = trConfFlags(fs, "", "tcp", "ipv4", "127.0.0.1", "4420")
	return addr, location, disabled, trConf
}

// ---------------------------------------------------------------------------
// cluster (§8.1)
// ---------------------------------------------------------------------------

// setupCreateCluster drives CreateCluster. It registers no conf flags at all:
// every cluster this suite creates takes the gateway's pure defaults (§10.11
// step 1 asserts those defaults verbatim against etcd), so the only knob is
// --name, for the pg0..pg4 clusters of the pagination step.
func setupCreateCluster(fs *flag.FlagSet) job {
	name := clusterNameFlag(fs)
	return func(
		ctx context.Context, client pb.GatewayClient, g *globals,
	) (any, error) {
		return client.CreateCluster(ctx, &pb.CreateClusterRequest{
			ClusterName: clusterNameOf(g, *name),
		})
	}
}

// setupDeleteCluster drives DeleteCluster.
func setupDeleteCluster(fs *flag.FlagSet) job {
	name := clusterNameFlag(fs)
	return func(
		ctx context.Context, client pb.GatewayClient, g *globals,
	) (any, error) {
		return client.DeleteCluster(ctx, &pb.DeleteClusterRequest{
			ClusterName: clusterNameOf(g, *name),
		})
	}
}

// setupGetCluster drives GetCluster, the read-back of step 1: its reply
// carries the ClusterConf and the three globals the script compares with the
// raw etcd values workerctl decoded.
func setupGetCluster(fs *flag.FlagSet) job {
	name := clusterNameFlag(fs)
	return func(
		ctx context.Context, client pb.GatewayClient, g *globals,
	) (any, error) {
		return client.GetCluster(ctx, &pb.GetClusterRequest{
			ClusterName: clusterNameOf(g, *name),
		})
	}
}

// setupListClusters drives ListClusters. The request is cluster-independent —
// it is the one RPC of this file with no cluster_name field — so it takes only
// the page flags; --page-token also carries the deliberately malformed '!!'
// of §10.11 step 2.
func setupListClusters(fs *flag.FlagSet) job {
	count := fs.Uint("count", 0, "max entries per page (0 = server default)")
	pageToken := fs.String("page-token", "",
		"opaque continuation token from the previous reply")
	return func(
		ctx context.Context, client pb.GatewayClient, g *globals,
	) (any, error) {
		return client.ListClusters(ctx, &pb.ListClustersRequest{
			Count:     uint32(*count),
			PageToken: *pageToken,
		})
	}
}

// ---------------------------------------------------------------------------
// disk node (§8.2)
// ---------------------------------------------------------------------------

// setupCreateDn drives CreateDiskNode, the RPC that also makes the gateway
// call the DN agent's GetDnSize — the trace-id chain case S asserts against
// the fakeagent log.
func setupCreateDn(fs *flag.FlagSet) job {
	addr, location, disabled, trConf := nodeCreateFlags(fs)
	return func(
		ctx context.Context, client pb.GatewayClient, g *globals,
	) (any, error) {
		return client.CreateDiskNode(ctx, &pb.CreateDiskNodeRequest{
			ClusterName: g.cluster,
			AddrPort:    *addr,
			NvmeTrConf:  trConf(),
			Location:    *location,
			Disabled:    *disabled,
		})
	}
}

// setupDeleteDn drives DeleteDiskNode. The DnRev message is sent with only
// its revision set: the gateway matches the token against the stored dn_rev
// and never reads addr_port back out of it.
func setupDeleteDn(fs *flag.FlagSet) job {
	addr := nodeAddrFlag(fs)
	var rev hexUint
	fs.Var(&rev, "rev", "expected dn_rev token; 0 sends a zero token")
	return func(
		ctx context.Context, client pb.GatewayClient, g *globals,
	) (any, error) {
		return client.DeleteDiskNode(ctx, &pb.DeleteDiskNodeRequest{
			ClusterName: g.cluster,
			AddrPort:    *addr,
			DnRev:       &pb.DnRev{Revision: uint64(rev)},
		})
	}
}

// setupGetDn drives GetDiskNode; its reply carries the DnConf and the current
// dn_rev, which is where the script refreshes a DN token from.
func setupGetDn(fs *flag.FlagSet) job {
	addr := nodeAddrFlag(fs)
	return func(
		ctx context.Context, client pb.GatewayClient, g *globals,
	) (any, error) {
		return client.GetDiskNode(ctx, &pb.GetDiskNodeRequest{
			ClusterName: g.cluster,
			AddrPort:    *addr,
		})
	}
}

// setupListDns drives ListDiskNodes.
func setupListDns(fs *flag.FlagSet) job {
	count := fs.Uint("count", 0, "max entries per page (0 = server default)")
	pageToken := fs.String("page-token", "",
		"opaque continuation token from the previous reply")
	return func(
		ctx context.Context, client pb.GatewayClient, g *globals,
	) (any, error) {
		return client.ListDiskNodes(ctx, &pb.ListDiskNodesRequest{
			ClusterName: g.cluster,
			Count:       uint32(*count),
			PageToken:   *pageToken,
		})
	}
}

// setupSetDnDisabled drives UpdateDiskNodeDisabled. --disabled is an explicit
// value rather than a toggle, so that re-sending the state the DN already has
// exercises the §0 #17 idempotency rule (no write, no revision bump, OK).
func setupSetDnDisabled(fs *flag.FlagSet) job {
	addr := nodeAddrFlag(fs)
	var rev hexUint
	fs.Var(&rev, "rev", "expected dn_rev token; 0 sends a zero token")
	disabled := fs.Bool("disabled", false, "the disabled flag to store")
	return func(
		ctx context.Context, client pb.GatewayClient, g *globals,
	) (any, error) {
		return client.UpdateDiskNodeDisabled(
			ctx, &pb.UpdateDiskNodeDisabledRequest{
				ClusterName: g.cluster,
				AddrPort:    *addr,
				DnRev:       &pb.DnRev{Revision: uint64(rev)},
				Disabled:    *disabled,
			})
	}
}

// setupInspectDn drives InspectDiskNode, which the gateway answers by calling
// the DN agent's GetDnInfo — so this is a live-agent read, not an etcd read.
func setupInspectDn(fs *flag.FlagSet) job {
	addr := nodeAddrFlag(fs)
	return func(
		ctx context.Context, client pb.GatewayClient, g *globals,
	) (any, error) {
		return client.InspectDiskNode(ctx, &pb.InspectDiskNodeRequest{
			ClusterName: g.cluster,
			AddrPort:    *addr,
		})
	}
}

// ---------------------------------------------------------------------------
// controller node (§8.3) — the six exact mirrors of the DN subcommands
// ---------------------------------------------------------------------------

// setupCreateCn drives CreateControllerNode; like its DN mirror it makes the
// gateway call the agent, here GetCnSize.
func setupCreateCn(fs *flag.FlagSet) job {
	addr, location, disabled, trConf := nodeCreateFlags(fs)
	return func(
		ctx context.Context, client pb.GatewayClient, g *globals,
	) (any, error) {
		return client.CreateControllerNode(
			ctx, &pb.CreateControllerNodeRequest{
				ClusterName: g.cluster,
				AddrPort:    *addr,
				NvmeTrConf:  trConf(),
				Location:    *location,
				Disabled:    *disabled,
			})
	}
}

// setupDeleteCn drives DeleteControllerNode over the CnRev token.
func setupDeleteCn(fs *flag.FlagSet) job {
	addr := nodeAddrFlag(fs)
	var rev hexUint
	fs.Var(&rev, "rev", "expected cn_rev token; 0 sends a zero token")
	return func(
		ctx context.Context, client pb.GatewayClient, g *globals,
	) (any, error) {
		return client.DeleteControllerNode(
			ctx, &pb.DeleteControllerNodeRequest{
				ClusterName: g.cluster,
				AddrPort:    *addr,
				CnRev:       &pb.CnRev{Revision: uint64(rev)},
			})
	}
}

// setupGetCn drives GetControllerNode; its reply carries the CnConf and the
// current cn_rev.
func setupGetCn(fs *flag.FlagSet) job {
	addr := nodeAddrFlag(fs)
	return func(
		ctx context.Context, client pb.GatewayClient, g *globals,
	) (any, error) {
		return client.GetControllerNode(ctx, &pb.GetControllerNodeRequest{
			ClusterName: g.cluster,
			AddrPort:    *addr,
		})
	}
}

// setupListCns drives ListControllerNodes.
func setupListCns(fs *flag.FlagSet) job {
	count := fs.Uint("count", 0, "max entries per page (0 = server default)")
	pageToken := fs.String("page-token", "",
		"opaque continuation token from the previous reply")
	return func(
		ctx context.Context, client pb.GatewayClient, g *globals,
	) (any, error) {
		return client.ListControllerNodes(
			ctx, &pb.ListControllerNodesRequest{
				ClusterName: g.cluster,
				Count:       uint32(*count),
				PageToken:   *pageToken,
			})
	}
}

// setupSetCnDisabled drives UpdateControllerNodeDisabled, with the same
// explicit --disabled value as its DN mirror.
func setupSetCnDisabled(fs *flag.FlagSet) job {
	addr := nodeAddrFlag(fs)
	var rev hexUint
	fs.Var(&rev, "rev", "expected cn_rev token; 0 sends a zero token")
	disabled := fs.Bool("disabled", false, "the disabled flag to store")
	return func(
		ctx context.Context, client pb.GatewayClient, g *globals,
	) (any, error) {
		return client.UpdateControllerNodeDisabled(
			ctx, &pb.UpdateControllerNodeDisabledRequest{
				ClusterName: g.cluster,
				AddrPort:    *addr,
				CnRev:       &pb.CnRev{Revision: uint64(rev)},
				Disabled:    *disabled,
			})
	}
}

// setupInspectCn drives InspectControllerNode, answered by the CN agent's
// GetCnInfo.
func setupInspectCn(fs *flag.FlagSet) job {
	addr := nodeAddrFlag(fs)
	return func(
		ctx context.Context, client pb.GatewayClient, g *globals,
	) (any, error) {
		return client.InspectControllerNode(
			ctx, &pb.InspectControllerNodeRequest{
				ClusterName: g.cluster,
				AddrPort:    *addr,
			})
	}
}

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

// setupPing is the liveness probe of §10.7 step 8: a ListClusters with
// count 1, which touches etcd through the gateway and so proves the instance
// really serves rather than merely holding its port open. It takes no flags —
// the endpoint is the global --gateway, and the reply is thrown away by the
// caller, which only checks the exit code.
func setupPing(fs *flag.FlagSet) job {
	return func(
		ctx context.Context, client pb.GatewayClient, g *globals,
	) (any, error) {
		return client.ListClusters(ctx, &pb.ListClustersRequest{Count: 1})
	}
}
