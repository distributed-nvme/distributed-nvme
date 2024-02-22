#!/bin/bash

set -e

CURR_DIR=$(readlink -f $(dirname $0))
source ${CURR_DIR}/common.sh

cn_mgr_id=$(format_id $1)
cn_port_num=$2
cn_host_name=$3
vd_id=$(format_id $4)
external_host_nqn=$5
leg_cnt=$6

shift 6

host_nqn=$(get_host_nqn ${cn_host_name})

final_nqn=$(get_final_nqn ${vd_id} ${DEFAULT_THIN_DEV_ID_32BIT})
nvmet_delete ${final_nqn} ${host_nqn} ${cn_port_num}

final_dev_name=$(get_final_dev_name ${cn_mgr_id} ${vd_id} ${DEFAULT_THIN_DEV_ID_32BIT})
dm_delete ${final_dev_name}

for _ in $(seq ${leg_cnt}); do
    leg_id=$(format_id $1)
    owner_cn_mgr_id=$(format_id $2)
    owner_cn_tr_addr=$3
    owner_cn_tr_svc_id=$4
    shift 4
    if [ "${owner_cn_mgr_id}" != "${cn_mgr_id}" ]; then
        forward_nqn=$(get_forward_nqn ${owner_cn_mgr_id} ${vd_id} ${leg_id} ${DEFAULT_THIN_DEV_ID_32BIT} ${cn_mgr_id})
        nvme_disconnect ${forward_nqn} ${owner_cn_tr_addr} ${owner_cn_tr_svc_id}
    fi
done

echo "done"
