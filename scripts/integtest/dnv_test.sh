#!/bin/bash

set -ex

trap 'echo -n $(date)' DEBUG

CURR_DIR=$(readlink -f $(dirname $0))
source $CURR_DIR/conf.sh
source $CURR_DIR/utils.sh

ROOT_DIR=$CURR_DIR/../..
BIN_DIR=$ROOT_DIR/_out/linux_amd64
ETCD_BIN=$ROOT_DIR/bin/linux_amd64/etcd

sudo rm -rf $WORK_DIR
mkdir -p $WORK_DIR

dd if=/dev/zero of=$WORK_DIR/dn0.img bs=1M count=1024
dd if=/dev/zero of=$WORK_DIR/dn1.img bs=1M count=1024
sudo losetup /dev/loop240 $WORK_DIR/dn0.img
sudo losetup /dev/loop241 $WORK_DIR/dn1.img

echo "launch etcd"
$ETCD_BIN --listen-client-urls "http://localhost:$ETCD_PORT" \
          --advertise-client-urls "http://localhost:$ETCD_PORT" \
          --listen-peer-urls "http://localhost:$ETCD_PEER_PORT" \
          --name etcd0 --data-dir $WORK_DIR/etcd0.data \
          > $WORK_DIR/etcd0.log 2>&1 &

echo "launch dn agent 0"
sudo $BIN_DIR/dnvagent --grpc-network tcp --grpc-address "127.0.0.1:9020" --role dn \
                  > $WORK_DIR/dn_agent_0.log 2>&1 &

echo "launch dn agent 1"
sudo $BIN_DIR/dnvagent --grpc-network tcp --grpc-address "127.0.0.1:9021" --role dn \
                  > $WORK_DIR/dn_agent_1.log 2>&1 &

echo "launch cn agent"
sudo $BIN_DIR/dnvagent --grpc-network tcp --grpc-address "127.0.0.1:9120" --role cn \
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

rsp=$($BIN_DIR/dnvctl --address 127.0.0.1:9520 dn delete --dn-id $dn_id_0)
verify_rsp_msg "${rsp}" "succeed"

rsp=$($BIN_DIR/dnvctl --address 127.0.0.1:9520 dn delete --dn-id $dn_id_1)
verify_rsp_msg "${rsp}" "succeed"

rsp=$($BIN_DIR/dnvctl --address 127.0.0.1:9520 cluster delete)
verify_rsp_msg "${rsp}" "succeed"

rsp=$($BIN_DIR/dnvctl --address 127.0.0.1:9520 cluster get)
verify_rsp_code "${rsp}" "1002"

cleanup

echo "done"
