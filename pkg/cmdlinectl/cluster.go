package cmdlinectl

import (
	"github.com/spf13/cobra"

	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplane"
)

type clusterCreateArgsStruct struct {
}

type clusterDeleteArgsStruct struct {
}

type clusterGetArgsStruct struct {
}

var (
	clusterCmd = &cobra.Command{
		Use: "cluster",
	}

	clusterCreateCmd = &cobra.Command{
		Use:  "create",
		Args: cobra.MaximumNArgs(0),
		Run:  clusterCreateFunc,
	}
	clusterCreateArgs = &clusterCreateArgsStruct{}

	clusterDeleteCmd = &cobra.Command{
		Use:  "delete",
		Args: cobra.MaximumNArgs(0),
		Run:  clusterDeleteFunc,
	}
	clusterDeleteArgs = &clusterDeleteArgsStruct{}

	clusterGetCmd = &cobra.Command{
		Use:  "get",
		Args: cobra.MaximumNArgs(0),
		Run:  clusterGetFunc,
	}
	clusterGetArgs = &clusterGetArgsStruct{}
)

func init() {
	clusterCmd.AddCommand(clusterCreateCmd)
	clusterCmd.AddCommand(clusterDeleteCmd)
	clusterCmd.AddCommand(clusterGetCmd)
}

func (cli *client) createCluster(args *clusterCreateArgsStruct) string {
	req := &pbcp.CreateClusterRequest{}
	reply, err := cli.c.CreateCluster(cli.ctx, req)
	if err != nil {
		return err.Error()
	} else {
		return cli.serialize(reply)
	}
}

func clusterCreateFunc(cmd *cobra.Command, args []string) {
	cli := newClient(rootArgs)
	defer cli.close()
	output := cli.createCluster(clusterCreateArgs)
	cli.show(output)
}

func (cli *client) deleteCluster(args *clusterDeleteArgsStruct) string {
	req := &pbcp.DeleteClusterRequest{}
	reply, err := cli.c.DeleteCluster(cli.ctx, req)
	if err != nil {
		return err.Error()
	} else {
		return cli.serialize(reply)
	}
}

func clusterDeleteFunc(cmd *cobra.Command, args []string) {
	cli := newClient(rootArgs)
	defer cli.close()
	output := cli.deleteCluster(clusterDeleteArgs)
	cli.show(output)
}

func (cli *client) getCluster(args *clusterGetArgsStruct) string {
	req := &pbcp.GetClusterRequest{}
	reply, err := cli.c.GetCluster(cli.ctx, req)
	if err != nil {
		return err.Error()
	} else {
		return cli.serialize(reply)
	}
}

func clusterGetFunc(cmd *cobra.Command, args []string) {
	cli := newClient(rootArgs)
	defer cli.close()
	output := cli.getCluster(clusterGetArgs)
	cli.show(output)
}
