#!/bin/bash

set -ex

trap 'echo $(date)' DEBUG

CURR_DIR=$(readlink -f $(dirname $0))
source $CURR_DIR/conf.sh
source $CURR_DIR/utils.sh

ROOT_DIR=$CURR_DIR/../..
BIN_DIR=$ROOT_DIR/_out/linux_amd64
ETCD_BIN=$ROOT_DIR/bin/linux_amd64/etcd
NVME_BIN=$ROOT_DIR/bin/linux_amd64/nvme

if [ "$WORK_DIR" == "" ]; then
    echo "WORK_DIR is empty"
    exit 1
fi

sudo rm -rf $WORK_DIR
mkdir -p $WORK_DIR

dd if=/dev/zero of=$WORK_DIR/dn0.img bs=1M count=4096
dd if=/dev/zero of=$WORK_DIR/dn1.img bs=1M count=4096
sudo losetup /dev/loop240 $WORK_DIR/dn0.img
sudo losetup /dev/loop241 $WORK_DIR/dn1.img

echo "launch etcd"
$ETCD_BIN --listen-client-urls "http://localhost:$ETCD_PORT" \
          --advertise-client-urls "http://localhost:$ETCD_PORT" \
          --listen-peer-urls "http://localhost:$ETCD_PEER_PORT" \
          --name etcd0 --data-dir $WORK_DIR/etcd0.data \
          > $WORK_DIR/etcd0.log 2>&1 &

echo "launch dn agent 0"
sudo --background $BIN_DIR/dnvagent \
     --grpc-network tcp \
     --grpc-address "127.0.0.1:9020" \
     --role dn \
     > $WORK_DIR/dn_agent_0.log 2>&1 &
     
                  

echo "launch dn agent 1"
sudo --background $BIN_DIR/dnvagent \
     --grpc-network tcp \
     --grpc-address "127.0.0.1:9021" \
     --role dn \
     > $WORK_DIR/dn_agent_1.log 2>&1 &

echo "launch cn agent"
sudo --background $BIN_DIR/dnvagent \
     --grpc-network tcp \
     --grpc-address "127.0.0.1:9120" \
     --role cn \
     > $WORK_DIR/cn_agent.log 2>&1 &

echo "launch dn worker"
$BIN_DIR/dnvworker --etcd-endpoints "localhost:$ETCD_PORT" \
                   --grpc-network tcp --grpc-address "127.0.0.1:9220" \
                   --grpc-target "127.0.0.1:9220" \
                   --role dn \
                   > $WORK_DIR/dn_worker.log 2>&1 &

echo "launch cn worker"
$BIN_DIR/dnvworker --etcd-endpoints "localhost:$ETCD_PORT" \
                   --grpc-network tcp --grpc-address "127.0.0.1:9320" \
                   --grpc-target "127.0.0.1:9320" \
                   --role cn \
                   > $WORK_DIR/cn_worker.log 2>&1 &

echo "launch sp worker"
$BIN_DIR/dnvworker --etcd-endpoints "localhost:$ETCD_PORT" \
                   --grpc-network tcp --grpc-address "127.0.0.1:9420" \
                   --grpc-target "127.0.0.1:9420" \
                   --role sp \
                   > $WORK_DIR/sp_worker.log 2>&1 &

echo "launch api server"
$BIN_DIR/dnvapi --etcd-endpoints "localhost:$ETCD_PORT" \
                --grpc-network tcp --grpc-address "127.0.0.1:9520" \
                > $WORK_DIR/apiserver.log 2>&1 &

sleep 1

rsp=$($BIN_DIR/dnvctl --address 127.0.0.1:9520 cluster create)
verify_rsp_msg "${rsp}" "succeed"

rsp=$($BIN_DIR/dnvctl --address 127.0.0.1:9520 cluster get)
verify_rsp_msg "${rsp}" "succeed"

rsp=$($BIN_DIR/dnvctl --address 127.0.0.1:9520 dn create --grpc-target 127.0.0.1:9020 --dev-path /dev/loop240)
verify_rsp_msg "${rsp}" "succeed"

rsp=$($BIN_DIR/dnvctl --address 127.0.0.1:9520 dn get --grpc-target 127.0.0.1:9020)
verify_rsp_msg "${rsp}" "succeed"
dn_id_0=$(echo $rsp | jq -rM '.dn_id')

rsp=$($BIN_DIR/dnvctl --address 127.0.0.1:9520 dn create --grpc-target 127.0.0.1:9021 --dev-path /dev/loop241)
verify_rsp_msg "${rsp}" "succeed"

rsp=$($BIN_DIR/dnvctl --address 127.0.0.1:9520 dn get --grpc-target 127.0.0.1:9021)
verify_rsp_msg "${rsp}" "succeed"
dn_id_1=$(echo $rsp | jq -rM '.dn_id')

rsp=$($BIN_DIR/dnvctl --address 127.0.0.1:9520 cn create --grpc-target 127.0.0.1:9120)
verify_rsp_msg "${rsp}" "succeed"

rsp=$($BIN_DIR/dnvctl --address 127.0.0.1:9520 cn get --grpc-target 127.0.0.1:9120)
verify_rsp_msg "${rsp}" "succeed"
cn_id=$(echo $rsp | jq -rM '.cn_id')

sleep 10

rsp=$($BIN_DIR/dnvctl --address 127.0.0.1:9520 vol create --vol-name vol0 --size 1048576)
verify_rsp_msg "${rsp}" "succeed"

retry_cnt=0
max_retry=20
while true; do
    rsp=$($BIN_DIR/dnvctl --address 127.0.0.1:9520 vol get --vol-name vol0)
    verify_rsp_msg "${rsp}" "succeed"
    msg=$(echo $rsp | jq -rM '.sp_info.status_info.msg')
    if [ "$msg" == "succeed" ]; then
        echo "succeed"
        break
    fi
    if [ $retry_cnt -ge $max_retry ]; then
        echo "fail"
        exit 1
    fi
    sleep 1
    ((retry_cnt=retry_cnt+1))
done

rsp=$($BIN_DIR/dnvctl --address 127.0.0.1:9520 vol get --vol-name vol0)
verify_rsp_msg "${rsp}" "succeed"

sp_id=$(echo $rsp | jq -rM '.sp_id')
ss_id=$(echo $rsp | jq -rM '.sp_conf.ss_conf_list[0].ss_id')
tr_type=$(echo $rsp | jq -rM '.sp_conf.cntlr_conf_list[0].nvme_port_conf.nvme_listener.tr_type')
adr_fam=$(echo $rsp | jq -rM '.sp_conf.cntlr_conf_list[0].nvme_port_conf.nvme_listener.adr_fam')
tr_addr=$(echo $rsp | jq -rM '.sp_conf.cntlr_conf_list[0].nvme_port_conf.nvme_listener.tr_addr')
tr_svc_id=$(echo $rsp | jq -rM '.sp_conf.cntlr_conf_list[0].nvme_port_conf.nvme_listener.tr_svc_id')

nqn="nqn.2024-01.io.dnv:1200:${sp_id}:${ss_id}"

rsp=$($BIN_DIR/dnvctl --address 127.0.0.1:9520 vol export --vol-name vol0 --host-nqn $HOST_NQN)
verify_rsp_msg "${rsp}" "succeed"

retry_cnt=0
max_retry=20
while true; do
    rsp=$($BIN_DIR/dnvctl --address 127.0.0.1:9520 vol get --vol-name vol0)
    verify_rsp_msg "${rsp}" "succeed"
    msg=$(echo $rsp | jq -rM '.sp_info.ss_info_list[0].ss_per_cntlr_info_list[0].host_info_list[0].status_info.msg')
    if [ "$msg" == "succeed" ]; then
        echo "succeed"
        break
    fi
    if [ $retry_cnt -ge $max_retry ]; then
        echo "fail"
        exit 1
    fi
    sleep 1
    ((retry_cnt=retry_cnt+1))
done

sudo $NVME_BIN connect --nqn "${nqn}" --transport "${tr_type}" --traddr "${tr_addr}" --trsvcid "${tr_svc_id}" --hostnqn "${HOST_NQN}" --hostid "${HOST_ID}"

sudo $NVME_BIN disconnect --nqn "${nqn}"

rsp=$($BIN_DIR/dnvctl --address 127.0.0.1:9520 vol unexport --vol-name vol0 --host-nqn $HOST_NQN)
verify_rsp_msg "${rsp}" "succeed"

rsp=$($BIN_DIR/dnvctl --address 127.0.0.1:9520 vol delete --vol-name vol0)
verify_rsp_msg "${rsp}" "succeed"

vreify_res_no_exist

rsp=$($BIN_DIR/dnvctl --address 127.0.0.1:9520 cn delete --cn-id $cn_id)
verify_rsp_msg "${rsp}" "succeed"

rsp=$($BIN_DIR/dnvctl --address 127.0.0.1:9520 dn delete --dn-id $dn_id_0)
verify_rsp_msg "${rsp}" "succeed"

rsp=$($BIN_DIR/dnvctl --address 127.0.0.1:9520 dn delete --dn-id $dn_id_1)
verify_rsp_msg "${rsp}" "succeed"

rsp=$($BIN_DIR/dnvctl --address 127.0.0.1:9520 cluster delete)
verify_rsp_msg "${rsp}" "succeed"

rsp=$($BIN_DIR/dnvctl --address 127.0.0.1:9520 cluster get)
verify_rsp_code "${rsp}" "1002"

cleanup

sleep 1

force_cleanup

sleep 1

cleanup_check

echo "done"
