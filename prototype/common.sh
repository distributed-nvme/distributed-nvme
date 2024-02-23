#!/bin/bash

# Use a random uuid as our uuid namespace
UUID_NAMESPACE="37833e01-35d4-4e5a-b0a1-fff158b9d03b"

RANDOM_PREFIX=$(echo "${UUID_NAMESPACE}" | tr -d "-")
NQN_PREFIX="nqn.2024-01.io.dnv.${RANDOM_PREFIX}"
NVMET_PATH="/sys/kernel/config/nvmet"
NVME_TRTYPE="tcp"
NVME_ADRFAM="ipv4"
ANA_GROUP_OPTIMIZED=1
ANA_GROUP_NON_OPTIMIZED=2
ANA_GROUP_INACCESSIBLE=3
ANA_GROUP_CHANGE=4
ANA_GROUP_PERSISTENT_LOSS=5
NS_ID=1

DEV_PREFIX="dnv.${RANDOM_PREFIX}"
THIN_META_RAID1_META_SECTORS=2048 # 1MB
THIN_META_RAID1_DATA_SECTORS=2048 # 1MB
THIN_DATA_RAID1_META_SECTORS=2048 # 1MB
THIN_META_RAID1_META_START=0
THIN_META_RAID1_DATA_START=$((THIN_META_RAID1_META_START+THIN_META_RAID1_META_SECTORS))
THIN_DATA_RAID1_META_START=$((THIN_META_RAID1_DATA_START+THIN_META_RAID1_DATA_SECTORS))
THIN_DATA_RAID1_DATA_START=$((THIN_DATA_RAID1_META_START+THIN_DATA_RAID1_META_SECTORS))
THIN_META_RAID1_SECTORS=${THIN_META_RAID1_DATA_SECTORS}
THIN_META_SECTORS=${THIN_META_RAID1_DATA_SECTORS}
THIN_META_REGION_SECTORS=2048     # 1MB
THIN_DATA_REGION_SECTORS=8192     # 4MB
THIN_DATA_BLOCK_SECTORS=2048    # 1MB
THIN_LOW_WATER_MARK=10
DEFAULT_THIN_DEV_ID=0
DEFAULT_THIN_DEV_ID_32BIT="00000000"
RAID0_STRIPE_SECTORS=32         # 16KB

DEV_TYPE_LD_DEV="0000"
DEV_TYPE_THIN_META_RAID1_META="1000"
DEV_TYPE_THIN_META_RAID1_DATA="1001"
DEV_TYPE_THIN_DATA_RAID1_META="1002"
DEV_TYPE_THIN_DATA_RAID1_DATA="1003"
DEV_TYPE_THIN_META_GRP="1100"
DEV_TYPE_THIN_DATA_GRP="1101"
DEV_TYPE_THIN_META="1102"
DEV_TYPE_THIN_DATA="1103"
DEV_TYPE_THIN_POOL="1104"
DEV_TYPE_THIN_DEV="1105"
DEV_TYPE_FORWARD_DEV="1106"
DEV_TYPE_FINAL_DEV="1200"

NQN_TYPE_HOST="0000"
NQN_TYPE_LD_TO_LEG="1000"
NQN_TYPE_FORWARD="1100"
NQN_TYPE_FINAL="1200"

DEFAULT_CNTLID_MIN=10000
DEFAULT_CNTLID_MAX=19999

function format_id()
{
    printf "%08x" $1
}

MPATH_MGR_ID=$(format_id 0)

MAX_RETRY=100
RETRY_INTERVAL=0.1

function wait_on_path()
{
    path=$1
    retry_cnt=0
    while true; do
        if [ -e ${path} ]; then
            return
        fi
        if [ $retry_cnt -ge $MAX_RETRY ]; then
            echo "Failed on waiting ${path}"
            exit 1
        fi
        sleep ${RETRY_INTERVAL}
        ((retry_cnt=retry_cnt+1))
    done
}

function nvmet_prepare()
{
    port_num=$1
    tr_addr=$2
    tr_svc_id=$3

    echo "nvmet_prepare: [${port_num}] [${tr_addr}] [${tr_svc_id}]"

    port_path="${NVMET_PATH}/ports/${port_num}"
    mkdir ${port_path}
    wait_on_path ${port_path}

    echo "${NVME_TRTYPE}" > ${port_path}/addr_trtype
    echo "${NVME_ADRFAM}" > ${port_path}/addr_adrfam
    echo "${tr_addr}" > ${port_path}/addr_traddr
    echo "${tr_svc_id}" > ${port_path}/addr_trsvcid

    mkdir "${port_path}/ana_groups/${ANA_GROUP_NON_OPTIMIZED}"
    mkdir "${port_path}/ana_groups/${ANA_GROUP_INACCESSIBLE}"
    mkdir "${port_path}/ana_groups/${ANA_GROUP_CHANGE}"
    mkdir "${port_path}/ana_groups/${ANA_GROUP_PERSISTENT_LOSS}"

    echo "non-optimized" > ${port_path}/ana_groups/${ANA_GROUP_NON_OPTIMIZED}/ana_state
    echo "inaccessible" > ${port_path}/ana_groups/${ANA_GROUP_INACCESSIBLE}/ana_state
    # Use non-optimized instead of change to workaround potential kernel bug
    # echo "change" > ${port_path}/ana_groups/${ANA_GROUP_CHANGE}/ana_state
    echo "non-optimized" > ${port_path}/ana_groups/${ANA_GROUP_CHANGE}/ana_state
    echo "persistent-loss" > ${port_path}/ana_groups/${ANA_GROUP_PERSISTENT_LOSS}/ana_state
}

function nvmet_cleanup()
{
    port_num=$1

    echo "nvmet_cleanup: [${port_num}]"

    port_path="${NVMET_PATH}/ports/${port_num}"

    ana_grp_path="${port_path}/ana_groups/${ANA_GROUP_NON_OPTIMIZED}"
    if [ -e "${ana_grp_path}" ]; then
        echo "remove ${ana_grp_path}"
        rmdir ${ana_grp_path}
    fi
    ana_grp_path="${port_path}/ana_groups/${ANA_GROUP_INACCESSIBLE}"
    if [ -e "${ana_grp_path}" ]; then
        echo "remove ${ana_grp_path}"
        rmdir ${ana_grp_path}
    fi
    ana_grp_path="${port_path}/ana_groups/${ANA_GROUP_CHANGE}"
    if [ -e "${ana_grp_path}" ]; then
        echo "remove ${ana_grp_path}"
        rmdir ${ana_grp_path}
    fi
    ana_grp_path="${port_path}/ana_groups/${ANA_GROUP_PERSISTENT_LOSS}"
    if [ -e "${ana_grp_path}" ]; then
        echo "remove ${ana_grp_path}"
        rmdir ${ana_grp_path}
    fi

    if [ -e ${port_path} ]; then
        echo "remove ${port_path}"
        rmdir ${port_path}
    fi
}

function nvmet_create()
{
    nqn=$1
    dev_path=$2
    host_nqn=$3
    port_num=$4
    ana_group=$5
    attr_cntlid_min="$6"
    attr_cntlid_max="$7"

    echo "nvmet_create [${nqn}] [${dev_path}] [${host_nqn}] [${port_num}] [${ana_group}] [${attr_cntlid_min}] [${attr_cntlid_max}]"

    nqn_path="${NVMET_PATH}/subsystems/${nqn}"
    mkdir ${nqn_path}
    wait_on_path ${nqn_path}

    if [ "${attr_cntlid_min}" != "" ]; then
        echo ${attr_cntlid_min} > "${nqn_path}/attr_cntlid_min"
    fi

    if [ "${attr_cntlid_max}" != "" ]; then
        echo ${attr_cntlid_max} > "${nqn_path}/attr_cntlid_max"
    fi

    ns_path="${nqn_path}/namespaces/${NS_ID}"
    mkdir ${ns_path}
    wait_on_path ${ns_path}

    echo ${dev_path} > "${ns_path}/device_path"
    echo ${ana_group} > "${ns_path}/ana_grpid"
    dev_uuid=$(uuidgen --md5 --namespace "${UUID_NAMESPACE}" --name "${nqn}")
    echo ${dev_uuid} > "${ns_path}/device_nguid"
    echo ${dev_uuid} > "${ns_path}/device_uuid"
    echo 1 > "${ns_path}/enable"

    host_nqn_path="${NVMET_PATH}/hosts/${host_nqn}"
    if [ ! -e ${host_nqn_path} ]; then
        mkdir ${host_nqn_path}
        wait_on_path ${host_nqn_path}
    fi

    echo 0 > ${nqn_path}/attr_allow_any_host

    host_nqn_allowed_path="${nqn_path}/allowed_hosts/${host_nqn}"
    ln -s ${host_nqn_path} ${host_nqn_allowed_path}

    nqn_port_path="${NVMET_PATH}/ports/${port_num}/subsystems/${nqn}"
    ln -s ${nqn_path} ${nqn_port_path}
}

function nvmet_delete()
{
    nqn=$1
    host_nqn=$2
    port_num=$3

    echo "nvmet_delete: [${nqn}] [${host_nqn}] [${port_num}]"

    nqn_path="${NVMET_PATH}/subsystems/${nqn}"
    ns_path="${nqn_path}/namespaces/${NS_ID}"
    host_nqn_allowed_path="${nqn_path}/allowed_hosts/${host_nqn}"
    host_nqn_path="${NVMET_PATH}/hosts/${host_nqn}"
    nqn_port_path="${NVMET_PATH}/ports/${port_num}/subsystems/${nqn}"

    if [ -e "${nqn_port_path}" ]; then
        echo "remove ${nqn_port_path}"
        unlink ${nqn_port_path}
    fi

    if [ -e "${host_nqn_allowed_path}" ]; then
        echo "remove ${host_nqn_allowed_path}"
        unlink ${host_nqn_allowed_path}
    fi

    # if [ -e ${host_nqn_path} ]; then
    #     echo "remove ${host_nqn_path}"
    #     rmdir ${host_nqn_path}
    # fi

    if [ -e ${ns_path} ]; then
        echo "remove ${ns_path}"
        rmdir ${ns_path}
    fi

    if [ -e ${nqn_path} ]; then
        echo "remove ${nqn_path}"
        rmdir ${nqn_path}
    fi
}

function nvme_dev_path_from_nqn()
{
    nqn=$1
    dev_uuid=$(uuidgen --md5 --namespace "${UUID_NAMESPACE}" --name "${nqn}")
    echo "/dev/disk/by-id/nvme-uuid.${dev_uuid}"
}

function nvme_connect()
{
    nqn=$1
    tr_addr=$2
    tr_svc_id=$3
    host_nqn=$4

    echo "nvme_connect: [${nqn}] [${tr_addr}] [${tr_svc_id}] [${host_nqn}]"

    nvme connect --nqn "${nqn}" --transport "${NVME_TRTYPE}" --traddr "${tr_addr}" --trsvcid "${tr_svc_id}" --hostnqn "${host_nqn}"

    dev_uuid=$(uuidgen --md5 --namespace "${UUID_NAMESPACE}" --name "${nqn}")
    dev_path=$(nvme_dev_path_from_nqn ${nqn})
    wait_on_path ${dev_path}
}

function nvme_disconnect()
{
    nqn=$1
    tr_addr=$2
    tr_svc_id=$3

    echo "nvme_disconnect: [${nqn}] [${tr_addr}] [${tr_svc_id}]"

    # nvme 2.x and 1.x have different formats
    is_nvme2=$(nvme list-subsys --output-format json | jq -rM 'if type=="array" then "yes" else "no" end')
    if [ "${is_nvme2}" == "yes" ]; then
        nvme2x_disconnect ${nqn} ${tr_addr} ${tr_svc_id}
    else
        nvme1x_disconnect ${nqn} ${tr_addr} ${tr_svc_id}
    fi
}

function nvme2x_disconnect()
{
    nqn=$1
    tr_addr=$2
    tr_svc_id=$3

    has_path=$(nvme list-subsys --output-format json | jq -rM ".[].Subsystems[] | select(.NQN==\"${nqn}\") | has(\"Paths\")")
    if [ "${has_path}" == "false" ]; then
        nvme disconnect --nqn ${nqn}
        return
    fi

    subsys=$(nvme list-subsys --output-format json | jq -rM ".[].Subsystems[] | select(.NQN==\"${nqn}\")")
    if [ -z "${subsys}" ]; then
        return
    fi

    address="traddr=${tr_addr},trsvcid=${tr_svc_id}"
    nvme_device=$(echo $subsys | jq -rM ".Paths[] | select(.Address | contains(\"${address}\")) | .Name")
    if [ -z "${nvme_device}" ]; then
        return
    fi

    nvme disconnect --device ${nvme_device}
}

function nvme1x_disconnect()
{
    nqn=$1
    tr_addr=$2
    tr_svc_id=$3

    has_subsys=$(nvme list-subsys --output-format json | jq -rM 'has("Subsystems")')
    if [ "${has_subsys}" == "false" ]; then
        return
    fi

    has_path=$(nvme list-subsys --output-format json | jq -rM ".Subsystems[] | select(.NQN==\"${nqn}\") | has(\"Paths\")")
    if [ "${has_path}" == "false" ]; then
        nvme disconnect --nqn ${nqn}
        return
    fi

    subsys=$(nvme list-subsys --output-format json | jq -rM ".Subsystems[] | select(.NQN==\"${nqn}\")")
    if [ -z "${subsys}" ]; then
        return
    fi
    address="traddr=${tr_addr} trsvcid=${tr_svc_id}"
    # nvme_device=$(echo $subsys | jq -rM ".Paths[] | select(.Address==\"$address\") | .Name")
    nvme_device=$(echo $subsys | jq -rM ".Paths[] | select(.Address | contains(\"${address}\")) | .Name")
    if [ -z "${nvme_device}" ]; then
        return
    fi

    nvme disconnect --device ${nvme_device}

    # has_path=$(nvme list-subsys --output-format json | jq -rM ".Subsystems[] | select(.NQN==\"${nqn}\") | has(\"Paths\")")
    # if [ "${has_path}" == "false" ]; then
    #     nvme disconnect --nqn ${nqn}
    # fi
}

function dm_create()
{
    dm_name=$1
    dm_table="$2"

    echo "dm_create: [${dm_name}] [${dm_table}]"

    dm_path="/dev/mapper/${dm_name}"
    dmsetup create ${dm_name} --table "${dm_table}"
    wait_on_path ${dm_path}
}

function dm_delete()
{
    dm_name=$1

    echo "dm_delete: [${dm_name}]"

    if dmsetup status ${dm_name} > /dev/null 2>&1; then
        echo "remove ${dm_name}"
        dmsetup remove ${dm_name}
    fi
}

function get_ld_dev_name()
{
    dn_mgr_id=$1
    vd_id=$2
    ld_id=$3
    echo "${DEV_PREFIX}-${dn_mgr_id}-${vd_id}-${DEV_TYPE_LD_DEV}-${ld_id}"
}

function get_thin_meta_raid1_meta_name()
{
    cn_mgr_id=$1
    vd_id=$2
    ld_id=$3
    echo "${DEV_PREFIX}-${cn_mgr_id}-${vd_id}-${DEV_TYPE_THIN_META_RAID1_META}-${ld_id}"
}

function get_thin_meta_raid1_data_name()
{
    cn_mgr_id=$1
    vd_id=$2
    ld_id=$3
    echo "${DEV_PREFIX}-${cn_mgr_id}-${vd_id}-${DEV_TYPE_THIN_META_RAID1_DATA}-${ld_id}"
}

function get_thin_data_raid1_meta_name()
{
    cn_mgr_id=$1
    vd_id=$2
    ld_id=$3
    echo "${DEV_PREFIX}-${cn_mgr_id}-${vd_id}-${DEV_TYPE_THIN_DATA_RAID1_META}-${ld_id}"
}

function get_thin_data_raid1_data_name()
{
    cn_mgr_id=$1
    vd_id=$2
    ld_id=$3
    echo "${DEV_PREFIX}-${cn_mgr_id}-${vd_id}-${DEV_TYPE_THIN_DATA_RAID1_DATA}-${ld_id}"
}

function get_thin_meta_grp_name()
{
    cn_mgr_id=$1
    vd_id=$2
    grp_id=$3
    echo "${DEV_PREFIX}-${cn_mgr_id}-${vd_id}-${DEV_TYPE_THIN_META_GRP}-${grp_id}"
}

function get_thin_data_grp_name()
{
    cn_mgr_id=$1
    vd_id=$2
    grp_id=$3
    echo "${DEV_PREFIX}-${cn_mgr_id}-${vd_id}-${DEV_TYPE_THIN_DATA_GRP}-${grp_id}"
}

function get_thin_meta_name()
{
    cn_mgr_id=$1
    vd_id=$2
    leg_id=$3
    echo "${DEV_PREFIX}-${cn_mgr_id}-${vd_id}-${DEV_TYPE_THIN_META}-${leg_id}"
}

function get_thin_data_name()
{
    cn_mgr_id=$1
    vd_id=$2
    leg_id=$3
    echo "${DEV_PREFIX}-${cn_mgr_id}-${vd_id}-${DEV_TYPE_THIN_DATA}-${leg_id}"
}

function get_thin_pool_name()
{
    cn_mgr_id=$1
    vd_id=$2
    leg_id=$3
    echo "${DEV_PREFIX}-${cn_mgr_id}-${vd_id}-${DEV_TYPE_THIN_POOL}-${leg_id}"
}

function get_thin_dev_name()
{
    cn_mgr_id=$1
    vd_id=$2
    leg_id=$3
    dev_id=$4
    echo "${DEV_PREFIX}-${cn_mgr_id}-${vd_id}-${DEV_TYPE_THIN_POOL}-${leg_id}-${dev_id}"
}

function get_forward_dev_name()
{
    cn_mgr_id=$1
    vd_id=$2
    leg_id=$3
    dev_id=$4
    forward_cn_mgr_id=$5
    echo "${DEV_PREFIX}-${cn_mgr_id}-${vd_id}-${DEV_TYPE_FORWARD_DEV}-${leg_id}-${dev_id}-${forward_cn_mgr_id}"
}

function get_final_dev_name()
{
    cn_mgr_id=$1
    vd_id=$2
    dev_id=$3
    echo "${DEV_PREFIX}-${cn_mgr_id}-${vd_id}-${DEV_TYPE_FINAL_DEV}-${dev_id}"
}

function get_host_nqn()
{
    host_name=$1
    echo "${NQN_PREFIX}:${NQN_TYPE_HOST}:${host_name}"
}

function get_ld_to_leg_nqn()
{
    dn_mgr_id=$1
    vd_id=$2
    ld_id=$3
    cn_mgr_id=$4
    echo "${NQN_PREFIX}:${dn_mgr_id}:${vd_id}:${NQN_TYPE_LD_TO_LEG}:${ld_id}:${cn_mgr_id}"
}

function get_forward_nqn()
{
    cn_mgr_id=$1
    vd_id=$2
    leg_id=$3
    dev_id=$4
    forward_cn_mgr_id=$5
    echo "${NQN_PREFIX}:${cn_mgr_id}:${vd_id}:${NQN_TYPE_FORWARD}:${leg_id}:${forward_cn_mgr_id}"
}

function get_final_nqn()
{
    vd_id=$1
    dev_id=$2
    echo "${NQN_PREFIX}:${MPATH_MGR_ID}:${vd_id}:${NQN_TYPE_FINAL}:${dev_id}"
}
