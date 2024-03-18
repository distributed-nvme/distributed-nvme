#!/bin/bash

DN0_IP_ADDR=192.168.122.195
CN0_IP_ADDR=192.168.122.168
CN1_IP_ADDR=192.168.122.180
HOST_IP_ADDR=192.168.122.53

PD_PATH="/dev/loop240"
LD_NAME="ld0"
LD_PATH="/dev/mapper/${LD_NAME}"
DEV_SIZE_MB=2048
DEV_SIZE_SECTORS=$((DEV_SIZE_MB*2048))

NVMET_PATH="/sys/kernel/config/nvmet"
LD_NQN="nqn.2024-03.org.dnv:ld0"
LD_PORT=1
LD_TR_SVC_ID=10000
CN0_NQN="nqn.2024-03.org.dnv:cn0"
CN1_NQN="nqn.2024-03.org.dnv:cn1"

LD_CNTLID_MIN=10000
LD_CNTLID_MAX=19999
LD_UUID="3dce5ec5-0c82-44ec-95e3-8a94e0f83145"

CN0_PORT=1
CN0_TR_SVC_ID=20000
CN0_CNTLID_MIN=20000
CN0_CNTLID_MAX=24999
CN1_PORT=1
CN1_TR_SVC_ID=20000
CN1_CNTLID_MIN=25000
CN1_CNTLID_MAX=29999

VD_NQN="nqn.2024-03.org.dnv:vd0"
VD_UUID="c10bf6ed-cc45-4ccf-8fbe-9156050fb077"
VD_DEV_NAME="vd_dev"
VD_DEV_PATH="/dev/mapper/${VD_DEV_NAME}"
CN_NVME_DEV_PATH="/dev/disk/by-id/nvme-uuid.${LD_UUID}"
HOST0_NQN="nqn.2024-03.org.dnv:host0"
HOST_NVME_DEV_PATH="/dev/disk/by-id/nvme-uuid.${VD_UUID}"


# DN0
sudo mkdir "${NVMET_PATH}/ports/${LD_PORT}"
sudo mkdir "${NVMET_PATH}/subsystems/${LD_NQN}"
sudo mkdir "${NVMET_PATH}/hosts/${CN0_NQN}"
sudo mkdir "${NVMET_PATH}/hosts/${CN1_NQN}"

sudo dmsetup create ${LD_NAME} --table "0 ${DEV_SIZE_SECTORS} linear ${PD_PATH} 0"
sudo dd if=/dev/zero of=${LD_PATH} bs=4M count=1

sudo bash -c "echo tcp > ${NVMET_PATH}/ports/${LD_PORT}/addr_trtype"
sudo bash -c "echo ipv4 > ${NVMET_PATH}/ports/${LD_PORT}/addr_adrfam"
sudo bash -c "echo ${DN0_IP_ADDR} > ${NVMET_PATH}/ports/${LD_PORT}/addr_traddr"
sudo bash -c "echo ${LD_TR_SVC_ID} > ${NVMET_PATH}/ports/${LD_PORT}/addr_trsvcid"

sudo mkdir "${NVMET_PATH}/subsystems/${LD_NQN}/namespaces/1"

sudo bash -c "echo ${LD_CNTLID_MIN} > ${NVMET_PATH}/subsystems/${LD_NQN}/attr_cntlid_min"
sudo bash -c "echo ${LD_CNTLID_MAX} > ${NVMET_PATH}/subsystems/${LD_NQN}/attr_cntlid_max"

sudo bash -c "echo 0 > ${NVMET_PATH}/subsystems/${LD_NQN}/attr_allow_any_host"
sudo ln -s "${NVMET_PATH}/hosts/${CN0_NQN}" "${NVMET_PATH}/subsystems/${LD_NQN}/allowed_hosts/${CN0_NQN}"
# sudo ln -s "${NVMET_PATH}/hosts/${CN1_NQN}" "${NVMET_PATH}/subsystems/${LD_NQN}/allowed_hosts/${CN1_NQN}"

sudo bash -c "echo ${LD_PATH} > ${NVMET_PATH}/subsystems/${LD_NQN}/namespaces/1/device_path"
sudo bash -c "echo 1 > ${NVMET_PATH}/subsystems/${LD_NQN}/namespaces/1/ana_grpid"
sudo bash -c "echo ${LD_UUID} > ${NVMET_PATH}/subsystems/${LD_NQN}/namespaces/1/device_nguid"
sudo bash -c "echo ${LD_UUID} > ${NVMET_PATH}/subsystems/${LD_NQN}/namespaces/1/device_uuid"
sudo bash -c "echo 1 > ${NVMET_PATH}/subsystems/${LD_NQN}/namespaces/1/enable"

sudo ln -s "${NVMET_PATH}/subsystems/${LD_NQN}" "${NVMET_PATH}/ports/${LD_PORT}/subsystems/${LD_NQN}"


# CN0
sudo mkdir "${NVMET_PATH}/ports/${CN0_PORT}"
sudo mkdir "${NVMET_PATH}/subsystems/${VD_NQN}"
sudo mkdir "${NVMET_PATH}/hosts/${HOST0_NQN}"

sudo nvme connect --nqn "${LD_NQN}" --transport "tcp" --traddr "${DN0_IP_ADDR}" --trsvcid "${LD_TR_SVC_ID}" --hostnqn "${CN0_NQN}"

sudo bash -c "echo tcp > ${NVMET_PATH}/ports/${CN0_PORT}/addr_trtype"
sudo bash -c "echo ipv4 > ${NVMET_PATH}/ports/${CN0_PORT}/addr_adrfam"
sudo bash -c "echo ${CN0_IP_ADDR} > ${NVMET_PATH}/ports/${CN0_PORT}/addr_traddr"
sudo bash -c "echo ${CN0_TR_SVC_ID} > ${NVMET_PATH}/ports/${CN0_PORT}/addr_trsvcid"
sudo bash -c "echo optimized > ${NVMET_PATH}/ports/${CN1_PORT}/ana_groups/1/ana_state"

sudo mkdir "${NVMET_PATH}/subsystems/${VD_NQN}/namespaces/1"

sudo bash -c "echo ${CN0_CNTLID_MIN} > ${NVMET_PATH}/subsystems/${VD_NQN}/attr_cntlid_min"
sudo bash -c "echo ${CN0_CNTLID_MAX} > ${NVMET_PATH}/subsystems/${VD_NQN}/attr_cntlid_max"

sudo bash -c "echo 0 > ${NVMET_PATH}/subsystems/${VD_NQN}/attr_allow_any_host"
sudo ln -s "${NVMET_PATH}/hosts/${HOST0_NQN}" "${NVMET_PATH}/subsystems/${VD_NQN}/allowed_hosts/${HOST0_NQN}"

sudo dmsetup create ${VD_DEV_NAME} --table "0 ${DEV_SIZE_SECTORS} linear ${CN_NVME_DEV_PATH} 0"

sudo bash -c "echo ${VD_DEV_PATH} > ${NVMET_PATH}/subsystems/${VD_NQN}/namespaces/1/device_path"
sudo bash -c "echo 1 > ${NVMET_PATH}/subsystems/${VD_NQN}/namespaces/1/ana_grpid"
sudo bash -c "echo ${VD_UUID} > ${NVMET_PATH}/subsystems/${VD_NQN}/namespaces/1/device_nguid"
sudo bash -c "echo ${VD_UUID} > ${NVMET_PATH}/subsystems/${VD_NQN}/namespaces/1/device_uuid"
sudo bash -c "echo 1 > ${NVMET_PATH}/subsystems/${VD_NQN}/namespaces/1/enable"

sudo ln -s "${NVMET_PATH}/subsystems/${VD_NQN}" "${NVMET_PATH}/ports/${LD_PORT}/subsystems/${VD_NQN}"

# CN1
sudo mkdir "${NVMET_PATH}/ports/${CN1_PORT}"
sudo mkdir "${NVMET_PATH}/subsystems/${VD_NQN}"
sudo mkdir "${NVMET_PATH}/hosts/${HOST0_NQN}"

sudo dmsetup create myerror --table "0 ${DEV_SIZE_SECTORS} error"

sudo bash -c "echo tcp > ${NVMET_PATH}/ports/${CN1_PORT}/addr_trtype"
sudo bash -c "echo ipv4 > ${NVMET_PATH}/ports/${CN1_PORT}/addr_adrfam"
sudo bash -c "echo ${CN1_IP_ADDR} > ${NVMET_PATH}/ports/${CN1_PORT}/addr_traddr"
sudo bash -c "echo ${CN1_TR_SVC_ID} > ${NVMET_PATH}/ports/${CN1_PORT}/addr_trsvcid"
sudo bash -c "echo inaccessible > ${NVMET_PATH}/ports/${CN1_PORT}/ana_groups/1/ana_state"

sudo mkdir "${NVMET_PATH}/subsystems/${VD_NQN}/namespaces/1"

sudo dmsetup create ${VD_DEV_NAME} --table "0 ${DEV_SIZE_SECTORS} delay /dev/mapper/myerror 0 100"

sudo bash -c "echo ${CN1_CNTLID_MIN} > ${NVMET_PATH}/subsystems/${VD_NQN}/attr_cntlid_min"
sudo bash -c "echo ${CN1_CNTLID_MAX} > ${NVMET_PATH}/subsystems/${VD_NQN}/attr_cntlid_max"

sudo bash -c "echo 0 > ${NVMET_PATH}/subsystems/${VD_NQN}/attr_allow_any_host"
sudo ln -s "${NVMET_PATH}/hosts/${HOST0_NQN}" "${NVMET_PATH}/subsystems/${VD_NQN}/allowed_hosts/${HOST0_NQN}"

sudo bash -c "echo ${VD_DEV_PATH} > ${NVMET_PATH}/subsystems/${VD_NQN}/namespaces/1/device_path"
sudo bash -c "echo 1 > ${NVMET_PATH}/subsystems/${VD_NQN}/namespaces/1/ana_grpid"
sudo bash -c "echo ${VD_UUID} > ${NVMET_PATH}/subsystems/${VD_NQN}/namespaces/1/device_nguid"
sudo bash -c "echo ${VD_UUID} > ${NVMET_PATH}/subsystems/${VD_NQN}/namespaces/1/device_uuid"
sudo bash -c "echo 1 > ${NVMET_PATH}/subsystems/${VD_NQN}/namespaces/1/enable"

sudo dmsetup suspend ${VD_DEV_NAME}

sudo ln -s "${NVMET_PATH}/subsystems/${VD_NQN}" "${NVMET_PATH}/ports/${LD_PORT}/subsystems/${VD_NQN}"

# HOST0
sudo nvme connect --nqn "${VD_NQN}" --transport "tcp" --traddr "${CN0_IP_ADDR}" --trsvcid "${CN0_TR_SVC_ID}" --hostnqn "${HOST0_NQN}"
sudo nvme connect --nqn "${VD_NQN}" --transport "tcp" --traddr "${CN1_IP_ADDR}" --trsvcid "${CN1_TR_SVC_ID}" --hostnqn "${HOST0_NQN}"
sudo mkfs.ext4 "${HOST_NVME_DEV_PATH}"
sudo mount ${HOST_NVME_DEV_PATH} /mnt
sudo dd if=/dev/zero of=/mnt/t0.img bs=4k count=1 oflag=direct

# CN0
sudo iptables -I INPUT -s ${HOST_IP_ADDR} -j DROP

# DN0
sudo unlink "${NVMET_PATH}/ports/${LD_PORT}/subsystems/${LD_NQN}"
sudo unlink "${NVMET_PATH}/subsystems/${LD_NQN}/allowed_hosts/${CN0_NQN}"
sudo ln -s "${NVMET_PATH}/subsystems/${LD_NQN}" "${NVMET_PATH}/ports/${LD_PORT}/subsystems/${LD_NQN}"
sudo ln -s "${NVMET_PATH}/hosts/${CN1_NQN}" "${NVMET_PATH}/subsystems/${LD_NQN}/allowed_hosts/${CN1_NQN}"

# CN1
sudo bash -c "echo change > ${NVMET_PATH}/ports/${CN1_PORT}/ana_groups/1/ana_state"
sudo nvme connect --nqn "${LD_NQN}" --transport "tcp" --traddr "${DN0_IP_ADDR}" --trsvcid "${LD_TR_SVC_ID}" --hostnqn "${CN1_NQN}"
sudo dmsetup resume ${VD_DEV_NAME}
sudo dmsetup suspend ${VD_DEV_NAME}
sudo dmsetup load ${VD_DEV_NAME} --table "0 ${DEV_SIZE_SECTORS} linear ${CN_NVME_DEV_PATH} 0"
sudo dmsetup resume ${VD_DEV_NAME}
sudo bash -c "echo optimized > ${NVMET_PATH}/ports/${CN1_PORT}/ana_groups/1/ana_state"

# CN0
sudo bash -c "echo inaccessible > ${NVMET_PATH}/ports/${CN0_PORT}/ana_groups/1/ana_state"
sudo dmsetup create myerror --table "0 ${DEV_SIZE_SECTORS} error"
sudo dmsetup suspend ${VD_DEV_NAME}
sudo dmsetup load ${VD_DEV_NAME} --table "0 ${DEV_SIZE_SECTORS} delay /dev/mapper/myerror 0 100"
sudo dmsetup resume ${VD_DEV_NAME}
sudo iptables -F
sudo nvme disconnect --nqn ${LD_NQN}

# HOST0
sudo umount /mnt
sudo nvme disconnect --nqn ${VD_NQN}

# CN0
sudo rmdir "${NVMET_PATH}/subsystems/${VD_NQN}/namespaces/1"
sudo unlink "${NVMET_PATH}/ports/${CN0_PORT}/subsystems/${VD_NQN}"
sudo unlink "${NVMET_PATH}/subsystems/${VD_NQN}/allowed_hosts/${HOST0_NQN}"
sudo rmdir "${NVMET_PATH}/ports/${CN0_PORT}"
sudo rmdir "${NVMET_PATH}/subsystems/${VD_NQN}"
sudo rmdir "${NVMET_PATH}/hosts/${HOST0_NQN}"

sudo dmsetup remove ${VD_DEV_NAME}
sudo dmsetup remove myerror

# CN1
sudo rmdir "${NVMET_PATH}/subsystems/${VD_NQN}/namespaces/1"
sudo unlink "${NVMET_PATH}/ports/${CN1_PORT}/subsystems/${VD_NQN}"
sudo unlink "${NVMET_PATH}/subsystems/${VD_NQN}/allowed_hosts/${HOST0_NQN}"
sudo rmdir "${NVMET_PATH}/ports/${CN1_PORT}"
sudo rmdir "${NVMET_PATH}/subsystems/${VD_NQN}"
sudo rmdir "${NVMET_PATH}/hosts/${HOST0_NQN}"

sudo dmsetup remove ${VD_DEV_NAME}
sudo dmsetup remove myerror

sudo nvme disconnect --nqn ${LD_NQN}

# DN0
sudo rmdir "${NVMET_PATH}/subsystems/${LD_NQN}/namespaces/1"
sudo unlink "${NVMET_PATH}/ports/${LD_PORT}/subsystems/${LD_NQN}"
sudo unlink "${NVMET_PATH}/subsystems/${LD_NQN}/allowed_hosts/${CN1_NQN}"
sudo rmdir "${NVMET_PATH}/ports/${LD_PORT}"
sudo rmdir "${NVMET_PATH}/subsystems/${LD_NQN}"
sudo rmdir "${NVMET_PATH}/hosts/${CN0_NQN}"
sudo rmdir "${NVMET_PATH}/hosts/${CN1_NQN}"

sudo dmsetup remove ${LD_NAME}
