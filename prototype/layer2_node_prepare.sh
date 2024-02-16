#!/bin/bash

set -e

CURR_DIR=$(readlink -f $(dirname $0))
source ${CURR_DIR}/common.sh

l2_port_num=$1
l2_tr_addr=$2
l2_tr_svc_id=$3

nvmet_prepare ${l2_port_num} ${l2_tr_addr} ${l2_tr_svc_id}

echo "done"
