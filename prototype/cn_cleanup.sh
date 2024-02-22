#!/bin/bash

set -e

CURR_DIR=$(readlink -f $(dirname $0))
source ${CURR_DIR}/common.sh

cn_port_num=$1

nvmet_cleanup ${cn_port_num}
