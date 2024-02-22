#!/bin/bash

set -e

CURR_DIR=$(readlink -f $(dirname $0))
source ${CURR_DIR}/common.sh

dn_mgr_id=$(format_id $1)
dn_port_num=$2
pd_path=$3
vd_id=$(format_id $4)
ld_id=$(format_id $5)
ld_start_mb=$6
ld_size_mb=$7
cn_mgr_id=$(format_id $8)
cn_host_name=$9

ld_start_sectors=$((ld_start_mb*1024*2))
ld_size_sectors=$((ld_size_mb*1024*2))

ld_dev_name=$(get_ld_dev_name ${dn_mgr_id} ${vd_id} ${ld_id})
ld_dev_path="/dev/mapper/${ld_dev_name}"
table="0 ${ld_size_sectors} linear ${pd_path} ${ld_start_sectors}"
dm_create ${ld_dev_name} "${table}"

ld_to_leg_nqn=$(get_ld_to_leg_nqn ${dn_mgr_id} ${vd_id} ${ld_id} ${cn_mgr_id})
cn_host_nqn=$(get_host_nqn ${cn_host_name})
nvmet_create ${ld_to_leg_nqn} ${ld_dev_path} ${cn_host_nqn} ${dn_port_num} ${ANA_GROUP_OPTIMIZED} ${DEFAULT_CNTLID_MIN} ${DEFAULT_CNTLID_MAX}

echo "done"
