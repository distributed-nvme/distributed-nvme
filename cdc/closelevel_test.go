package cdc

import (
	"context"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
)

// closedClient is an etcd client already closed once, whose next Close
// fails.
func closedClient(t *testing.T) *etcdutil.Client {
	t.Helper()
	cli, err := etcdutil.New(context.Background(), []string{"127.0.0.1:1"},
		time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := cli.Close(); err != nil {
		t.Fatal(err)
	}
	return cli
}

// log.md R6 (CM4): a failed close of the etcd client at the stop is an event
// the program cannot act on, at Info, as in the gateway and the worker.
func TestFailedCloseAtStopIsInfo(t *testing.T) {
	logs := captureLogs(t)
	if err := Run(context.Background(), closedClient(t), Config{
		Ranges: []uint32{0}, TrType: common.DefaultCdcTrType,
		AdrFam: common.DefaultCdcAdrFam, TrAddr: "256.0.0.1", TrSvcId: "8009",
	}); err == nil {
		t.Fatal("Run served on an address that cannot be listened on")
	}
	recs := logs.find("etcd client close failed")
	if len(recs) != 1 || recs[0]["level"] != "INFO" {
		t.Errorf("close records %v, want one at INFO", recs)
	}
}
