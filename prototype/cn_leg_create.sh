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
ld_size_mb=${15}
thin_dev_size_mb=${16}
forward_cn_cnt=${17}

ld_size_sectors=$((ld_size_mb*1024*2))
data_size_sectors=$((ld_size_sectors-THIN_META_RAID1_META_SECTORS-THIN_META_RAID1_DATA_SECTORS-THIN_DATA_RAID1_META_SECTORS))
thin_dev_size_sectors=$((thin_dev_size_mb*1024*2))

cn_host_nqn=$(get_host_nqn ${cn_host_name})

ld_to_leg_nqn_grp0_0=$(get_ld_to_leg_nqn ${dn_mgr_id_grp0_0} ${vd_id} ${ld_id_grp0_0} ${cn_mgr_id})
nvme_connect ${ld_to_leg_nqn_grp0_0} ${dn_tr_addr_grp0_0} ${dn_tr_svc_id_grp0_0} ${cn_host_nqn}
ld_path_grp0_0=$(nvme_dev_path_from_nqn ${ld_to_leg_nqn_grp0_0})

thin_meta_raid1_meta_name_grp0_0=$(get_thin_meta_raid1_meta_name ${cn_mgr_id} ${vd_id} ${ld_id_grp0_0})
thin_meta_raid1_meta_path_grp0_0="/dev/mapper/${thin_meta_raid1_meta_name_grp0_0}"
table="0 ${THIN_META_RAID1_META_SECTORS} linear ${ld_path_grp0_0} ${THIN_META_RAID1_META_START}"
dm_create ${thin_meta_raid1_meta_name_grp0_0} "${table}"

thin_meta_raid1_data_name_grp0_0=$(get_thin_meta_raid1_data_name ${cn_mgr_id} ${vd_id} ${ld_id_grp0_0})
thin_meta_raid1_data_path_grp0_0="/dev/mapper/${thin_meta_raid1_data_name_grp0_0}"
table="0 ${THIN_META_RAID1_DATA_SECTORS} linear ${ld_path_grp0_0} ${THIN_META_RAID1_DATA_START}"
dm_create ${thin_meta_raid1_data_name_grp0_0} "${table}"

thin_data_raid1_meta_name_grp0_0=$(get_thin_data_raid1_meta_name ${cn_mgr_id} ${vd_id} ${ld_id_grp0_0})
thin_data_raid1_meta_path_grp0_0="/dev/mapper/${thin_data_raid1_meta_name_grp0_0}"
table="0 ${THIN_DATA_RAID1_META_SECTORS} linear ${ld_path_grp0_0} ${THIN_DATA_RAID1_META_START}"
dm_create ${thin_data_raid1_meta_name_grp0_0} "${table}"

thin_data_raid1_data_name_grp0_0=$(get_thin_data_raid1_data_name ${cn_mgr_id} ${vd_id} ${ld_id_grp0_0})
thin_data_raid1_data_path_grp0_0="/dev/mapper/${thin_data_raid1_data_name_grp0_0}"
table="0 ${data_size_sectors} linear ${ld_path_grp0_0} ${THIN_DATA_RAID1_DATA_START}"
dm_create ${thin_data_raid1_data_name_grp0_0} "${table}"

ld_to_leg_nqn_grp0_1=$(get_ld_to_leg_nqn ${dn_mgr_id_grp0_1} ${vd_id} ${ld_id_grp0_1} ${cn_mgr_id})
nvme_connect ${ld_to_leg_nqn_grp0_1} ${dn_tr_addr_grp0_1} ${dn_tr_svc_id_grp0_1} ${cn_host_nqn}
ld_path_grp0_1=$(nvme_dev_path_from_nqn ${ld_to_leg_nqn_grp0_1})

thin_meta_raid1_meta_name_grp0_1=$(get_thin_meta_raid1_meta_name ${cn_mgr_id} ${vd_id} ${ld_id_grp0_1})
thin_meta_raid1_meta_path_grp0_1="/dev/mapper/${thin_meta_raid1_meta_name_grp0_1}"
table="0 ${THIN_META_RAID1_META_SECTORS} linear ${ld_path_grp0_1} ${THIN_META_RAID1_META_START}"
dm_create ${thin_meta_raid1_meta_name_grp0_1} "${table}"

thin_meta_raid1_data_name_grp0_1=$(get_thin_meta_raid1_data_name ${cn_mgr_id} ${vd_id} ${ld_id_grp0_1})
thin_meta_raid1_data_path_grp0_1="/dev/mapper/${thin_meta_raid1_data_name_grp0_1}"
table="0 ${THIN_META_RAID1_DATA_SECTORS} linear ${ld_path_grp0_1} ${THIN_META_RAID1_DATA_START}"
dm_create ${thin_meta_raid1_data_name_grp0_1} "${table}"

thin_data_raid1_meta_name_grp0_1=$(get_thin_data_raid1_meta_name ${cn_mgr_id} ${vd_id} ${ld_id_grp0_1})
thin_data_raid1_meta_path_grp0_1="/dev/mapper/${thin_data_raid1_meta_name_grp0_1}"
table="0 ${THIN_DATA_RAID1_META_SECTORS} linear ${ld_path_grp0_1} ${THIN_DATA_RAID1_META_START}"
dm_create ${thin_data_raid1_meta_name_grp0_1} "${table}"

thin_data_raid1_data_name_grp0_1=$(get_thin_data_raid1_data_name ${cn_mgr_id} ${vd_id} ${ld_id_grp0_1})
thin_data_raid1_data_path_grp0_1="/dev/mapper/${thin_data_raid1_data_name_grp0_1}"
table="0 ${data_size_sectors} linear ${ld_path_grp0_1} ${THIN_DATA_RAID1_DATA_START}"
dm_create ${thin_data_raid1_data_name_grp0_1} "${table}"

dd if=/dev/zero of=${thin_meta_raid1_meta_path_grp0_0} bs=4k count=1
dd if=/dev/zero of=${thin_meta_raid1_meta_path_grp0_1} bs=4k count=1
thin_meta_grp0_name=$(get_thin_meta_grp_name ${cn_mgr_id} ${vd_id} ${grp0_id})
thin_meta_grp0_path="/dev/mapper/${thin_meta_grp0_name}"
table="0 ${THIN_META_RAID1_SECTORS} raid raid1 4 0 region_size ${THIN_META_REGION_SECTORS} nosync 2 ${thin_meta_raid1_meta_path_grp0_0} ${thin_meta_raid1_data_path_grp0_0} ${thin_meta_raid1_meta_path_grp0_1} ${thin_meta_raid1_data_path_grp0_1}"
dm_create ${thin_meta_grp0_name} "${table}"

thin_meta_name=$(get_thin_meta_name ${cn_mgr_id} ${vd_id} ${leg_id})
thin_meta_path="/dev/mapper/${thin_meta_name}"
table="0 ${THIN_META_SECTORS} linear ${thin_meta_grp0_path} 0"
dm_create ${thin_meta_name} "${table}"

dd if=/dev/zero of=${thin_data_raid1_meta_path_grp0_0} bs=4k count=1
dd if=/dev/zero of=${thin_data_raid1_meta_path_grp0_1} bs=4k count=1
thin_data_grp0_name=$(get_thin_data_grp_name ${cn_mgr_id} ${vd_id} ${grp0_id})
thin_data_grp0_path="/dev/mapper/${thin_data_grp0_name}"
table="0 ${data_size_sectors} raid raid1 4 0 region_size ${THIN_DATA_REGION_SECTORS} nosync 2 ${thin_data_raid1_meta_path_grp0_0} ${thin_data_raid1_data_path_grp0_0} ${thin_data_raid1_meta_path_grp0_1} ${thin_data_raid1_data_path_grp0_1}"
dm_create ${thin_data_grp0_name} "${table}"

thin_data_name=$(get_thin_data_name ${cn_mgr_id} ${vd_id} ${leg_id})
thin_data_path="/dev/mapper/${thin_data_name}"
table="0 ${data_size_sectors} linear ${thin_data_grp0_path} 0"
dm_create ${thin_data_name} "${table}"

dd if=/dev/zero of=${thin_meta_path} bs=4k count=1
thin_pool_name=$(get_thin_pool_name ${cn_mgr_id} ${vd_id} ${leg_id})
thin_pool_path="/dev/mapper/${thin_pool_name}"
table="0 ${data_size_sectors} thin-pool ${thin_meta_path} ${thin_data_path} ${THIN_DATA_BLOCK_SECTORS} ${THIN_LOW_WATER_MARK} 2 skip_block_zeroing error_if_no_space"
dm_create ${thin_pool_name} "${table}"

dmsetup message ${thin_pool_name} 0 "create_thin ${DEFAULT_THIN_DEV_ID}"

thin_dev_name=$(get_thin_dev_name ${cn_mgr_id} ${vd_id} ${leg_id} ${DEFAULT_THIN_DEV_ID_32BIT})
thin_dev_path="/dev/mapper/${thin_dev_name}"
table="0 ${thin_dev_size_sectors} thin ${thin_pool_path} ${DEFAULT_THIN_DEV_ID}"
dm_create ${thin_dev_name} "${table}"

shift 17

for _ in $(seq ${forward_cn_cnt}); do
    forward_cn_mgr_id=$(format_id ${1})
    forward_cn_host_name=$2
    shift 2
    forward_dev_name=$(get_forward_dev_name ${cn_mgr_id} ${vd_id} ${leg_id} ${DEFAULT_THIN_DEV_ID_32BIT} ${forward_cn_mgr_id})
    forward_dev_path="/dev/mapper/${forward_dev_name}"
    table = "0 ${thin_dev_size_sectors} linear ${thin_dev_path} 0"
    dm_create ${forward_dev_name} "${table}"

    forward_nqn=$(get_forward_nqn ${cn_mgr_id} ${vd_id} ${leg_id} ${DEFAULT_THIN_DEV_ID_32BIT} ${forward_cn_mgr_id})
    forward_host_nqn=$(get_host_nqn ${forward_cn_host_name})
    nvmet_create ${forward_nqn} ${forward_dev_path} ${forward_host_nqn} ${cn_port_num} ${ANA_GROUP_OPTIMIZED} ${DEFAULT_CNTLID_MIN} ${DEFAULT_CNTLID_MAX}
done

echo "done"
