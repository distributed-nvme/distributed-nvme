#!/bin/bash

set -e

CURR_DIR=$(readlink -f $(dirname $0))
source ${CURR_DIR}/common.sh

dn_mgr_id=$(format_id $1)
dn_port_num=$2
dn_tr_addr=$3
dn_tr_scv_id=$4

nvmet_prepare ${dn_port_num} ${dn_tr_addr} ${dn_tr_svc_id}

echo "done"
