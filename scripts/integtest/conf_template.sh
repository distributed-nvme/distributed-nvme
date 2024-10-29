#!/bin/bash

# Do not use the default port to avoid
# the confliction between the minikub default settings.
ETCD_PORT=2479
ETCD_PEER_PORT=2489

WORK_DIR=/tmp/dnvtest

LOOP_NAME0="/dev/loop240"
LOOP_NAME1="/dev/loop241"

HOST_NQN="nqn.2024-01.io.dnv:test:host0"
HOST_ID="ed6850d5-1ac8-4085-a0e9-fc7ebd860649"
