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

function cleanup_nvmet_by_flag() {
    nqn_flag=$1
    nqn_prefix="nqn.2024-01.io.dnv:$nqn_flag"

    for ss in /sys/kernel/config/nvmet/subsystems/*; do
        if echo $ss | grep -q $nqn_prefix; then

            # delete ns
            for ns in $ss/namespaces/*; do
                if echo $ns | grep -q '[0-9]$'; then
                    echo "remove ns: $ns"
                    sudo rmdir $ns
                fi
            done

            # delete host
            for h in $ss/allowed_hosts/*; do
                if echo $h | grep -q 'allowed_hosts/nqn'; then
                    echo "remove host: $h"
                    sudo unlink $h
                fi
            done

            # delete ss
            echo "remove ss: $ss"
            sudo rmdir $ss
        fi
    done
}

function cleanup_nvmet() {
    for p in /sys/kernel/config/nvmet/ports/*; do
        if echo $p | grep -q '[0-9]$'; then
            for ss in $p/subsystems/*; do
                if echo $ss | grep -q "nqn"; then
                    echo "remove $ss from $p"
                    sudo unlink $ss
                fi
            done
            echo "remove port $p"
            sudo rmdir $p
        fi
    done

    cleanup_nvmet_by_flag "0000"
    cleanup_nvmet_by_flag "1100"
    cleanup_nvmet_by_flag "1200"
    cleanup_nvmet_by_flag "1000"
}

function cleanup() {
    echo "nvme disconnect-all"
    sudo nvme disconnect-all
    echo "cleanup nvmet"
    cleanup_nvmet
    sudo dmsetup remove_all --deferred
    set +e
    echo "stop dnvapi"
    ps -f -C dnvapi > /dev/null && killall dnvapi
    echo "stop dnvworker"
    ps -f -C dnvworker > /dev/null && killall dnvworker
    echo "stop dnvagent"
    ps -f -C dnvagent > /dev/null && sudo killall dnvagent
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
    echo "cleanup nvmet"
    cleanup_nvmet
    sudo dmsetup remove_all --deferred
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

function verify_rsp_msg() {
    rsp=$1
    target_msg=$2
    msg=$(echo $rsp | jq -rM '.reply_info.reply_msg')
    if [ "$msg" != "$target_msg" ]; then
        echo "Msg mismatch"
        exit 1;
    fi
}

function verify_rsp_code() {
    rsp=$1
    target_code=$2
    code=$(echo $rsp | jq -rM '.reply_info.reply_code')
    if [ "$code" != "$target_code" ]; then
        echo "Code mismatch"
        exit 1;
    fi
}
