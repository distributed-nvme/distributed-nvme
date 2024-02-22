#!/bin/bash

set -e

CURR_DIR=$(readlink -f $(dirname $0))
source ${CURR_DIR}/common.sh

dn_mgr_id=$(format_id $1)
dn_port_num=$2
vd_id=$(format_id $3)
ld_id=$(format_id $4)
cn_mgr_id=$(format_id $5)
cn_host_name=$6

ld_to_leg_nqn=$(get_ld_to_leg_nqn ${dn_mgr_id} ${vd_id} ${ld_id} ${cn_mgr_id})
cn_host_nqn=$(get_host_nqn ${cn_host_name})
nvmet_delete ${ld_to_leg_nqn} ${cn_host_nqn} ${dn_port_num}

ld_dev_name=$(get_ld_dev_name ${dn_mgr_id} ${vd_id} ${ld_id})
dm_delete ${ld_dev_name}

echo "done"
