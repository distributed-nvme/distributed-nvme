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
sec0_mgr_id_unformat=${24}
sec0_host_name=${25}
cntlr_cnt=${26}

shift 26

function delete_raid_meta_data_and_disconnect_leg()
{
    l1_mgr_id=$1
    l1_tr_addr=$2
    l1_tr_svc_id=$3
    leg_id=$4

    data_name=$(get_raiddata_name ${prim_mgr_id} ${vd_id} ${leg_id})
    dm_delete ${data_name}

    meta_name=$(get_raidmeta_name ${prim_mgr_id} ${vd_id} ${leg_id})
    dm_delete ${meta_name}

    leg_to_prim_tgt_nqn=$(get_leg_to_prim_tgt_nqn ${l1_mgr_id} ${vd_id} ${leg_id} ${prim_mgr_id})
    nvme_disconnect ${leg_to_prim_tgt_nqn} ${l1_tr_addr} ${l1_tr_svc_id}
}

function delete_raid1_grp()
{
    grp_id=$1

    grp_name=$(get_grp_name ${prim_mgr_id} ${vd_id} ${grp_id})
    dm_delete ${grp_name}
}

for i in $(seq ${cntlr_cnt}); do
    cntlr_mgr_id=$(format_id $1)
    cntlr_host_name=$2
    shift 2

    l2_to_cntlr_tgt_nqn=$(get_l2_to_cntlr_tgt_nqn ${vd_id} ${stripe_id} ${DEFAULT_THIN_DEV_ID_32BIT} ${cntlr_mgr_id})
    l2_to_cntlr_host_nqn=$(get_host_nqn ${cntlr_host_name})
    nvmet_delete ${l2_to_cntlr_tgt_nqn} ${l2_to_cntlr_host_nqn} ${prim_port_num}

    prim_to_cntlr_name=$(get_prim_to_cntlr_name ${prim_mgr_id} ${vd_id} ${stripe_id} ${DEFAULT_THIN_DEV_ID_32BIT} ${cntlr_mgr_id})
    dm_delete ${prim_to_cntlr_name}
done

if [ "${sec0_mgr_id_unformat}" != "-" ]; then
    sec0_mgr_id=$(format_id ${sec0_mgr_id_unformat})
    prim_to_sec0_tgt_nqn=$(get_prim_to_sec_tgt_nqn ${prim_mgr_id} ${vd_id} ${stripe_id} ${DEFAULT_THIN_DEV_ID_32BIT} ${sec0_mgr_id})
    prim_to_sec0_host_nqn=$(get_host_nqn ${sec0_host_name})
    nvmet_delete ${prim_to_sec0_tgt_nqn} ${prim_to_sec0_host_nqn} ${prim_port_num}

    prim_to_sec0_name=$(get_prim_to_sec_name ${prim_mgr_id} ${vd_id} ${stripe_id} ${DEFAULT_THIN_DEV_ID_32BIT} ${sec0_mgr_id})
    dm_delete ${prim_to_sec0_name}
fi

thindev_name=$(get_thindev_name ${prim_mgr_id} ${vd_id} ${stripe_id} ${DEFAULT_THIN_DEV_ID_32BIT})
dm_delete ${thindev_name}

thinpool_name=$(get_thinpool_name ${prim_mgr_id} ${vd_id} ${stripe_id})
dm_delete ${thinpool_name}

thindata_name=$(get_thindata_name ${prim_mgr_id} ${vd_id} ${stripe_id})
dm_delete ${thindata_name}

thinmeta_name=$(get_thinmeta_name ${prim_mgr_id} ${vd_id} ${stripe_id})
dm_delete ${thinmeta_name}

delete_raid1_grp ${thindata_grp0_id}
delete_raid1_grp ${thinmeta_grp0_id}

delete_raid_meta_data_and_disconnect_leg ${thindata_grp0_leg1_l1_mgr_id} ${thindata_grp0_leg1_l1_tr_addr} ${thindata_grp0_leg1_l1_tr_svc_id} ${thindata_grp0_leg1_id}
delete_raid_meta_data_and_disconnect_leg ${thindata_grp0_leg0_l1_mgr_id} ${thindata_grp0_leg0_l1_tr_addr} ${thindata_grp0_leg0_l1_tr_svc_id} ${thindata_grp0_leg0_id}
delete_raid_meta_data_and_disconnect_leg ${thinmeta_grp0_leg1_l1_mgr_id} ${thinmeta_grp0_leg1_l1_tr_addr} ${thinmeta_grp0_leg1_l1_tr_svc_id} ${thinmeta_grp0_leg1_id}
delete_raid_meta_data_and_disconnect_leg ${thinmeta_grp0_leg0_l1_mgr_id} ${thinmeta_grp0_leg0_l1_tr_addr} ${thinmeta_grp0_leg0_l1_tr_svc_id} ${thinmeta_grp0_leg0_id}

echo "done"
