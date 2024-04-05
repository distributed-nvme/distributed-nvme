package stmwrapper

import (
	"fmt"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/ctxhelper"
)

type StmError struct {
	Code uint32
	Msg  string
}

func (e *StmError) Error() string {
	return fmt.Sprintf("code: %d msg: %s", e.Code, e.Msg)
}

type StmWrapper struct {
	etcdCli *clientv3.Client
}

func (sm *StmWrapper) RunStm(
	pch *ctxhelper.PerCtxHelper,
	apply func(stm concurrency.STM) error,
) error {
	cnt := 0
	applyWrapper := func(stm concurrency.STM) error {
		cnt++
		pch.Logger.Info("stm apply, cnt=%d", cnt)
		err := apply(stm)
		if err != nil {
			pch.Logger.Warning("stm apply err: %v", err)
		}
		return err
	}
	_, err := concurrency.NewSTM(
		sm.etcdCli,
		applyWrapper,
		concurrency.WithAbortContext(pch.Ctx),
	)
	if err != nil {
		pch.Logger.Warning("stm create err: %v", err)
	}
	return err
}

func NewStmWrapper(etcdCli *clientv3.Client) *StmWrapper {
	return &StmWrapper{
		etcdCli: etcdCli,
	}
}
