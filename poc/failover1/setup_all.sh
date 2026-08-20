#!/bin/bash
# setup_all.sh -- build the topology bottom-up (each layer needs the one below).
set -e
cd "$(dirname "$0")"; . ./nodes.sh
ssh yupeng@$DN0   "cd $REMOTE && ./dn0_setup.sh"
ssh yupeng@$CN0   "cd $REMOTE && ./cn_setup.sh cn0"
ssh yupeng@$CN1   "cd $REMOTE && ./cn_setup.sh cn1"
ssh yupeng@$HOST0 "cd $REMOTE && ./host0_setup.sh"
