#!/bin/bash

set -e

CURR_DIR=$(readlink -f $(dirname $0))
source ${CURR_DIR}/common.sh

l1_mgr_id=$(format_id $1)
l1_port_num=$2
pv_path=$3

vg_name=$(get_vg_name ${l1_mgr_id})

nvmet_cleanup ${l1_port_num}
lvm_pv_and_vg_delete ${pv_path} ${vg_name}

echo "done"
