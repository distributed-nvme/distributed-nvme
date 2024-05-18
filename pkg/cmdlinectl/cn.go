package cmdlinectl

import (
	"github.com/spf13/cobra"
	"strings"

	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplane"
)

type cnCreateArgsStruct struct {
	grpcTarget string
	trType     string
	adrFam     string
	trAddr     string
	trSvcId    string
	online     bool
	tags       string
}

type cnDeleteArgsStruct struct {
	cnId string
}

type cnGetArgsStruct struct {
	grpcTarget string
	cnId       string
}

var (
	cnCmd = &cobra.Command{
		Use: "cn",
	}

	cnCreateCmd = &cobra.Command{
		Use:  "create",
		Args: cobra.MaximumNArgs(0),
		Run:  cnCreateFunc,
	}
	cnCreateArgs = &cnCreateArgsStruct{}

	cnDeleteCmd = &cobra.Command{
		Use:  "delete",
		Args: cobra.MaximumNArgs(0),
		Run:  cnDeleteFunc,
	}
	cnDeleteArgs = &cnDeleteArgsStruct{}

	cnGetCmd = &cobra.Command{
		Use:  "get",
		Args: cobra.MaximumNArgs(0),
		Run:  cnGetFunc,
	}
	cnGetArgs = &cnGetArgsStruct{}
)

func init() {
	cnCreateCmd.Flags().StringVarP(
		&cnCreateArgs.grpcTarget, "grpc-target", "", "", "grpc target",
	)
	cnCreateCmd.MarkFlagRequired("grpc-target")

	cnCreateCmd.Flags().StringVarP(
		&cnCreateArgs.trType, "tr-type", "", "tcp", "tr type",
	)

	cnCreateCmd.Flags().StringVarP(
		&cnCreateArgs.adrFam, "adr-fam", "", "ipv4", "ard fam",
	)

	cnCreateCmd.Flags().StringVarP(
		&cnCreateArgs.trAddr, "tr-addr", "", "127.0.0.1", "tr addr",
	)

	cnCreateCmd.Flags().StringVarP(
		&cnCreateArgs.trSvcId, "tr-svc-id", "", "4430", "tr svc id",
	)

	cnCreateCmd.Flags().BoolVarP(
		&cnCreateArgs.online, "online", "", true, "online",
	)

	cnCreateCmd.Flags().StringVarP(
		&cnCreateArgs.tags, "tags", "", "", "tags",
	)

	cnCmd.AddCommand(cnCreateCmd)

	cnDeleteCmd.Flags().StringVarP(
		&cnDeleteArgs.cnId, "cn-id", "", "", "cn id",
	)
	cnDeleteCmd.MarkFlagRequired("cn-id")

	cnCmd.AddCommand(cnDeleteCmd)

	cnGetCmd.Flags().StringVarP(
		&cnGetArgs.grpcTarget, "grpc-target", "", "", "grpc target",
	)

	cnGetCmd.Flags().StringVarP(
		&cnGetArgs.cnId, "cn-id", "", "", "cn id",
	)

	cnCmd.AddCommand(cnGetCmd)
}

func (cli *client) createCn(args *cnCreateArgsStruct) string {
	tags := strings.Split(args.tags, ",")
	tagList := make([]*pbcp.Tag, len(tags))
	for i, tag := range tags {
		kv := strings.Split(tag, ":")
		tagList[i] = &pbcp.Tag{
			Key:   kv[0],
			Value: kv[1],
		}
	}
	req := &pbcp.CreateCnRequest{
		GrpcTarget: args.grpcTarget,
		TrType:     args.trType,
		AdrFam:     args.adrFam,
		TrAddr:     args.trAddr,
		TrSvcId:    args.trSvcId,
		Online:     args.online,
		TagList:    tagList,
	}
	reply, err := cli.c.CreateCn(cli.ctx, req)
	if err != nil {
		return err.Error()
	} else {
		return cli.serialize(reply)
	}
}

func cnCreateFunc(cmd *cobra.Command, args []string) {
	cli := newClient(rootArgs)
	defer cli.close()
	output := cli.createCn(cnCreateArgs)
	cli.show(output)
}

func (cli *client) deleteCn(args *cnDeleteArgsStruct) string {
	req := &pbcp.DeleteCnRequest{
		CnId: args.cnId,
	}
	reply, err := cli.c.DeleteCn(cli.ctx, req)
	if err != nil {
		return err.Error()
	} else {
		return cli.serialize(reply)
	}
}

func cnDeleteFunc(cmd *cobra.Command, args []string) {
	cli := newClient(rootArgs)
	defer cli.close()
	output := cli.deleteCn(cnDeleteArgs)
	cli.show(output)
}

func (cli *client) getCn(args *cnGetArgsStruct) string {
	req := &pbcp.GetCnRequest{}
	if len(args.grpcTarget) > 0 {
		req.Name = &pbcp.GetCnRequest_GrpcTarget{
			GrpcTarget: args.grpcTarget,
		}
	} else {
		req.Name = &pbcp.GetCnRequest_CnId{
			CnId: args.cnId,
		}
	}
	reply, err := cli.c.GetCn(cli.ctx, req)
	if err != nil {
		return err.Error()
	} else {
		return cli.serialize(reply)
	}
}

func cnGetFunc(cmd *cobra.Command, args []string) {
	cli := newClient(rootArgs)
	defer cli.close()
	output := cli.getCn(cnGetArgs)
	cli.show(output)
}
