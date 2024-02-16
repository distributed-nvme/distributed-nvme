#!/bin/bash

set -e

CURR_DIR=$(readlink -f $(dirname $0))
source ${CURR_DIR}/common.sh

sec_mgr_id=$(format_id $1)
sec_port_num=$2
vd_id=$(format_id $3)
stripe_id=$(format_id $4)
prim_mgr_id=$(format_id $5)
prim_tr_addr=$6
prim_tr_svc_id=$7
cntlr_cnt=$8

shift 8

for i in $(seq ${cntlr_cnt}); do
    cntlr_mgr_id=$(format_id ${1})
    cntlr_host_name=$2
    shift 2

    l2_to_cntlr_tgt_nqn=$(get_l2_to_cntlr_tgt_nqn ${vd_id} ${stripe_id} ${DEFAULT_THIN_DEV_ID_32BIT} ${cntlr_mgr_id})
    l2_to_cntlr_host_nqn=$(get_host_nqn ${cntlr_host_name})
    nvmet_delete ${l2_to_cntlr_tgt_nqn} ${l2_to_cntlr_host_nqn} ${sec_port_num}

    sec_to_cntlr_name=$(get_sec_to_cntlr_name ${sec_mgr_id} ${vd_id} ${stripe_id} ${DEFAULT_THIN_DEV_ID_32BIT} ${cntlr_mgr_id})
    dm_delete ${sec_to_cntlr_name}
done

prim_to_sec_tgt_nqn=$(get_prim_to_sec_tgt_nqn ${prim_mgr_id} ${vd_id} ${stripe_id} ${DEFAULT_THIN_DEV_ID_32BIT} ${sec_mgr_id})
nvme_disconnect ${prim_to_sec_tgt_nqn} ${prim_tr_addr} ${prim_tr_svc_id}

echo "done"
