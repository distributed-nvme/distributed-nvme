#!/bin/bash

CURR_DIR=$(readlink -f $(dirname $0))
source ${CURR_DIR}/common.sh

cntlr_mgr_id=$(format_id $1)
cntlr_port_num=$2
vd_id=$(format_id $3)
external_host_nqn=$4
stripe_cnt=$5

shift 5

final_tgt_nqn=$(get_final_tgt_nqn ${vd_id} ${DEFAULT_THIN_DEV_ID_32BIT})
nvmet_delete ${final_tgt_nqn} ${external_host_nqn} ${cntlr_port_num}

final_dev_name=$(get_final_dev_name ${cntlr_mgr_id} ${vd_id} ${DEFAULT_THIN_DEV_ID_32BIT})
dm_delete ${final_dev_name}

for i in $(seq ${stripe_cnt}); do
    stripe_id=$(format_id $1)
    l2_prim_tr_addr=$2
    l2_prim_tr_svc_id=$3
    l2_sec0_tr_addr=$4
    l2_sec0_tr_svc_id=$5
    shift 5

    l2_to_cntlr_tgt_nqn=$(get_l2_to_cntlr_tgt_nqn ${vd_id} ${stripe_id} ${DEFAULT_THIN_DEV_ID_32BIT} ${cntlr_mgr_id})
    nvme_disconnect ${l2_to_cntlr_tgt_nqn} ${l2_prim_tr_addr} ${l2_prim_tr_svc_id}
    if [ "${l2_sec0_tr_addr}" != "-" ]; then
        nvme_disconnect ${l2_to_cntlr_tgt_nqn} ${l2_sec0_tr_addr} ${l2_sec0_tr_svc_id}
    fi
done

echo "done"
