#!/bin/bash

set -e

CURR_DIR=$(readlink -f $(dirname $0))
source ${CURR_DIR}/common.sh

dn_port_num=$

nvmet_cleanup ${dn_port_num}
