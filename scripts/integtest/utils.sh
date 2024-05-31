#!/bin/bash

function retry() {
    cmd=$@
    max_retry=600
    retry_cnt=0
    set +e
    while true; do
        ret=$($cmd 2>&1)
        if [ "$ret" == "" ]; then
            set -e
            return
        fi
        if [ $retry_cnt -ge $max_retry ]; then
            echo "failed"
            exit 1
        fi
        sleep 1
        ((retry_cnt=retry_cnt+1))
    done
}

function cleanup() {
    set +e
    echo "nvme disconnect-all"
    sudo nvme disconnect-all
    echo "stop dnvapi"
    ps -f -C dnvapi > /dev/null && killall dnvapi
    echo "stop dnvworker"
    ps -f -C dnvworker > /dev/null && killall dnvworker
    echo "stop dnvagent"
    ps -f -C dnvagent > /dev/null && killall dnvagent
    echo "stop etcd"
    ps -f -C etcd > /dev/null && killall etcd
    echo "stop loop devices"
    losetup $LOOP_NAME0 > /dev/null 2>&1 && retry sudo losetup --detach $LOOP_NAME0
    losetup $LOOP_NAME1 > /dev/null 2>&1 && retry sudo losetup --detach $LOOP_NAME1
    set -e
}

function force_cleanup() {
    set +e
    sudo nvme disconnect-all
    ps -f -C dnvapi > /dev/null && killall -9 dnvapi
    ps -f -C dnvworker > /dev/null && killall -9 dnvworker
    ps -f -C dnvagent > /dev/null && killall -9 agent
    ps -f -C etcd > /dev/null && killall -9 etcd
    losetup $LOOP_NAME0 > /dev/null 2>&1 && retry sudo losetup --detach $LOOP_NAME0
    losetup $LOOP_NAME1 > /dev/null 2>&1 && retry sudo losetup --detach $LOOP_NAME1
    set -e
}

function cleanup_check() {
    set +e
    ps -f -C etcd > /dev/null && echo "etcd is still running"
    ps -f -C dnvapi > /dev/null && echo "dnvapi is still running"
    ps -f -C dnvworker > /dev/null && echo "dnvworker is still running"b
    ps -f -C dnvagent > /dev/null && echo "dnvagent is still running"
    losetup /dev/loop240 > /dev/null 2>&1 && echo "loop240 still exist"
    losetup /dev/loop241 > /dev/null 2>&1 && echo "loop241 still exist"
    set -e
}
