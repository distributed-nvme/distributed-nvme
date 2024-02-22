#!/bin/bash

set -e

CURR_DIR=$(readlink -f $(dirname $0))
source ${CURR_DIR}/common.sh

cn_port_num=$1
cn_tr_addr=$2
cn_tr_svc_id=$3

nvmet_prepare ${cn_port_num} ${cn_tr_addr} ${cn_tr_svc_id}

echo "done"
