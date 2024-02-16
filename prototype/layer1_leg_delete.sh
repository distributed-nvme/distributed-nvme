#!/bin/bash

set -e

CURR_DIR=$(readlink -f $(dirname $0))
source ${CURR_DIR}/common.sh

l1_mgr_id=$(format_id $1)
l1_port_num=$2
vd_id=$(format_id $3)
leg_id=$(format_id $4)
prim_mgr_id=$(format_id $5)
prim_host_name=$6

vg_name=$(get_vg_name ${l1_mgr_id})
leg_lv_name=$(get_leg_lv_name ${l1_mgr_id} ${vd_id} ${leg_id})
leg_to_prim_name=$(get_leg_to_prim_name ${l1_mgr_id} ${vd_id} ${leg_id} ${prim_mgr_id})
leg_to_prim_tgt_nqn=$(get_leg_to_prim_tgt_nqn ${l1_mgr_id} ${vd_id} ${leg_id} ${prim_mgr_id})
leg_to_prim_host_nqn=$(get_host_nqn ${prim_host_name})

nvmet_delete ${leg_to_prim_tgt_nqn} ${leg_to_prim_host_nqn} ${l1_port_num}
dm_delete ${leg_to_prim_name}
lvm_lv_delete ${leg_lv_name} ${vg_name}

echo "done"
