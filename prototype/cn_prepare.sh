#!/bin/bash

set -e

CURR_DIR=$(readlink -f $(dirname $0))
source ${CURR_DIR}/common.sh

cn_mgr_id=$(format_id $1)
cn_port_num=$2
cn_tr_addr=$3
cn_tr_scv_id=$4

nvmet_prepare ${cn_port_num} ${cn_tr_addr} ${cn_tr_svc_id}

echo "done"
