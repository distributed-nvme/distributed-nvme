package cmdlinectl

import (
	"github.com/spf13/cobra"

	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplane"
)

type dnCreateArgsStruct struct {
	grpcTarget string
}

var (
	dnCmd = &cobra.Command{
		Use: "dn",
	}

	dnCreateCmd = &cobra.Command{
		Use:  "create",
		Args: cobra.MaximumNArgs(0),
		Run:  dnCreateFunc,
	}
	dnCreateArgs = &dnCreateArgsStruct{}
)

func init() {
	dnCreateCmd.Flags().StringVarP(
		&dnCreateArgs.grpcTarget, "grpc-target", "", "", "grpc target",
	)
	dnCreateCmd.MarkFlagRequired("grpc-target")
	dnCmd.AddCommand(dnCreateCmd)
}

func (cli *client) createDn(args *dnCreateArgsStruct) string {
	req := &pbcp.CreateDnRequest{
		GrpcTarget: args.grpcTarget,
	}
	reply, err := cli.c.CreateDn(cli.ctx, req)
	if err != nil {
		return err.Error()
	} else {
		return cli.serialize(reply)
	}
}

func dnCreateFunc(cmd *cobra.Command, args []string) {
	cli := newClient(rootArgs)
	defer cli.close()
	output := cli.createDn(dnCreateArgs)
	cli.show(output)
}
