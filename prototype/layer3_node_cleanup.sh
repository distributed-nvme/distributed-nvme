#!/bin/bash

set -e

CURR_DIR=$(readlink -f $(dirname $0))
source ${CURR_DIR}/common.sh

l3_port_num=$1

nvmet_cleanup ${l3_port_num}

echo "done"
