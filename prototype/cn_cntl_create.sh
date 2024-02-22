#!/bin/bash

set -e

CURR_DIR=$(readlink -f $(dirname $0))
source ${CURR_DIR}/common.sh

cn_mgr_id=$(format_id $1)
cn_port_num=$2
cn_host_name=$3
cntlid_min=$4
cntlid_max=$5
vd_id=$(format_id $6)
external_host_nqn=$7
thin_dev_size_mb=$8
leg_cnt=$9

shift 9

host_nqn=$(get_host_nqn ${cn_host_name})
total_sectors=$((leg_cnt*thin_dev_size_mb*1024*2))

table="0 ${total_sectors} raid raid0 1 ${RAID0_STRIPE_SECTORS} ${leg_cnt}"
for _ in $(seq ${leg_cnt}); do
    leg_id=$(format_id $1)
    owner_cn_mgr_id=$(format_id $2)
    owner_cn_tr_addr=$3
    owner_cn_tr_svc_id=$4
    shift 4
    if [ "${owner_cn_mgr_id}" == "${cn_mgr_id}" ]; then
        thin_dev_name=$(get_thin_dev_name ${cn_mgr_id} ${vd_id} ${leg_id} ${DEFAULT_THIN_DEV_ID_32BIT})
        thin_dev_path="/dev/mapper/${thin_dev_name}"
        table="${table} - ${thin_dev_path}"
    else
        forward_nqn=$(get_forward_nqn ${owner_cn_mgr_id} ${vd_id} ${leg_id} ${DEFAULT_THIN_DEV_ID_32BIT} ${cn_mgr_id})
        nvme_connect ${forward_nqn} ${owner_cn_tr_addr} ${owner_cn_tr_svc_id} ${host_nqn}
        dev_path=$(nvme_dev_path_from_nqn ${forward_nqn})
        table="${table} - ${dev_path}"
    fi
done

final_dev_name=$(get_final_dev_name ${cn_mgr_id} ${vd_id} ${DEFAULT_THIN_DEV_ID_32BIT})
final_dev_path="/dev/mapper/${final_dev_name}"
dm_create ${final_dev_name} "${table}"

final_nqn=$(get_final_nqn ${vd_id} ${DEFAULT_THIN_DEV_ID_32BIT})
nvmet_create ${final_nqn} ${final_dev_path} ${external_host_nqn} ${cn_port_num} ${ANA_GROUP_OPTIMIZED} ${cntlid_min} ${cntlid_max}

echo "done"
