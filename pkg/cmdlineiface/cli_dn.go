package cli

import (
	"github.com/spf13/cobra"

	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplane"
)

type dnCreateArgsStruct struct {
	sockAddr    string
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
	dnCreateCmd.Flags().StringVarP(&dnCreateArgs.sockAddr, "sock-addr", "", "",
		"dn socket address")
	dnCreateCmd.MarkFlagRequired("sock-addr")
	dnCmd.AddCommand(dnCreateCmd)
}

func (cli *client) createDn(args *dnCreateArgsStruct) string {
	req := &pbcp.CreateDnRequest{
		SockAddr:    args.sockAddr,
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
