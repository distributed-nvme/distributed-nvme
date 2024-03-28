package controlplane

import (
	"fmt"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
)
type cpStmError struct {
	code uint32
	msg  string
}

func (e *cpStmError) Error() string {
	return fmt.Sprintf("code: %d msg: %s", e.code, e.msg)
}

type stmWrapper struct {
	etcdCli *clientv3.Client
}

func (sm *stmWrapper)runStm(
	pch *lib.PerCtxHelper,
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

func newStmWrapper(etcdCli *clientv3.Client) (*stmWrapper){
	return &stmWrapper{
		etcdCli: etcdCli,
	}
}
