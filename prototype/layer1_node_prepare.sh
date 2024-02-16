#!/bin/bash

set -e

CURR_DIR=$(readlink -f $(dirname $0))
source ${CURR_DIR}/common.sh

l1_mgr_id=$(format_id $1)
l1_port_num=$2
l1_tr_addr=$3
l1_tr_svc_id=$4
pv_path=$5

vg_name=$(get_vg_name ${l1_mgr_id})

lvm_pv_and_vg_create ${pv_path} ${vg_name}
nvmet_prepare ${l1_port_num} ${l1_tr_addr} ${l1_tr_svc_id}

echo "done"
