#!/bin/bash

set -e

CURR_DIR=$(readlink -f $(dirname $0))
source ${CURR_DIR}/common.sh

l3_port_num=$1
l3_tr_addr=$2
l3_tr_svc_id=$3

nvmet_prepare ${l3_port_num} ${l3_tr_addr} ${l3_tr_svc_id}

echo "done"
