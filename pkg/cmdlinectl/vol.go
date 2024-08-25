package cmdlinectl

import (
	"github.com/spf13/cobra"
	"strings"

	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplane"
)

type volCreateArgsStruct struct {
	volName          string
	cntlrCnt         uint32
	legPerCntlr      uint32
	size             uint64
	dnDistinguishKey string
	cnDistinguishKey string
	tags             string
}

type volDeleteArgsStruct struct {
	volName string
}

type volGetArgsStruct struct {
	volName string
}

type volExportArgsStruct struct {
	volName string
	hostNqn string
}

type volUnexportArgsStruct struct {
	volName string
	hostNqn string
}

var (
	volCmd = &cobra.Command{
		Use: "vol",
	}

	volCreateCmd = &cobra.Command{
		Use:  "create",
		Args: cobra.MaximumNArgs(0),
		Run:  volCreateFunc,
	}
	volCreateArgs = &volCreateArgsStruct{}

	volDeleteCmd = &cobra.Command{
		Use:  "delete",
		Args: cobra.MaximumNArgs(0),
		Run:  volDeleteFunc,
	}
	volDeleteArgs = &volDeleteArgsStruct{}

	volGetCmd = &cobra.Command{
		Use:  "get",
		Args: cobra.MaximumNArgs(0),
		Run:  volGetFunc,
	}
	volGetArgs = &volGetArgsStruct{}

	volExportCmd = &cobra.Command{
		Use:  "export",
		Args: cobra.MaximumNArgs(0),
		Run:  volExportFunc,
	}
	volExportArgs = &volExportArgsStruct{}

	volUnexportCmd = &cobra.Command{
		Use:  "unexport",
		Args: cobra.MaximumNArgs(0),
		Run:  volUnexportFunc,
	}
	volUnexportArgs = &volUnexportArgsStruct{}
)

func init() {
	volCreateCmd.Flags().StringVarP(
		&volCreateArgs.volName, "vol-name", "", "", "vol name",
	)
	volCreateCmd.MarkFlagRequired("vol-name")

	volCreateCmd.Flags().Uint32VarP(
		&volCreateArgs.cntlrCnt, "cntlr-cnt", "", 1, "cntlr cnt",
	)

	volCreateCmd.Flags().Uint32VarP(
		&volCreateArgs.legPerCntlr, "leg-per-cntlr", "", 1, "leg per cntlr",
	)

	volCreateCmd.Flags().Uint64VarP(
		&volCreateArgs.size, "size", "", 0, "size",
	)
	volCreateCmd.MarkFlagRequired("size")

	volCreateCmd.Flags().StringVarP(
		&volCreateArgs.dnDistinguishKey, "dn-distinguish-key", "", "", "dn distinguish key",
	)

	volCreateCmd.Flags().StringVarP(
		&volCreateArgs.cnDistinguishKey, "cn-distinguish-key", "", "", "cn distinguish key",
	)

	volCreateCmd.Flags().StringVarP(
		&volCreateArgs.tags, "tags", "", "", "tags",
	)

	volCmd.AddCommand(volCreateCmd)

	volDeleteCmd.Flags().StringVarP(
		&volDeleteArgs.volName, "vol-name", "", "", "vol name",
	)
	volDeleteCmd.MarkFlagRequired("vol-name")

	volCmd.AddCommand(volDeleteCmd)

	volGetCmd.Flags().StringVarP(
		&volGetArgs.volName, "vol-name", "", "", "vol name",
	)
	volGetCmd.MarkFlagRequired("vol-name")

	volCmd.AddCommand(volGetCmd)

	volExportCmd.Flags().StringVarP(
		&volExportArgs.volName, "vol-name", "", "", "vol name",
	)
	volExportCmd.MarkFlagRequired("vol-name")

	volExportCmd.Flags().StringVarP(
		&volExportArgs.hostNqn, "host-nqn", "", "", "host nqn",
	)
	volExportCmd.MarkFlagRequired("host-nqn")

	volCmd.AddCommand(volExportCmd)

	volUnexportCmd.Flags().StringVarP(
		&volUnexportArgs.volName, "vol-name", "", "", "vol name",
	)
	volUnexportCmd.MarkFlagRequired("vol-name")

	volUnexportCmd.Flags().StringVarP(
		&volUnexportArgs.hostNqn, "host-nqn", "", "", "host nqn",
	)
	volUnexportCmd.MarkFlagRequired("host-nqn")

	volCmd.AddCommand(volUnexportCmd)
}

func (cli *client) createVol(args *volCreateArgsStruct) string {
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
	req := &pbcp.CreateVolRequest{
		VolName:          args.volName,
		CntlrCnt:         args.cntlrCnt,
		LegPerCntlr:      args.legPerCntlr,
		Size:             args.size,
		DnDistinguishKey: args.dnDistinguishKey,
		CnDistinguishKey: args.cnDistinguishKey,
		TagList:          tagList,
	}
	reply, err := cli.c.CreateVol(cli.ctx, req)
	if err != nil {
		return err.Error()
	} else {
		return cli.serialize(reply)
	}
}

func volCreateFunc(cmd *cobra.Command, args []string) {
	cli := newClient(rootArgs)
	defer cli.close()
	output := cli.createVol(volCreateArgs)
	cli.show(output)
}

func (cli *client) deleteVol(args *volDeleteArgsStruct) string {
	req := &pbcp.DeleteVolRequest{
		VolName: args.volName,
	}
	reply, err := cli.c.DeleteVol(cli.ctx, req)
	if err != nil {
		return err.Error()
	} else {
		return cli.serialize(reply)
	}
}

func volDeleteFunc(cmd *cobra.Command, args []string) {
	cli := newClient(rootArgs)
	defer cli.close()
	output := cli.deleteVol(volDeleteArgs)
	cli.show(output)
}

func (cli *client) getVol(args *volGetArgsStruct) string {
	req := &pbcp.GetVolRequest{
		VolName: args.volName,
	}
	reply, err := cli.c.GetVol(cli.ctx, req)
	if err != nil {
		return err.Error()
	} else {
		return cli.serialize(reply)
	}
}

func volGetFunc(cmd *cobra.Command, args []string) {
	cli := newClient(rootArgs)
	defer cli.close()
	output := cli.getVol(volGetArgs)
	cli.show(output)
}

func (cli *client) exportVol(args *volExportArgsStruct) string {
	req := &pbcp.ExportVolRequest{
		VolName: args.volName,
		HostNqn: args.hostNqn,
	}
	reply, err := cli.c.ExportVol(cli.ctx, req)
	if err != nil {
		return err.Error()
	} else {
		return cli.serialize(reply)
	}
}

func volExportFunc(cmd *cobra.Command, args []string) {
	cli := newClient(rootArgs)
	defer cli.close()
	output := cli.exportVol(volExportArgs)
	cli.show(output)
}

func (cli *client) unexportVol(args *volUnexportArgsStruct) string {
	req := &pbcp.UnexportVolRequest{
		VolName: args.volName,
		HostNqn: args.hostNqn,
	}
	reply, err := cli.c.UnexportVol(cli.ctx, req)
	if err != nil {
		return err.Error()
	} else {
		return cli.serialize(reply)
	}
}

func volUnexportFunc(cmd *cobra.Command, args []string) {
	cli := newClient(rootArgs)
	defer cli.close()
	output := cli.unexportVol(volUnexportArgs)
	cli.show(output)
}
