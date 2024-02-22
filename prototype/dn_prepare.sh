#!/bin/bash

set -e

CURR_DIR=$(readlink -f $(dirname $0))
source ${CURR_DIR}/common.sh

dn_port_num=$1
dn_tr_addr=$2
dn_tr_svc_id=$3

nvmet_prepare ${dn_port_num} ${dn_tr_addr} ${dn_tr_svc_id}

echo "done"
