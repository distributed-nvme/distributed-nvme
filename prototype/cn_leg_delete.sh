#!/bin/bash

set -e

CURR_DIR=$(readlink -f $(dirname $0))
source ${CURR_DIR}/common.sh

cn_mgr_id=$(format_id $1)
cn_port_num=$2
cn_host_name=$3
vd_id=$(format_id $4)
leg_id=$(format_id $5)
grp0_id=$(format_id $6)
ld_id_grp0_0=$(format_id $7)
dn_mgr_id_grp0_0=$(format_id $8)
dn_tr_addr_grp0_0=$9
dn_tr_svc_id_grp0_0=${10}
ld_id_grp0_1=$(format_id ${11})
dn_mgr_id_grp0_1=$(format_id ${12})
dn_tr_addr_grp0_1=${13}
dn_tr_svc_id_grp0_1=${14}
forward_cn_cnt=${15}

shift 15

for _ in $(seq ${forward_cn_cnt}); do
    forward_cn_mgr_id=$(format_id ${1})
    forward_cn_host_name=$2
    shift 2

    forward_nqn=$(get_forward_nqn ${cn_mgr_id} ${vd_id} ${leg_id} ${DEFAULT_THIN_DEV_ID_32BIT} ${forward_cn_mgr_id})
    forward_host_nqn=$(get_host_nqn ${forward_cn_host_name})
    nvmet_delete ${forward_nqn} ${forward_host_nqn} ${cn_port_num}

    forward_dev_name=$(get_forward_dev_name ${cn_mgr_id} ${vd_id} ${leg_id} ${DEFAULT_THIN_DEV_ID_32BIT} ${forward_cn_mgr_id})
    dm_delete ${forward_dev_name}
done

thin_dev_name=$(get_thin_dev_name ${cn_mgr_id} ${vd_id} ${leg_id} ${DEFAULT_THIN_DEV_ID_32BIT})
dm_delete ${thin_dev_name}

thin_pool_name=$(get_thin_pool_name ${cn_mgr_id} ${vd_id} ${leg_id})
dm_delete ${thin_pool_name}

thin_data_name=$(get_thin_data_name ${cn_mgr_id} ${vd_id} ${leg_id})
dm_delete ${thin_data_name}

thin_data_grp0_name=$(get_thin_data_grp_name ${cn_mgr_id} ${vd_id} ${grp0_id})
dm_delete ${thin_data_grp0_name}

thin_meta_name=$(get_thin_meta_name ${cn_mgr_id} ${vd_id} ${leg_id})
dm_delete ${thin_meta_name}

thin_meta_grp0_name=$(get_thin_meta_grp_name ${cn_mgr_id} ${vd_id} ${grp0_id})
dm_delete ${thin_meta_grp0_name}

thin_data_raid1_data_name_grp0_1=$(get_thin_data_raid1_data_name ${cn_mgr_id} ${vd_id} ${ld_id_grp0_1})
dm_delete ${thin_data_raid1_data_name_grp0_1}

thin_data_raid1_meta_name_grp0_1=$(get_thin_data_raid1_meta_name ${cn_mgr_id} ${vd_id} ${ld_id_grp0_1})
dm_delete ${thin_data_raid1_meta_name_grp0_1}

thin_meta_raid1_data_name_grp0_1=$(get_thin_meta_raid1_data_name ${cn_mgr_id} ${vd_id} ${ld_id_grp0_1})
dm_delete ${thin_meta_raid1_data_name_grp0_1}

thin_meta_raid1_meta_name_grp0_1=$(get_thin_meta_raid1_meta_name ${cn_mgr_id} ${vd_id} ${ld_id_grp0_1})
dm_delete ${thin_meta_raid1_meta_name_grp0_1}

ld_to_leg_nqn_grp0_1=$(get_ld_to_leg_nqn ${dn_mgr_id_grp0_1} ${vd_id} ${ld_id_grp0_1} ${cn_mgr_id})
nvme_disconnect ${ld_to_leg_nqn_grp0_1} ${dn_tr_addr_grp0_1} ${dn_tr_svc_id_grp0_1}

thin_data_raid1_data_name_grp0_0=$(get_thin_data_raid1_data_name ${cn_mgr_id} ${vd_id} ${ld_id_grp0_0})
dm_delete ${thin_data_raid1_data_name_grp0_0}

thin_data_raid1_meta_name_grp0_0=$(get_thin_data_raid1_meta_name ${cn_mgr_id} ${vd_id} ${ld_id_grp0_0})
dm_delete ${thin_data_raid1_meta_name_grp0_0}

thin_meta_raid1_data_name_grp0_0=$(get_thin_meta_raid1_data_name ${cn_mgr_id} ${vd_id} ${ld_id_grp0_0})
dm_delete ${thin_meta_raid1_data_name_grp0_0}

thin_meta_raid1_meta_name_grp0_0=$(get_thin_meta_raid1_meta_name ${cn_mgr_id} ${vd_id} ${ld_id_grp0_0})
dm_delete ${thin_meta_raid1_meta_name_grp0_0}

ld_to_leg_nqn_grp0_0=$(get_ld_to_leg_nqn ${dn_mgr_id_grp0_0} ${vd_id} ${ld_id_grp0_0} ${cn_mgr_id})
nvme_disconnect ${ld_to_leg_nqn_grp0_0} ${dn_tr_addr_grp0_0} ${dn_tr_svc_id_grp0_0}
