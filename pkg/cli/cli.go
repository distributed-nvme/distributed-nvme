package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplaneapi"
)

type rootArgsStruct struct {
	cpAddr    string
	cpTimeout int
}

var (
	rootCmd = &cobra.Command{
		Use:   "dnv_cli",
		Short: "dnv cli",
		Long:  `dnv cli`,
	}
	rootArgs = &rootArgsStruct{}
	gLogger = lib.NewLogger("cli")
)

func init() {
	rootCmd.PersistentFlags().StringVarP(
		&rootArgs.cpAddr, "cp-addr", "", "localhost:9520", "control plane socket address")
	rootCmd.PersistentFlags().IntVarP(
		&rootArgs.cpTimeout, "cp-timeout", "", 30, "control plane timeout")
	rootCmd.AddCommand(dnCmd)
	rootCmd.AddCommand(clusterCmd)
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		gLogger.Fatal("Execute err: %v", err)
	}
}

type client struct {
	conn   *grpc.ClientConn
	c      pbcp.ControlPlaneClient
	ctx    context.Context
	cancel context.CancelFunc
}

func (cli *client) close() {
	cli.cancel()
	cli.conn.Close()
}

func (cli *client) serialize(reply interface{}) string {
	output, err := json.MarshalIndent(reply, "", "  ")
	if err != nil {
		return err.Error()
	} else {
		return string(output)
	}
}

func (cli *client) show(output string) {
	fmt.Println(output)
}

func newClient(args *rootArgsStruct) *client {
	conn, err := grpc.Dial(
		args.cpAddr,
		grpc.WithInsecure(),
		grpc.WithBlock(),
		grpc.WithTimeout(time.Duration(args.cpTimeout)*time.Second),
	)
	if err != nil {
		gLogger.Fatal("Connection err: %v %v", args, err)
	}
	c := pbcp.NewControlPlaneClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(args.cpTimeout)*time.Second)
	return &client{
		conn:   conn,
		c:      c,
		ctx:    ctx,
		cancel: cancel,
	}
}
