#!/bin/bash

set -e

CURR_DIR=$(readlink -f $(dirname $0))
source ${CURR_DIR}/common.sh

cntlr_mgr_id=$(format_id $1)
cntlr_port_num=$2
cntlr_host_name=$3
vd_id=$(format_id $4)
external_host_nqn=$5
cntlid_min=$6
cntlid_max=$7
thindev_mb=$8
stripe_cnt=$9

shift 9

total_sectors=$((thindev_mb*stripe_cnt*1024*2))

table="0 ${total_sectors} raid raid0 1 ${RAID0_STRIPE_SECTORS} ${stripe_cnt}"
for i in $(seq ${stripe_cnt}); do
    stripe_id=$(format_id $1)
    l2_prim_tr_addr=$2
    l2_prim_tr_svc_id=$3
    l2_sec0_tr_addr=$4
    l2_sec0_tr_svc_id=$5
    shift 5

    l2_to_cntlr_tgt_nqn=$(get_l2_to_cntlr_tgt_nqn ${vd_id} ${stripe_id} ${DEFAULT_THIN_DEV_ID_32BIT} ${cntlr_mgr_id})
    l2_to_cntlr_host_nqn=$(get_host_nqn ${cntlr_host_name})
    nvme_connect ${l2_to_cntlr_tgt_nqn} ${l2_prim_tr_addr} ${l2_prim_tr_svc_id} ${l2_to_cntlr_host_nqn}
    if [ "${l2_sec0_tr_addr}" != "-" ]; then
        nvme_connect ${l2_to_cntlr_tgt_nqn} ${l2_sec0_tr_addr} ${l2_sec0_tr_svc_id} ${l2_to_cntlr_host_nqn}
    fi

    dev_path=$(nvme_dev_path_from_nqn ${l2_to_cntlr_tgt_nqn})
    table="${table} - ${dev_path}"
done

final_dev_name=$(get_final_dev_name ${cntlr_mgr_id} ${vd_id} ${DEFAULT_THIN_DEV_ID_32BIT})
final_dev_path="/dev/mapper/${final_dev_name}"
dm_create ${final_dev_name} "${table}"

final_tgt_nqn=$(get_final_tgt_nqn ${vd_id} ${DEFAULT_THIN_DEV_ID_32BIT})
nvmet_create ${final_tgt_nqn} ${final_dev_path} ${external_host_nqn} ${cntlr_port_num} ${ANA_GROUP_OPTIMIZED} ${cntlid_min} ${cntlid_max}

echo "done"
