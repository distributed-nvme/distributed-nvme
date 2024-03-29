package cmdlineiface

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"github.com/google/uuid"
	"google.golang.org/grpc/metadata"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplane"
)

type rootArgsStruct struct {
	address string
	timeout int
}

var (
	rootCmd = &cobra.Command{
		Use:   "dnv_cli",
		Short: "dnv cli",
		Long:  `dnv cli`,
	}
	rootArgs = &rootArgsStruct{}
	gLogger = lib.NewPrefixLogger("cli")
)

func init() {
	rootCmd.PersistentFlags().StringVarP(
		&rootArgs.address, "address", "", "localhost:9520", "grpc address",
	)
	rootCmd.PersistentFlags().IntVarP(
		&rootArgs.timeout, "timeout", "", 30, "grpc timeout",
	)
	rootCmd.AddCommand(clusterCmd)
	rootCmd.AddCommand(dnCmd)
}

func CliExecute() {
	if err := rootCmd.Execute(); err != nil {
		gLogger.Fatal("Execute err: %v", err)
	}
}

type client struct {
	conn   *grpc.ClientConn
	c      pbcp.ExternalApiClient
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
		args.address,
		grpc.WithInsecure(),
		grpc.WithBlock(),
		grpc.WithTimeout(time.Duration(args.timeout)*time.Second),
	)
	if err != nil {
		gLogger.Fatal("Connection err: %v %v", args, err)
	}
	c := pbcp.NewExternalApiClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(args.timeout)*time.Second)
	md := metadata.Pairs(lib.TraceIdKey, uuid.New().String())
	newCtx := metadata.NewOutgoingContext(ctx, md)
	return &client{
		conn:   conn,
		c:      c,
		ctx:    newCtx,
		cancel: cancel,
	}
}

