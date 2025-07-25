#!/bin/bash

set -ex

trap 'echo $(date)' DEBUG

CURR_DIR=$(readlink -f $(dirname $0))
source $CURR_DIR/multi_nodes_conf.sh

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

function start_dn_node() {
    node_name=$1
    echo "sudo rm -rf ${WORK_DIR}" | ssh $node_name
    echo "mkdir -p ${WORK_DIR}" | ssh $node_name
    scp $CURR_DIR/* $node_name:${WORK_DIR}/
    scp $BIN_DIR/* $node_name:${WORK_DIR}/
    ip_addr=$(dig +short $node_name)
    echo "${WORK_DIR}/launch_dn.sh ${ip_addr}" | ssh $node_name
}

function start_cn_node() {
    node_name=$1
    echo "sudo rm -rf ${WORK_DIR}" | ssh $node_name
    echo "mkdir -p ${WORK_DIR}" | ssh $node_name
    scp $CURR_DIR/* $node_name:${WORK_DIR}/
    scp $BIN_DIR/* $node_name:${WORK_DIR}/
    ip_addr=$(dig +short $node_name)
    echo "${WORK_DIR}/launch_cn.sh ${ip_addr}" | ssh $node_name
}

function stop_node() {
    node_name=$1
    echo "${WORK_DIR}/cleanup.sh" | ssh $node_name
}

start_dn_node dn0
start_dn_node dn1
start_dn_node dn2
start_dn_node dn3
start_cn_node cn0
start_cn_node cn1
start_cn_node cn2

sleep 3

stop_node dn0
stop_node dn1
stop_node dn2
stop_node dn3
stop_node cn0
stop_node cn1
stop_node cn2

