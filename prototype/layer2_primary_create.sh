#!/bin/bash

set -e

CURR_DIR=$(readlink -f $(dirname $0))
source ${CURR_DIR}/common.sh

prim_mgr_id=$(format_id $1)
prim_port_num=$2
prim_host_name=$3
vd_id=$(format_id $4)
stripe_id=$(format_id $5)
thinmeta_grp0_id=$(format_id $6)
thinmeta_grp0_leg0_id=$(format_id $7)
thinmeta_grp0_leg0_l1_mgr_id=$(format_id $8)
thinmeta_grp0_leg0_l1_tr_addr=$9
thinmeta_grp0_leg0_l1_tr_svc_id=${10}
thinmeta_grp0_leg1_id=$(format_id ${11})
thinmeta_grp0_leg1_l1_mgr_id=$(format_id ${12})
thinmeta_grp0_leg1_l1_tr_addr=${13}
thinmeta_grp0_leg1_l1_tr_svc_id=${14}
thindata_grp0_id=$(format_id ${15})
thindata_grp0_leg0_id=$(format_id ${16})
thindata_grp0_leg0_l1_mgr_id=$(format_id ${17})
thindata_grp0_leg0_l1_tr_addr=${18}
thindata_grp0_leg0_l1_tr_svc_id=${19}
thindata_grp0_leg1_id=$(format_id ${20})
thindata_grp0_leg1_l1_mgr_id=$(format_id ${21})
thindata_grp0_leg1_l1_tr_addr=${22}
thindata_grp0_leg1_l1_tr_svc_id=${23}
thinmeta_grp0_raid1meta_mb=${24}
thinmeta_grp0_raid1data_mb=${25}
thindata_grp0_raid1meta_mb=${26}
thindata_grp0_raid1data_mb=${27}
thindev_mb=${28}
sec0_mgr_id_unformat=${29}
sec0_host_name=${30}
cntlr_cnt=${31}

shift 31

thinmeta_grp0_raid1meta_sectors=$((thinmeta_grp0_raid1meta_mb*1024*2))
thinmeta_grp0_raid1data_sectors=$((thinmeta_grp0_raid1data_mb*1024*2))
thindata_grp0_raid1meta_sectors=$((thindata_grp0_raid1meta_mb*1024*2))
thindata_grp0_raid1data_sectors=$((thindata_grp0_raid1data_mb*1024*2))
thindev_sectors=$((thindev_mb*1024*2))

function connect_leg_and_create_raid_meta_data()
{
    l1_mgr_id=$1
    l1_tr_addr=$2
    l1_tr_svc_id=$3
    leg_id=$4
    meta_sectors=$5
    data_sectors=$6

    leg_to_prim_tgt_nqn=$(get_leg_to_prim_tgt_nqn ${l1_mgr_id} ${vd_id} ${leg_id} ${prim_mgr_id})
    leg_to_prim_host_nqn=$(get_host_nqn ${prim_host_name})
    nvme_connect ${leg_to_prim_tgt_nqn} ${l1_tr_addr} ${l1_tr_svc_id} ${leg_to_prim_host_nqn}

    leg_path=$(nvme_dev_path_from_nqn ${leg_to_prim_tgt_nqn})

    meta_name=$(get_raidmeta_name ${prim_mgr_id} ${vd_id} ${leg_id})
    table="0 ${meta_sectors} linear ${leg_path} 0"
    dm_create ${meta_name} "${table}"

    data_name=$(get_raiddata_name ${prim_mgr_id} ${vd_id} ${leg_id})
    table="0 ${data_sectors} linear ${leg_path} ${meta_sectors}"
    dm_create ${data_name} "${table}"
}

function create_raid1_grp()
{
    grp_id=$1
    leg0_id=$2
    leg1_id=$3
    raid1_sectors=$4
    region_sectors=$5

    grp_name=$(get_grp_name ${prim_mgr_id} ${vd_id} ${grp_id})
    leg0_meta_name=$(get_raidmeta_name ${prim_mgr_id} ${vd_id} ${leg0_id})
    leg0_meta_path="/dev/mapper/${leg0_meta_name}"
    leg0_data_name=$(get_raiddata_name ${prim_mgr_id} ${vd_id} ${leg0_id})
    leg0_data_path="/dev/mapper/${leg0_data_name}"
    leg1_meta_name=$(get_raidmeta_name ${prim_mgr_id} ${vd_id} ${leg1_id})
    leg1_meta_path="/dev/mapper/${leg1_meta_name}"
    leg1_data_name=$(get_raiddata_name ${prim_mgr_id} ${vd_id} ${leg1_id})
    leg1_data_path="/dev/mapper/${leg1_data_name}"

    dd if=/dev/zero of=${leg0_meta_path} bs=4k count=1
    dd if=/dev/zero of=${leg1_meta_path} bs=4k count=1

    table="0 ${raid1_sectors} raid raid1 4 0 region_size ${region_sectors} nosync 2 ${leg0_meta_path} ${leg0_data_path} ${leg1_meta_path} ${leg1_data_path}"
    dm_create ${grp_name} "${table}"
}

connect_leg_and_create_raid_meta_data ${thinmeta_grp0_leg0_l1_mgr_id} ${thinmeta_grp0_leg0_l1_tr_addr} ${thinmeta_grp0_leg0_l1_tr_svc_id} ${thinmeta_grp0_leg0_id} ${thinmeta_grp0_raid1meta_sectors} ${thinmeta_grp0_raid1data_sectors}
connect_leg_and_create_raid_meta_data ${thinmeta_grp0_leg1_l1_mgr_id} ${thinmeta_grp0_leg1_l1_tr_addr} ${thinmeta_grp0_leg1_l1_tr_svc_id} ${thinmeta_grp0_leg1_id} ${thinmeta_grp0_raid1meta_sectors} ${thinmeta_grp0_raid1data_sectors}
connect_leg_and_create_raid_meta_data ${thindata_grp0_leg0_l1_mgr_id} ${thindata_grp0_leg0_l1_tr_addr} ${thindata_grp0_leg0_l1_tr_svc_id} ${thindata_grp0_leg0_id} ${thindata_grp0_raid1meta_sectors} ${thindata_grp0_raid1data_sectors}
connect_leg_and_create_raid_meta_data ${thindata_grp0_leg1_l1_mgr_id} ${thindata_grp0_leg1_l1_tr_addr} ${thindata_grp0_leg1_l1_tr_svc_id} ${thindata_grp0_leg1_id} ${thindata_grp0_raid1meta_sectors} ${thindata_grp0_raid1data_sectors}

create_raid1_grp ${thinmeta_grp0_id} ${thinmeta_grp0_leg0_id} ${thinmeta_grp0_leg1_id} ${thinmeta_grp0_raid1data_sectors} ${META_REGION_SECTORS}
create_raid1_grp ${thindata_grp0_id} ${thindata_grp0_leg0_id} ${thindata_grp0_leg1_id} ${thindata_grp0_raid1data_sectors} ${DATA_REGION_SECTORS}

thinmeta_name=$(get_thinmeta_name ${prim_mgr_id} ${vd_id} ${stripe_id})
thinmeta_path="/dev/mapper/${thinmeta_name}"
thinmeta_grp0_name=$(get_grp_name ${prim_mgr_id} ${vd_id} ${thinmeta_grp0_id})
thinmeta_grp0_path="/dev/mapper/${thinmeta_grp0_name}"
table="0 ${thinmeta_grp0_raid1data_sectors} linear ${thinmeta_grp0_path} 0"
dm_create ${thinmeta_name} "${table}"
dd if=/dev/zero of=${thinmeta_path} bs=4k count=1

thindata_name=$(get_thindata_name ${prim_mgr_id} ${vd_id} ${stripe_id})
thindata_path="/dev/mapper/${thindata_name}"
thindata_grp0_name=$(get_grp_name ${prim_mgr_id} ${vd_id} ${thindata_grp0_id})
thindata_grp0_path="/dev/mapper/${thindata_grp0_name}"
table="0 ${thindata_grp0_raid1data_sectors} linear ${thindata_grp0_path} 0"
dm_create ${thindata_name} "${table}"

thinpool_name=$(get_thinpool_name ${prim_mgr_id} ${vd_id} ${stripe_id})
thinpool_path="/dev/mapper/${thinpool_name}"
table="0 ${thindata_grp0_raid1data_sectors} thin-pool ${thinmeta_path} ${thindata_path} ${THIN_DATA_BLOCK_SECTORS} ${THIN_LOW_WATER_MARK} 2 skip_block_zeroing error_if_no_space"
dm_create ${thinpool_name} "${table}"

dmsetup message ${thinpool_name} 0 "create_thin ${DEFAULT_THIN_DEV_ID}"

thindev_name=$(get_thindev_name ${prim_mgr_id} ${vd_id} ${stripe_id} ${DEFAULT_THIN_DEV_ID_32BIT})
thindev_path="/dev/mapper/${thindev_name}"
table="0 ${thindev_sectors} thin ${thinpool_path} ${DEFAULT_THIN_DEV_ID_32BIT}"
dm_create ${thindev_name} "${table}"

if [ "${sec0_mgr_id_unformat}" != "-" ]; then
    sec0_mgr_id=$(format_id ${sec0_mgr_id_unformat})
    prim_to_sec0_name=$(get_prim_to_sec_name ${prim_mgr_id} ${vd_id} ${stripe_id} ${DEFAULT_THIN_DEV_ID_32BIT} ${sec0_mgr_id})
    prim_to_sec0_path="/dev/mapper/${prim_to_sec0_name}"
    table="0 ${thindev_sectors} linear ${thindev_path} 0"
    dm_create ${prim_to_sec0_name} "${table}"

    prim_to_sec0_tgt_nqn=$(get_prim_to_sec_tgt_nqn ${prim_mgr_id} ${vd_id} ${stripe_id} ${DEFAULT_THIN_DEV_ID_32BIT} ${sec0_mgr_id})
    prim_to_sec0_host_nqn=$(get_host_nqn ${sec0_host_name})
    nvmet_create ${prim_to_sec0_tgt_nqn} ${prim_to_sec0_path} ${prim_to_sec0_host_nqn} ${prim_port_num} ${ANA_GROUP_OPTIMIZED}
fi

for i in $(seq ${cntlr_cnt}); do
    cntlr_mgr_id=$(format_id ${1})
    cntlr_host_name=$2
    shift 2
    prim_to_cntlr_name=$(get_prim_to_cntlr_name ${prim_mgr_id} ${vd_id} ${stripe_id} ${DEFAULT_THIN_DEV_ID_32BIT} ${cntlr_mgr_id})
    prim_to_cntlr_path="/dev/mapper/${prim_to_cntlr_name}"
    table="0 ${thindev_sectors} linear ${thindev_path} 0"
    dm_create ${prim_to_cntlr_name} "${table}"

    l2_to_cntlr_tgt_nqn=$(get_l2_to_cntlr_tgt_nqn ${vd_id} ${stripe_id} ${DEFAULT_THIN_DEV_ID_32BIT} ${cntlr_mgr_id})
    l2_to_cntlr_host_nqn=$(get_host_nqn ${cntlr_host_name})
    nvmet_create ${l2_to_cntlr_tgt_nqn} ${prim_to_cntlr_path} ${l2_to_cntlr_host_nqn} ${prim_port_num} ${ANA_GROUP_OPTIMIZED} ${PRIM_CNTLID_MIN} ${PRIM_CNTLID_MAX}
done

echo "done"
