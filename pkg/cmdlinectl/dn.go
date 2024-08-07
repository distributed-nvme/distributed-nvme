package cmdlinectl

import (
	"github.com/spf13/cobra"
	"strings"

	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplane"
)

type dnCreateArgsStruct struct {
	grpcTarget string
	devPath    string
	trType     string
	adrFam     string
	trAddr     string
	trSvcId    string
	online     bool
	tags       string
}

type dnDeleteArgsStruct struct {
	dnId string
}

type dnGetArgsStruct struct {
	grpcTarget string
	dnId       string
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

	dnDeleteCmd = &cobra.Command{
		Use:  "delete",
		Args: cobra.MaximumNArgs(0),
		Run:  dnDeleteFunc,
	}
	dnDeleteArgs = &dnDeleteArgsStruct{}

	dnGetCmd = &cobra.Command{
		Use:  "get",
		Args: cobra.MaximumNArgs(0),
		Run:  dnGetFunc,
	}
	dnGetArgs = &dnGetArgsStruct{}
)

func init() {
	dnCreateCmd.Flags().StringVarP(
		&dnCreateArgs.grpcTarget, "grpc-target", "", "", "grpc target",
	)
	dnCreateCmd.MarkFlagRequired("grpc-target")

	dnCreateCmd.Flags().StringVarP(
		&dnCreateArgs.devPath, "dev-path", "", "", "dev path",
	)
	dnCreateCmd.MarkFlagRequired("dev-path")

	dnCreateCmd.Flags().StringVarP(
		&dnCreateArgs.trType, "tr-type", "", "tpc", "tr type",
	)

	dnCreateCmd.Flags().StringVarP(
		&dnCreateArgs.adrFam, "adr-fam", "", "ipv4", "adr fam",
	)

	dnCreateCmd.Flags().StringVarP(
		&dnCreateArgs.trAddr, "tr-addr", "", "127.0.0.1", "tr addr",
	)

	dnCreateCmd.Flags().StringVarP(
		&dnCreateArgs.trSvcId, "tr-svc-id", "", "4420", "tr svc id",
	)

	dnCreateCmd.Flags().BoolVarP(
		&dnCreateArgs.online, "online", "", true, "online",
	)

	dnCreateCmd.Flags().StringVarP(
		&dnCreateArgs.tags, "tags", "", "", "tags",
	)

	dnCmd.AddCommand(dnCreateCmd)

	dnDeleteCmd.Flags().StringVarP(
		&dnDeleteArgs.dnId, "dn-id", "", "", "dn id",
	)
	dnDeleteCmd.MarkFlagRequired("dn-id")

	dnCmd.AddCommand(dnDeleteCmd)

	dnGetCmd.Flags().StringVarP(
		&dnGetArgs.grpcTarget, "grpc-target", "", "", "grpc target",
	)

	dnGetCmd.Flags().StringVarP(
		&dnGetArgs.dnId, "dn-id", "", "", "dn id",
	)

	dnCmd.AddCommand(dnGetCmd)
}

func (cli *client) createDn(args *dnCreateArgsStruct) string {
	tagList := make([]*pbcp.Tag, 0)
	if args.tags != "" {
		tags := strings.Split(args.tags, ",")
		tagList = make([]*pbcp.Tag, len(tags))
		for i, tag := range tags {
			kv := strings.Split(tag, ":")
			tagList[i] = &pbcp.Tag{
				Key:   kv[0],
				Value: kv[1],
			}
		}
	}
	req := &pbcp.CreateDnRequest{
		GrpcTarget: args.grpcTarget,
		DevPath:    args.devPath,
		TrType:     args.trType,
		AdrFam:     args.adrFam,
		TrAddr:     args.trAddr,
		TrSvcId:    args.trSvcId,
		Online:     args.online,
		TagList:    tagList,
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

func (cli *client) deleteDn(args *dnDeleteArgsStruct) string {
	req := &pbcp.DeleteDnRequest{
		DnId: args.dnId,
	}
	reply, err := cli.c.DeleteDn(cli.ctx, req)
	if err != nil {
		return err.Error()
	} else {
		return cli.serialize(reply)
	}
}

func dnDeleteFunc(cmd *cobra.Command, args []string) {
	cli := newClient(rootArgs)
	defer cli.close()
	output := cli.deleteDn(dnDeleteArgs)
	cli.show(output)
}

func (cli *client) getDn(args *dnGetArgsStruct) string {
	req := &pbcp.GetDnRequest{}
	if len(args.grpcTarget) > 0 {
		req.Name = &pbcp.GetDnRequest_GrpcTarget{
			GrpcTarget: args.grpcTarget,
		}
	} else {
		req.Name = &pbcp.GetDnRequest_DnId{
			DnId: args.dnId,
		}
	}
	reply, err := cli.c.GetDn(cli.ctx, req)
	if err != nil {
		return err.Error()
	} else {
		return cli.serialize(reply)
	}
}

func dnGetFunc(cmd *cobra.Command, args []string) {
	cli := newClient(rootArgs)
	defer cli.close()
	output := cli.getDn(dnGetArgs)
	cli.show(output)
}
