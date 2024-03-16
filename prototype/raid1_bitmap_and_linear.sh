#!/bin/bash

sudo modprobe nvmet
sudo modprobe nvmet-tcp
sudo modprobe nvme-tcp

target_ip_addr=192.168.0.60
host_ip_addr=192.168.0.186

raid1_meta_mb=4
raid1_data_mb=4194304
raid1_region_mb=32

raid1_meta_sectors=$((raid1_meta_mb*2048))
raid1_data_sectors=$((raid1_data_mb*2048))
raid1_region_sectors=$((raid1_region_mb*2048))

small_linear_cnt=1024
small_linear_mb=1024
small_linear_sectors=$((small_linear_mb*2048))

small_linear_prefix="small_linear"
large_linear_name="large_linear"

small_raid1_cnt=1024
small_meta_mb=1
small_data_mb=4096
small_region_mb=64
small_meta_sectors=$((small_meta_mb*2048))
small_data_sectors=$((small_data_mb*2048))
small_region_sectors=$((small_region_mb*2048))

small_meta_prefix="small_meta_0000000000000000_0000000000000000_0000000000000000_00000000_0000"
small_data_prefix="small_data_0000000000000000_0000000000000000_0000000000000000_00000000_0000"
small_raid1_prefix="small_raid1_0000000000000000_0000000000000000_0000000000000000_00000000_0000"
local_linear_name="local_linear_0000000000000000_0000000000000000_0000000000000000_00000000_0000"

base_nqn="nqn.2024-03.org.dnv:0000000000000000:0000000000000000:0000000000000000:00000000:0000"

local_pd_0_path="/dev/disk/by-path/pci-0000:00:18.0-nvme-1"
local_pd_1_path="/dev/disk/by-path/pci-0000:00:1a.0-nvme-1"

remote_pd_0_path="/dev/disk/by-path/pci-0000:00:1c.0-nvme-1"
remote_pd_1_path="/dev/disk/by-path/pci-0000:00:1e.0-nvme-1"

local_raid1_meta_0_name="local_raid1_meta_0"
local_raid1_meta_0_path="/dev/mapper/${local_raid1_meta_0_name}"
local_raid1_data_0_name="local_raid1_data_0"
local_raid1_data_0_path="/dev/mapper/${local_raid1_data_0_name}"
local_raid1_meta_1_name="local_raid1_meta_1"
local_raid1_meta_1_path="/dev/mapper/${local_raid1_meta_1_name}"
local_raid1_data_1_name="local_raid1_data_1"
local_raid1_data_1_path="/dev/mapper/${local_raid1_data_1_name}"
local_raid1_name="local_raid1_a"
local_raid1_path="/dev/mapper/${local_raid1_name}"

remote_raid1_meta_0_name="remote_raid1_meta_0"
remote_raid1_meta_0_path="/dev/mapper/${remote_raid1_meta_0_name}"
remote_raid1_data_0_name="remote_raid1_data_0"
remote_raid1_data_0_path="/dev/mapper/${remote_raid1_data_0_name}"
remote_raid1_meta_1_name="remote_raid1_meta_1"
remote_raid1_meta_1_path="/dev/mapper/${remote_raid1_meta_1_name}"
remote_raid1_data_1_name="remote_raid1_data_1"
remote_raid1_data_1_path="/dev/mapper/${remote_raid1_data_1_name}"
remote_raid1_name="remote_raid1_a"
remote_raid1_path="/dev/mapper/${remote_raid1_name}"
port=1
nqn="nqn.2014-08.org.nvmexpress:NVMf:test0"
remote_raid1_meta_0_ns=1
remote_raid1_data_0_ns=2
remote_raid1_meta_1_ns=3
remote_raid1_data_1_ns=4
remote_raid1_meta_0_uuid="3dce5ec5-0c82-44ec-95e3-8a94e0f83145"
remote_raid1_data_0_uuid="c10bf6ed-cc45-4ccf-8fbe-9156050fb077"
remote_raid1_meta_1_uuid="573ffd4d-fa8d-4c84-811a-4e9e6d2c5d01"
remote_raid1_data_1_uuid="1d08bec5-c0c0-42ac-ab23-0fd8f7da3e49"

# local mirror

sudo dmsetup create ${local_raid1_meta_0_name} --table "0 ${raid1_meta_sectors} linear ${local_pd_0_path} 0"
sudo dmsetup create ${local_raid1_data_0_name} --table "0 ${raid1_data_sectors} linear ${local_pd_0_path} ${raid1_meta_sectors}"
sudo dmsetup create ${local_raid1_meta_1_name} --table "0 ${raid1_meta_sectors} linear ${local_pd_1_path} 0"
sudo dmsetup create ${local_raid1_data_1_name} --table "0 ${raid1_data_sectors} linear ${local_pd_1_path} ${raid1_meta_sectors}"
sudo dd if=/dev/zero of=${local_raid1_meta_0_path}
sudo dd if=/dev/zero of=${local_raid1_meta_1_path}
sudo dmsetup create ${local_raid1_name} --table "0 ${raid1_data_sectors} raid raid1 4 0 region_size ${raid1_region_sectors} nosync 2 ${local_raid1_meta_0_path} ${local_raid1_data_0_path} ${local_raid1_meta_1_path} ${local_raid1_data_1_path}" && sudo dmsetup status

sudo fio --filename=${local_raid1_path} --name mytest --rw=randwrite --ioengine=libaio --iodepth=4 --bs=4k --numjobs=8 --direct=1 --time_based --runtime=60 --group_reporting --norandommap

sudo dmsetup remove ${local_raid1_name}
sudo dmsetup remove ${local_raid1_meta_0_name}
sudo dmsetup remove ${local_raid1_data_0_name}
sudo dmsetup remove ${local_raid1_meta_1_name}
sudo dmsetup remove ${local_raid1_data_1_name}

# remote mirror

sudo dmsetup create ${remote_raid1_meta_0_name} --table "0 ${raid1_meta_sectors} linear ${remote_pd_0_path} 0"
sudo dmsetup create ${remote_raid1_data_0_name} --table "0 ${raid1_data_sectors} linear ${remote_pd_0_path} ${raid1_meta_sectors}"
sudo dmsetup create ${remote_raid1_meta_1_name} --table "0 ${raid1_meta_sectors} linear ${remote_pd_1_path} 0"
sudo dmsetup create ${remote_raid1_data_1_name} --table "0 ${raid1_data_sectors} linear ${remote_pd_1_path} ${raid1_meta_sectors}"
sudo dd if=/dev/zero of=${remote_raid1_meta_0_path}
sudo dd if=/dev/zero of=${remote_raid1_meta_1_path}

sudo mkdir "/sys/kernel/config/nvmet/ports/${port}"
sudo mkdir "/sys/kernel/config/nvmet/subsystems/${nqn}"
sleep 1
sudo bash -c "echo tcp > /sys/kernel/config/nvmet/ports/${port}/addr_trtype"
sudo bash -c "echo ipv4 > /sys/kernel/config/nvmet/ports/${port}/addr_adrfam"
sudo bash -c "echo ${target_ip_addr} > /sys/kernel/config/nvmet/ports/${port}/addr_traddr"
sudo bash -c "echo 4420 > /sys/kernel/config/nvmet/ports/${port}/addr_trsvcid"
sudo bash -c "echo 1 > /sys/kernel/config/nvmet/subsystems/${nqn}/attr_allow_any_host"
sudo mkdir "/sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/${remote_raid1_meta_0_ns}"
sudo mkdir "/sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/${remote_raid1_data_0_ns}"
sudo mkdir "/sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/${remote_raid1_meta_1_ns}"
sudo mkdir "/sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/${remote_raid1_data_1_ns}"
sleep 1

sudo bash -c "echo ${remote_raid1_meta_0_path} > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/${remote_raid1_meta_0_ns}/device_path"
sudo bash -c "echo ${remote_raid1_meta_0_uuid} > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/${remote_raid1_meta_0_ns}/device_nguid"
sudo bash -c "echo ${remote_raid1_meta_0_uuid} > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/${remote_raid1_meta_0_ns}/device_uuid"
sudo bash -c "echo 1 > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/${remote_raid1_meta_0_ns}/ana_grpid"
sudo bash -c "echo 1 > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/${remote_raid1_meta_0_ns}/enable"

sudo bash -c "echo ${remote_raid1_data_0_path} > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/${remote_raid1_data_0_ns}/device_path"
sudo bash -c "echo ${remote_raid1_data_0_uuid} > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/${remote_raid1_data_0_ns}/device_nguid"
sudo bash -c "echo ${remote_raid1_data_0_uuid} > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/${remote_raid1_data_0_ns}/device_uuid"
sudo bash -c "echo 1 > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/${remote_raid1_data_0_ns}/ana_grpid"
sudo bash -c "echo 1 > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/${remote_raid1_data_0_ns}/enable"

sudo bash -c "echo ${remote_raid1_meta_1_path} > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/${remote_raid1_meta_1_ns}/device_path"
sudo bash -c "echo ${remote_raid1_meta_1_uuid} > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/${remote_raid1_meta_1_ns}/device_nguid"
sudo bash -c "echo ${remote_raid1_meta_1_uuid} > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/${remote_raid1_meta_1_ns}/device_uuid"
sudo bash -c "echo 1 > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/${remote_raid1_meta_1_ns}/ana_grpid"
sudo bash -c "echo 1 > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/${remote_raid1_meta_1_ns}/enable"

sudo bash -c "echo ${remote_raid1_data_1_path} > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/${remote_raid1_data_1_ns}/device_path"
sudo bash -c "echo ${remote_raid1_data_1_uuid} > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/${remote_raid1_data_1_ns}/device_nguid"
sudo bash -c "echo ${remote_raid1_data_1_uuid} > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/${remote_raid1_data_1_ns}/device_uuid"
sudo bash -c "echo 1 > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/${remote_raid1_data_1_ns}/ana_grpid"
sudo bash -c "echo 1 > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/${remote_raid1_data_1_ns}/enable"

sudo ln -s "/sys/kernel/config/nvmet/subsystems/${nqn}" "/sys/kernel/config/nvmet/ports/${port}/subsystems/${nqn}"


sudo nvme connect -t tcp -n ${nqn} -a ${target_ip_addr} -s 4420 --hostnqn nqn.2016-06.io.spdk:host0
sudo dd if=/dev/zero of="/dev/disk/by-id/nvme-uuid.${remote_raid1_meta_0_uuid}"
sudo dd if=/dev/zero of="/dev/disk/by-id/nvme-uuid.${remote_raid1_meta_1_uuid}"
sudo dmsetup create ${remote_raid1_name} --table "0 ${raid1_data_sectors} raid raid1 4 0 region_size ${raid1_region_sectors} nosync 2 /dev/disk/by-id/nvme-uuid.${remote_raid1_meta_0_uuid} /dev/disk/by-id/nvme-uuid.${remote_raid1_data_0_uuid} /dev/disk/by-id/nvme-uuid.${remote_raid1_meta_1_uuid} /dev/disk/by-id/nvme-uuid.${remote_raid1_data_1_uuid}" && sudo dmsetup status

sudo fio --filename=${remote_raid1_path} --name mytest --rw=randwrite --ioengine=libaio --iodepth=4 --bs=4k --numjobs=16 --direct=1 --time_based --runtime=60 --group_reporting --norandommap

sudo dmsetup remove ${remote_raid1_name}
sudo nvme disconnect -n ${nqn}

sudo unlink "/sys/kernel/config/nvmet/ports/${port}/subsystems/${nqn}"
sudo rmdir "/sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/${remote_raid1_meta_0_ns}"
sudo rmdir "/sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/${remote_raid1_data_0_ns}"
sudo rmdir "/sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/${remote_raid1_meta_1_ns}"
sudo rmdir "/sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/${remote_raid1_data_1_ns}"
sleep 1
sudo rmdir "/sys/kernel/config/nvmet/ports/${port}"
sudo rmdir "/sys/kernel/config/nvmet/subsystems/${nqn}"

sudo dmsetup remove ${remote_raid1_meta_0_name}
sudo dmsetup remove ${remote_raid1_data_0_name}
sudo dmsetup remove ${remote_raid1_meta_1_name}
sudo dmsetup remove ${remote_raid1_data_1_name}

# lots of linears

table_str="" && offset=0 && for i in $(seq --equal-width ${small_linear_cnt}); do name="${small_linear_prefix}_$i" && echo ${name} && sudo dmsetup create ${name} --table "0 ${small_linear_sectors} linear ${local_pd_0_path} ${offset}" && table_str="${table_str}${offset} ${small_linear_sectors} linear /dev/mapper/${name} 0\n" && offset=$((offset+small_linear_sectors)); done

echo -e "${table_str}" | sudo dmsetup create ${large_linear_name}

sudo fio --filename="/dev/mapper/${large_linear_name}" --name mytest --rw=randwrite --ioengine=libaio --iodepth=4 --bs=4k --numjobs=8 --direct=1 --time_based --runtime=60 --group_reporting --norandommap

sudo dmsetup remove ${large_linear_name}

for i in $(seq --equal-width ${small_linear_cnt}); do name="${small_linear_prefix}_$i" && echo ${name} && sudo dmsetup remove ${name}; done

# local mirror with lots of linears

table_str="" && offset=0 && linear_offset=0
for i in $(seq --equal-width ${small_raid1_cnt}); do
    small_meta_name_0="${small_meta_prefix}_0_${i}"
    small_meta_path_0="/dev/mapper/${small_meta_name_0}"
    sudo dmsetup create ${small_meta_name_0} --table "0 ${small_meta_sectors} linear ${local_pd_0_path} ${offset}"

    small_meta_name_1="${small_meta_prefix}_1_${i}"
    small_meta_path_1="/dev/mapper/${small_meta_name_1}"
    sudo dmsetup create ${small_meta_name_1} --table "0 ${small_meta_sectors} linear ${local_pd_1_path} ${offset}"

    offset=$((offset+small_meta_sectors))

    small_data_name_0="${small_data_prefix}_0_${i}"
    small_data_path_0="/dev/mapper/${small_data_name_0}"
    sudo dmsetup create ${small_data_name_0} --table "0 ${small_data_sectors} linear ${local_pd_0_path} ${offset}"
   
    small_data_name_1="${small_data_prefix}_1_${i}"
    small_data_path_1="/dev/mapper/${small_data_name_1}"
    sudo dmsetup create ${small_data_name_1} --table "0 ${small_data_sectors} linear ${local_pd_1_path} ${offset}"
    offset=$((offset+small_data_sectors))

    sudo dd if=/dev/zero of=${small_meta_path_0} bs=4k count=1
    sudo dd if=/dev/zero of=${small_meta_path_1} bs=4k count=1

    small_raid1_name="${small_raid1_prefix}_${i}"
    small_raid1_path="/dev/mapper/${small_raid1_name}"
    sudo dmsetup create ${small_raid1_name} --table "0 ${small_data_sectors} raid raid1 4 0 region_size ${small_region_sectors} nosync 2 ${small_meta_path_0} ${small_data_path_0} ${small_meta_path_1} ${small_data_path_1}"

    echo "${small_raid1_name}"

    table_str="${table_str}${linear_offset} ${small_data_sectors} linear ${small_raid1_path} 0\n"
    linear_offset=$((linear_offset+small_data_sectors))
done

echo -e "${table_str}" | sudo dmsetup create ${local_linear_name}

sudo fio --filename="/dev/mapper/${local_linear_name}" --name mytest --rw=randwrite --ioengine=libaio --iodepth=4 --bs=4k --numjobs=8 --direct=1 --time_based --runtime=60 --group_reporting --norandommap

sudo dmsetup remove ${local_linear_name}

for i in $(seq --equal-width ${small_raid1_cnt}); do
    small_raid1_name="${small_raid1_prefix}_${i}"
    echo ${small_raid1_name}
    sudo dmsetup remove ${small_raid1_name}
done

for i in $(seq --equal-width ${small_raid1_cnt}); do
    echo ${i}
    small_meta_name_0="${small_meta_prefix}_0_${i}"
    small_meta_name_1="${small_meta_prefix}_1_${i}"
    small_data_name_0="${small_data_prefix}_0_${i}"
    small_data_name_1="${small_data_prefix}_1_${i}"
    sudo dmsetup remove ${small_meta_name_0}
    sudo dmsetup remove ${small_meta_name_1}
    sudo dmsetup remove ${small_data_name_0}
    sudo dmsetup remove ${small_data_name_1}
done

# remote mirror with lots of linears

offset=0 && port=1 && trsvcid=10000
for i in $(seq --equal-width ${small_raid1_cnt}); do
    nqn="${base_nqn}:${i}"
    sudo mkdir "/sys/kernel/config/nvmet/subsystems/${nqn}"
    sudo mkdir "/sys/kernel/config/nvmet/ports/${port}"

    small_meta_name_0="${small_meta_prefix}_0_${i}"
    small_meta_path_0="/dev/mapper/${small_meta_name_0}"
    sudo dmsetup create ${small_meta_name_0} --table "0 ${small_meta_sectors} linear ${local_pd_0_path} ${offset}"

    small_meta_name_1="${small_meta_prefix}_1_${i}"
    small_meta_path_1="/dev/mapper/${small_meta_name_1}"
    sudo dmsetup create ${small_meta_name_1} --table "0 ${small_meta_sectors} linear ${local_pd_1_path} ${offset}"

    offset=$((offset+small_meta_sectors))

    small_data_name_0="${small_data_prefix}_0_${i}"
    small_data_path_0="/dev/mapper/${small_data_name_0}"
    sudo dmsetup create ${small_data_name_0} --table "0 ${small_data_sectors} linear ${local_pd_0_path} ${offset}"
   
    small_data_name_1="${small_data_prefix}_1_${i}"
    small_data_path_1="/dev/mapper/${small_data_name_1}"
    sudo dmsetup create ${small_data_name_1} --table "0 ${small_data_sectors} linear ${local_pd_1_path} ${offset}"
    offset=$((offset+small_data_sectors))

    sudo mkdir "/sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/1"
    sudo mkdir "/sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/2"
    sudo mkdir "/sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/3"
    sudo mkdir "/sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/4"

    sudo bash -c "echo tcp > /sys/kernel/config/nvmet/ports/${port}/addr_trtype"
    sudo bash -c "echo ipv4 > /sys/kernel/config/nvmet/ports/${port}/addr_adrfam"
    sudo bash -c "echo ${target_ip_addr} > /sys/kernel/config/nvmet/ports/${port}/addr_traddr"
    sudo bash -c "echo ${trsvcid} > /sys/kernel/config/nvmet/ports/${port}/addr_trsvcid"

    sudo bash -c "echo 1 > /sys/kernel/config/nvmet/subsystems/${nqn}/attr_allow_any_host"

    echo ${i}

    dev_uuid=$(uuidgen --md5 --namespace @dns --name "${i}_meta_0")
    sudo bash -c "echo ${small_meta_path_0} > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/1/device_path"
    sudo bash -c "echo ${dev_uuid} > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/1/device_nguid"
    sudo bash -c "echo ${dev_uuid} > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/1/device_uuid"
    sudo bash -c "echo 1 > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/1/ana_grpid"
    sudo bash -c "echo 1 > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/1/enable"

    dev_uuid=$(uuidgen --md5 --namespace @dns --name "${i}_data_0")
    sudo bash -c "echo ${small_data_path_0} > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/2/device_path"
    sudo bash -c "echo ${dev_uuid} > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/2/device_nguid"
    sudo bash -c "echo ${dev_uuid} > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/2/device_uuid"
    sudo bash -c "echo 1 > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/2/ana_grpid"
    sudo bash -c "echo 1 > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/2/enable"

    dev_uuid=$(uuidgen --md5 --namespace @dns --name "${i}_meta_1")
    sudo bash -c "echo ${small_meta_path_1} > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/3/device_path"
    sudo bash -c "echo ${dev_uuid} > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/3/device_nguid"
    sudo bash -c "echo ${dev_uuid} > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/3/device_uuid"
    sudo bash -c "echo 1 > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/3/ana_grpid"
    sudo bash -c "echo 1 > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/3/enable"

    dev_uuid=$(uuidgen --md5 --namespace @dns --name "${i}_data_1")
    sudo bash -c "echo ${small_data_path_1} > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/4/device_path"
    sudo bash -c "echo ${dev_uuid} > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/4/device_nguid"
    sudo bash -c "echo ${dev_uuid} > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/4/device_uuid"
    sudo bash -c "echo 1 > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/4/ana_grpid"
    sudo bash -c "echo 1 > /sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/4/enable"

    sudo ln -s "/sys/kernel/config/nvmet/subsystems/${nqn}" "/sys/kernel/config/nvmet/ports/${port}/subsystems/${nqn}"

    port=$((port+1))
    trsvcid=$((trsvcid+1))
done

trsvcid=10000
for i in $(seq --equal-width ${small_raid1_cnt}); do
    nqn="${base_nqn}:${i}"
    echo ${nqn}
    sudo nvme connect -t tcp -n ${nqn} -a ${target_ip_addr} -s ${trsvcid} --hostnqn nqn.2016-06.io.spdk:host0
    trsvcid=$((trsvcid+1))
done

table_str="" && linear_offset=0
for i in $(seq --equal-width ${small_raid1_cnt}); do
    meta_0_uuid=$(uuidgen --md5 --namespace @dns --name "${i}_meta_0")
    meta_0_path="/dev/disk/by-id/nvme-uuid.${meta_0_uuid}"
    data_0_uuid=$(uuidgen --md5 --namespace @dns --name "${i}_data_0")
    data_0_path="/dev/disk/by-id/nvme-uuid.${data_0_uuid}"
    meta_1_uuid=$(uuidgen --md5 --namespace @dns --name "${i}_meta_1")
    meta_1_path="/dev/disk/by-id/nvme-uuid.${meta_1_uuid}"
    data_1_uuid=$(uuidgen --md5 --namespace @dns --name "${i}_data_1")
    data_1_path="/dev/disk/by-id/nvme-uuid.${data_1_uuid}"
    sudo dd if=/dev/zero of="${meta_0_path}" bs=4k count=1 > /dev/null 2>&1
    sudo dd if=/dev/zero of="${meta_1_path}" bs=4k count=1 > /dev/null 2>&1

    small_raid1_name="small_raid1_prefix_${i}"
    small_raid1_path="/dev/mapper/${small_raid1_name}"
    sudo dmsetup create ${small_raid1_name} --table "0 ${small_data_sectors} raid raid1 4 0 region_size ${small_region_sectors} nosync 2 ${meta_0_path} ${data_0_path} ${meta_1_path} ${data_1_path}"

    echo "${small_raid1_name}"

    table_str="${table_str}${linear_offset} ${small_data_sectors} linear ${small_raid1_path} 0\n"
    linear_offset=$((linear_offset+small_data_sectors))
done

echo -e "${table_str}" | sudo dmsetup create ${local_linear_name}

sudo fio --filename="/dev/mapper/${local_linear_name}" --name mytest --rw=randwrite --ioengine=libaio --iodepth=4 --bs=4k --numjobs=16 --direct=1 --time_based --runtime=60 --group_reporting --norandommap

sudo dmsetup remove ${local_linear_name}

for i in $(seq --equal-width ${small_raid1_cnt}); do
    small_raid1_name="small_raid1_prefix_${i}"
    echo ${small_raid1_name}
    sudo dmsetup remove ${small_raid1_name}
done

for i in $(seq --equal-width ${small_raid1_cnt}); do
    nqn="${base_nqn}:${i}"
    sudo nvme disconnect -n ${nqn}
done

port=1
for i in $(seq --equal-width ${small_raid1_cnt}); do
    nqn="${base_nqn}:${i}"
    sudo unlink "/sys/kernel/config/nvmet/ports/${port}/subsystems/${nqn}"
    sudo rmdir "/sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/1"
    sudo rmdir "/sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/2"
    sudo rmdir "/sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/3"
    sudo rmdir "/sys/kernel/config/nvmet/subsystems/${nqn}/namespaces/4"

    small_meta_name_0="${small_meta_prefix}_0_${i}"
    sudo dmsetup remove ${small_meta_name_0}
    small_meta_name_1="${small_meta_prefix}_1_${i}"
    sudo dmsetup remove ${small_meta_name_1}
    small_data_name_0="${small_data_prefix}_0_${i}"
    sudo dmsetup remove ${small_data_name_0}
    small_data_name_1="${small_data_prefix}_1_${i}"
    sudo dmsetup remove ${small_data_name_1}

    echo ${i}

    sudo rmdir "/sys/kernel/config/nvmet/ports/${port}"
    sudo rmdir "/sys/kernel/config/nvmet/subsystems/${nqn}"
    port=$((port+1))
done

# small meta

# 8k works
meta_sectors=16
data_sectors=209715200
offset=0
sudo dmsetup create meta0 --table "0 ${meta_sectors} linear /dev/nvme8n1 ${offset}" && offset=$((offset+meta_sectors))
sudo dmsetup create meta1 --table "0 ${meta_sectors} linear /dev/nvme8n1 ${offset}" && offset=$((offset+meta_sectors))
sudo dmsetup create data0 --table "0 ${data_sectors} linear /dev/nvme8n1 ${offset}" && offset=$((offset+data_sectors))
sudo dmsetup create data1 --table "0 ${data_sectors} linear /dev/nvme8n1 ${offset}" && offset=$((offset+data_sectors))
sudo dd if=/dev/zero of=/dev/mapper/meta0 bs=4k count=1
sudo dd if=/dev/zero of=/dev/mapper/meta1 bs=4k count=1

sudo dmsetup create myraid1 --table "0 ${data_sectors} raid raid1 4 0 region_size 8192 nosync 2 /dev/mapper/meta0 /dev/mapper/data0 /dev/mapper/meta1 /dev/mapper/data1"

sudo dmsetup remove myraid1
sudo dmsetup remove meta0
sudo dmsetup remove meta1
sudo dmsetup remove data0
sudo dmsetup remove data1
