#!/bin/bash

set -e

CURR_DIR=$(readlink -f $(dirname $0))
source ${CURR_DIR}/common.sh

sec_mgr_id=$(format_id $1)
sec_port_num=$2
sec_host_name=$3
vd_id=$(format_id $4)
stripe_id=$(format_id $5)
prim_mgr_id=$(format_id $6)
prim_tr_addr=$7
prim_tr_svc_id=$8
thindev_mb=$9
cntlr_cnt=${10}

shift 10

thindev_sectors=$((thindev_mb*1024*2))

prim_to_sec_tgt_nqn=$(get_prim_to_sec_tgt_nqn ${prim_mgr_id} ${vd_id} ${stripe_id} ${DEFAULT_THIN_DEV_ID_32BIT} ${sec_mgr_id})
prim_to_sec_host_nqn=$(get_host_nqn ${sec_host_name})
nvme_connect ${prim_to_sec_tgt_nqn} ${prim_tr_addr} ${prim_tr_svc_id} ${prim_to_sec_host_nqn}

thindev_path=$(nvme_dev_path_from_nqn ${prim_to_sec_tgt_nqn})

for i in $(seq ${cntlr_cnt}); do
    cntlr_mgr_id=$(format_id ${1})
    cntlr_host_name=$2
    shift 2
    sec_to_cntlr_name=$(get_sec_to_cntlr_name ${sec_mgr_id} ${vd_id} ${stripe_id} ${DEFAULT_THIN_DEV_ID_32BIT} ${cntlr_mgr_id})
    sec_to_cntlr_path="/dev/mapper/${sec_to_cntlr_name}"
    table="0 ${thindev_sectors} linear ${thindev_path} 0"
    dm_create ${sec_to_cntlr_name} "${table}"

    l2_to_cntlr_tgt_nqn=$(get_l2_to_cntlr_tgt_nqn ${vd_id} ${stripe_id} ${DEFAULT_THIN_DEV_ID_32BIT} ${cntlr_mgr_id})
    l2_to_cntlr_host_nqn=$(get_host_nqn ${cntlr_host_name})
    nvmet_create ${l2_to_cntlr_tgt_nqn} ${sec_to_cntlr_path} ${l2_to_cntlr_host_nqn} ${sec_port_num} ${ANA_GROUP_NON_OPTIMIZED} ${SEC0_CNTLID_MIN} ${SEC0_CNTLID_MAX}
done

echo "done"
