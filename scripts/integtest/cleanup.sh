#!/bin/bash

set -e

CURR_DIR=$(readlink -f $(dirname $0))
source $CURR_DIR/conf.sh
source $CURR_DIR/utils.sh

cleanup

sleep 1

force_cleanup

sleep 1

cleanup_check

echo "done"
