#!/bin/bash

dn_server_ip_A=192.168.0.157
dn_server_ip_B=192.168.0.127
cn_server_ip_A=192.168.0.39

dn_host_name_A="dn_host_A"
dn_host_name_B="dn_host_B"
cn_host_name_A="cn_host_A"

leg_cnt=4
cntl_cnt=1

global_id=1

leg00_grp0_ld0_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg00_grp0_ld0_pd_path="/dev/disk/by-path/pci-0000:00:18.0-nvme-1"
leg00_grp0_ld0_dn_port_num=1
leg00_grp0_ld0_dn_tr_addr=${dn_server_ip_A}
leg00_grp0_ld0_dn_tr_svc_id=4410

leg00_grp0_ld1_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg00_grp0_ld1_pd_path="/dev/disk/by-path/pci-0000:00:18.0-nvme-1"
leg00_grp0_ld1_dn_port_num=1
leg00_grp0_ld1_dn_tr_addr=${dn_server_ip_B}
leg00_grp0_ld1_dn_tr_svc_id=4410

leg01_grp0_ld0_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg01_grp0_ld0_pd_path="/dev/disk/by-path/pci-0000:00:19.0-nvme-1"
leg01_grp0_ld0_dn_port_num=2
leg01_grp0_ld0_dn_tr_addr=${dn_server_ip_A}
leg01_grp0_ld0_dn_tr_svc_id=4411

leg01_grp0_ld1_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg01_grp0_ld1_pd_path="/dev/disk/by-path/pci-0000:00:19.0-nvme-1"
leg01_grp0_ld1_dn_port_num=2
leg01_grp0_ld1_dn_tr_addr=${dn_server_ip_B}
leg01_grp0_ld1_dn_tr_svc_id=4411

leg02_grp0_ld0_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg02_grp0_ld0_pd_path="/dev/disk/by-path/pci-0000:00:1a.0-nvme-1"
leg02_grp0_ld0_dn_port_num=3
leg02_grp0_ld0_dn_tr_addr=${dn_server_ip_A}
leg02_grp0_ld0_dn_tr_svc_id=4412

leg02_grp0_ld1_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg02_grp0_ld1_pd_path="/dev/disk/by-path/pci-0000:00:1a.0-nvme-1"
leg02_grp0_ld1_dn_port_num=3
leg02_grp0_ld1_dn_tr_addr=${dn_server_ip_B}
leg02_grp0_ld1_dn_tr_svc_id=4412

leg03_grp0_ld0_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg03_grp0_ld0_pd_path="/dev/disk/by-path/pci-0000:00:1b.0-nvme-1"
leg03_grp0_ld0_dn_port_num=4
leg03_grp0_ld0_dn_tr_addr=${dn_server_ip_A}
leg03_grp0_ld0_dn_tr_svc_id=4413

leg03_grp0_ld1_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg03_grp0_ld1_pd_path="/dev/disk/by-path/pci-0000:00:1b.0-nvme-1"
leg03_grp0_ld1_dn_port_num=4
leg03_grp0_ld1_dn_tr_addr=${dn_server_ip_B}
leg03_grp0_ld1_dn_tr_svc_id=4413

cntl00_cn_mgr_id=${global_id} && global_id=$((global_id+1))
cntl00_cn_host_name=${cn_host_name_A}
cntl00_cn_port_num=1
cntl00_cn_tr_addr=${cn_server_ip_A}
cntl00_cn_tr_svc_id=4420
cntl00_cntlid_min=20000
cntl00_cntlid_max=24999

forward_cn_cnt=0

vd_id=${global_id} && global_id=$((global_id+1))
vd_internal_id=1

ld_start_mb=0
ld_size_mb=20480
thin_dev_size_mb=10240

leg00_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg00_grp0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg00_grp0_ld0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg00_grp0_ld1_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))

leg01_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg01_grp0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg01_grp0_ld0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg01_grp0_ld1_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))

leg02_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg02_grp0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg02_grp0_ld0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg02_grp0_ld1_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))

leg03_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg03_grp0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg03_grp0_ld0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg03_grp0_ld1_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))

external_host_nqn="nqn.2014-08.org.nvmexpress:uuid:16c2fe2c-94fd-4a9b-b0d2-fab74d3fb38b"
UUID_NAMESPACE="37833e01-35d4-4e5a-b0a1-fff158b9d03b"
RANDOM_PREFIX=$(echo "${UUID_NAMESPACE}" | tr -d "-")
vd_nqn="nqn.2024-01.io.dnv.${RANDOM_PREFIX}:00000000:$(printf %08x ${vd_id}):1200:00000000"
dev_uuid=$(uuidgen --md5 --namespace "${UUID_NAMESPACE}" --name "${vd_nqn}")
dev_path="/dev/disk/by-id/nvme-uuid.${dev_uuid}"

echo "sudo modprobe nvme-tcp && sudo modprobe nvmet && sudo modprobe nvmet-tcp" | ssh ${dn_server_ip_A}
echo "sudo modprobe nvme-tcp && sudo modprobe nvmet && sudo modprobe nvmet-tcp" | ssh ${dn_server_ip_B}
echo "sudo modprobe nvme-tcp && sudo modprobe nvmet && sudo modprobe nvmet-tcp" | ssh ${cn_server_ip_A}
scp *.sh ${dn_server_ip_A}:/tmp/ && scp *.sh ${dn_server_ip_B}:/tmp/ && scp *.sh ${cn_server_ip_A}:/tmp/

# prepare leg00 grp0 ld0
echo "sudo /tmp/dn_prepare.sh ${leg00_grp0_ld0_dn_port_num} ${leg00_grp0_ld0_dn_tr_addr} ${leg00_grp0_ld0_dn_tr_svc_id}" | ssh ${dn_server_ip_A}
# prepare leg00 grp0 ld1
echo "sudo /tmp/dn_prepare.sh ${leg00_grp0_ld1_dn_port_num} ${leg00_grp0_ld1_dn_tr_addr} ${leg00_grp0_ld1_dn_tr_svc_id}" | ssh ${dn_server_ip_B}
# prepare leg01 grp0 ld0
echo "sudo /tmp/dn_prepare.sh ${leg01_grp0_ld0_dn_port_num} ${leg01_grp0_ld0_dn_tr_addr} ${leg01_grp0_ld0_dn_tr_svc_id}" | ssh ${dn_server_ip_A}
# prepare leg01 grp0 ld1
echo "sudo /tmp/dn_prepare.sh ${leg01_grp0_ld1_dn_port_num} ${leg01_grp0_ld1_dn_tr_addr} ${leg01_grp0_ld1_dn_tr_svc_id}" | ssh ${dn_server_ip_B}
# prepare leg02 grp0 ld0
echo "sudo /tmp/dn_prepare.sh ${leg02_grp0_ld0_dn_port_num} ${leg02_grp0_ld0_dn_tr_addr} ${leg02_grp0_ld0_dn_tr_svc_id}" | ssh ${dn_server_ip_A}
# prepare leg02 grp0 ld1
echo "sudo /tmp/dn_prepare.sh ${leg02_grp0_ld1_dn_port_num} ${leg02_grp0_ld1_dn_tr_addr} ${leg02_grp0_ld1_dn_tr_svc_id}" | ssh ${dn_server_ip_B}
# prepare leg03 grp0 ld0
echo "sudo /tmp/dn_prepare.sh ${leg03_grp0_ld0_dn_port_num} ${leg03_grp0_ld0_dn_tr_addr} ${leg03_grp0_ld0_dn_tr_svc_id}" | ssh ${dn_server_ip_A}
# prepare leg03 grp0 ld1
echo "sudo /tmp/dn_prepare.sh ${leg03_grp0_ld1_dn_port_num} ${leg03_grp0_ld1_dn_tr_addr} ${leg03_grp0_ld1_dn_tr_svc_id}" | ssh ${dn_server_ip_B}

# cleanup leg00 grp0 ld0
echo "sudo /tmp/dn_cleanup.sh ${leg00_grp0_ld0_dn_port_num}" | ssh ${dn_server_ip_A}
# cleanup leg00 grp0 ld1
echo "sudo /tmp/dn_cleanup.sh ${leg00_grp0_ld1_dn_port_num}" | ssh ${dn_server_ip_B}
# cleanup leg01 grp0 ld0
echo "sudo /tmp/dn_cleanup.sh ${leg01_grp0_ld0_dn_port_num}" | ssh ${dn_server_ip_A}
# cleanup leg01 grp0 ld1
echo "sudo /tmp/dn_cleanup.sh ${leg01_grp0_ld1_dn_port_num}" | ssh ${dn_server_ip_B}
# cleanup leg02 grp0 ld0
echo "sudo /tmp/dn_cleanup.sh ${leg02_grp0_ld0_dn_port_num}" | ssh ${dn_server_ip_A}
# cleanup leg02 grp0 ld1
echo "sudo /tmp/dn_cleanup.sh ${leg02_grp0_ld1_dn_port_num}" | ssh ${dn_server_ip_B}
# cleanup leg03 grp0 ld0
echo "sudo /tmp/dn_cleanup.sh ${leg03_grp0_ld0_dn_port_num}" | ssh ${dn_server_ip_A}
# cleanup leg03 grp0 ld1
echo "sudo /tmp/dn_cleanup.sh ${leg03_grp0_ld1_dn_port_num}" | ssh ${dn_server_ip_B}

# prepare cntl00
echo "sudo /tmp/cn_prepare.sh ${cntl00_cn_port_num} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id}" | ssh ${cn_server_ip_A}

# cleanup cntl00
echo "sudo /tmp/cn_cleanup.sh ${cntl00_cn_port_num}" | ssh ${cn_server_ip_A}

# create leg00 grp0 ld0
echo "sudo /tmp/dn_ld_create.sh ${leg00_grp0_ld0_dn_mgr_id} ${leg00_grp0_ld0_dn_port_num} ${leg00_grp0_ld0_pd_path} ${vd_id} ${leg00_grp0_ld0_id} ${ld_start_mb} ${ld_size_mb} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name}" | ssh ${dn_server_ip_A}
# create leg00 grp0 ld1
echo "sudo /tmp/dn_ld_create.sh ${leg00_grp0_ld1_dn_mgr_id} ${leg00_grp0_ld1_dn_port_num} ${leg00_grp0_ld1_pd_path} ${vd_id} ${leg00_grp0_ld1_id} ${ld_start_mb} ${ld_size_mb} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name}" | ssh ${dn_server_ip_B}
# create leg01 grp0 ld0
echo "sudo /tmp/dn_ld_create.sh ${leg01_grp0_ld0_dn_mgr_id} ${leg01_grp0_ld0_dn_port_num} ${leg01_grp0_ld0_pd_path} ${vd_id} ${leg01_grp0_ld0_id} ${ld_start_mb} ${ld_size_mb} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name}" | ssh ${dn_server_ip_A}
# create leg01 grp0 ld1
echo "sudo /tmp/dn_ld_create.sh ${leg01_grp0_ld1_dn_mgr_id} ${leg01_grp0_ld1_dn_port_num} ${leg01_grp0_ld1_pd_path} ${vd_id} ${leg01_grp0_ld1_id} ${ld_start_mb} ${ld_size_mb} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name}" | ssh ${dn_server_ip_B}
# create leg02 grp0 ld0
echo "sudo /tmp/dn_ld_create.sh ${leg02_grp0_ld0_dn_mgr_id} ${leg02_grp0_ld0_dn_port_num} ${leg02_grp0_ld0_pd_path} ${vd_id} ${leg02_grp0_ld0_id} ${ld_start_mb} ${ld_size_mb} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name}" | ssh ${dn_server_ip_A}
# create leg02 grp0 ld1
echo "sudo /tmp/dn_ld_create.sh ${leg02_grp0_ld1_dn_mgr_id} ${leg02_grp0_ld1_dn_port_num} ${leg02_grp0_ld1_pd_path} ${vd_id} ${leg02_grp0_ld1_id} ${ld_start_mb} ${ld_size_mb} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name}" | ssh ${dn_server_ip_B}
# create leg03 grp0 ld0
echo "sudo /tmp/dn_ld_create.sh ${leg03_grp0_ld0_dn_mgr_id} ${leg03_grp0_ld0_dn_port_num} ${leg03_grp0_ld0_pd_path} ${vd_id} ${leg03_grp0_ld0_id} ${ld_start_mb} ${ld_size_mb} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name}" | ssh ${dn_server_ip_A}
# create leg03 grp0 ld1
echo "sudo /tmp/dn_ld_create.sh ${leg03_grp0_ld1_dn_mgr_id} ${leg03_grp0_ld1_dn_port_num} ${leg03_grp0_ld1_pd_path} ${vd_id} ${leg03_grp0_ld1_id} ${ld_start_mb} ${ld_size_mb} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name}" | ssh ${dn_server_ip_B}

# delete leg00 grp0 ld0
echo "sudo /tmp/dn_ld_delete.sh ${leg00_grp0_ld0_dn_mgr_id} ${leg00_grp0_ld0_dn_port_num} ${vd_id} ${leg00_grp0_ld0_id} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name}" | ssh ${dn_server_ip_A}
# delete leg00 grp0 ld1
echo "sudo /tmp/dn_ld_delete.sh ${leg00_grp0_ld1_dn_mgr_id} ${leg00_grp0_ld1_dn_port_num} ${vd_id} ${leg00_grp0_ld1_id} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name}" | ssh ${dn_server_ip_B}
# delete leg01 grp0 ld0
echo "sudo /tmp/dn_ld_delete.sh ${leg01_grp0_ld0_dn_mgr_id} ${leg01_grp0_ld0_dn_port_num} ${vd_id} ${leg01_grp0_ld0_id} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name}" | ssh ${dn_server_ip_A}
# delete leg01 grp0 ld1
echo "sudo /tmp/dn_ld_delete.sh ${leg01_grp0_ld1_dn_mgr_id} ${leg01_grp0_ld1_dn_port_num} ${vd_id} ${leg01_grp0_ld1_id} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name}" | ssh ${dn_server_ip_B}
# delete leg02 grp0 ld0
echo "sudo /tmp/dn_ld_delete.sh ${leg02_grp0_ld0_dn_mgr_id} ${leg02_grp0_ld0_dn_port_num} ${vd_id} ${leg02_grp0_ld0_id} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name}" | ssh ${dn_server_ip_A}
# delete leg02 grp0 ld1
echo "sudo /tmp/dn_ld_delete.sh ${leg02_grp0_ld1_dn_mgr_id} ${leg02_grp0_ld1_dn_port_num} ${vd_id} ${leg02_grp0_ld1_id} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name}" | ssh ${dn_server_ip_B}
# delete leg03 grp0 ld0
echo "sudo /tmp/dn_ld_delete.sh ${leg03_grp0_ld0_dn_mgr_id} ${leg03_grp0_ld0_dn_port_num} ${vd_id} ${leg03_grp0_ld0_id} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name}" | ssh ${dn_server_ip_A}
# delete leg03 grp0 ld1
echo "sudo /tmp/dn_ld_delete.sh ${leg03_grp0_ld1_dn_mgr_id} ${leg03_grp0_ld1_dn_port_num} ${vd_id} ${leg03_grp0_ld1_id} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name}" | ssh ${dn_server_ip_B}

# create leg00
echo "sudo /tmp/cn_leg_create.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg00_id} ${leg00_grp0_id} ${leg00_grp0_ld0_id} ${leg00_grp0_ld0_dn_mgr_id} ${leg00_grp0_ld0_dn_tr_addr} ${leg00_grp0_ld0_dn_tr_svc_id} ${leg00_grp0_ld1_id} ${leg00_grp0_ld1_dn_mgr_id} ${leg00_grp0_ld1_dn_tr_addr} ${leg00_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt}" | ssh ${cn_server_ip_A}
# create leg01
echo "sudo /tmp/cn_leg_create.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg01_id} ${leg01_grp0_id} ${leg01_grp0_ld0_id} ${leg01_grp0_ld0_dn_mgr_id} ${leg01_grp0_ld0_dn_tr_addr} ${leg01_grp0_ld0_dn_tr_svc_id} ${leg01_grp0_ld1_id} ${leg01_grp0_ld1_dn_mgr_id} ${leg01_grp0_ld1_dn_tr_addr} ${leg01_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt}" | ssh ${cn_server_ip_A}
# create leg02
echo "sudo /tmp/cn_leg_create.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg02_id} ${leg02_grp0_id} ${leg02_grp0_ld0_id} ${leg02_grp0_ld0_dn_mgr_id} ${leg02_grp0_ld0_dn_tr_addr} ${leg02_grp0_ld0_dn_tr_svc_id} ${leg02_grp0_ld1_id} ${leg02_grp0_ld1_dn_mgr_id} ${leg02_grp0_ld1_dn_tr_addr} ${leg02_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt}" | ssh ${cn_server_ip_A}
# create leg03
echo "sudo /tmp/cn_leg_create.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg03_id} ${leg03_grp0_id} ${leg03_grp0_ld0_id} ${leg03_grp0_ld0_dn_mgr_id} ${leg03_grp0_ld0_dn_tr_addr} ${leg03_grp0_ld0_dn_tr_svc_id} ${leg03_grp0_ld1_id} ${leg03_grp0_ld1_dn_mgr_id} ${leg03_grp0_ld1_dn_tr_addr} ${leg03_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt}" | ssh ${cn_server_ip_A}

# delete leg00
echo "sudo /tmp/cn_leg_delete.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg00_id} ${leg00_grp0_id} ${leg00_grp0_ld0_id} ${leg00_grp0_ld0_dn_mgr_id} ${leg00_grp0_ld0_dn_tr_addr} ${leg00_grp0_ld0_dn_tr_svc_id} ${leg00_grp0_ld1_id} ${leg00_grp0_ld1_dn_mgr_id} ${leg00_grp0_ld1_dn_tr_addr} ${leg00_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt}" | ssh ${cn_server_ip_A}
# delete leg01
echo "sudo /tmp/cn_leg_delete.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg01_id} ${leg01_grp0_id} ${leg01_grp0_ld0_id} ${leg01_grp0_ld0_dn_mgr_id} ${leg01_grp0_ld0_dn_tr_addr} ${leg01_grp0_ld0_dn_tr_svc_id} ${leg01_grp0_ld1_id} ${leg01_grp0_ld1_dn_mgr_id} ${leg01_grp0_ld1_dn_tr_addr} ${leg01_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt}" | ssh ${cn_server_ip_A}
# delete leg02
echo "sudo /tmp/cn_leg_delete.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg02_id} ${leg02_grp0_id} ${leg02_grp0_ld0_id} ${leg02_grp0_ld0_dn_mgr_id} ${leg02_grp0_ld0_dn_tr_addr} ${leg02_grp0_ld0_dn_tr_svc_id} ${leg02_grp0_ld1_id} ${leg02_grp0_ld1_dn_mgr_id} ${leg02_grp0_ld1_dn_tr_addr} ${leg02_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt}" | ssh ${cn_server_ip_A}
# delete leg03
echo "sudo /tmp/cn_leg_delete.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg03_id} ${leg03_grp0_id} ${leg03_grp0_ld0_id} ${leg03_grp0_ld0_dn_mgr_id} ${leg03_grp0_ld0_dn_tr_addr} ${leg03_grp0_ld0_dn_tr_svc_id} ${leg03_grp0_ld1_id} ${leg03_grp0_ld1_dn_mgr_id} ${leg03_grp0_ld1_dn_tr_addr} ${leg03_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt}" | ssh ${cn_server_ip_A}

# delete leg00
echo "sudo /tmp/cn_leg_delete.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg00_id} ${leg00_grp0_id} ${leg00_grp0_ld0_id} ${leg00_grp0_ld0_dn_mgr_id} ${leg00_grp0_ld0_dn_tr_addr} ${leg00_grp0_ld0_dn_tr_svc_id} ${leg00_grp0_ld1_id} ${leg00_grp0_ld1_dn_mgr_id} ${leg00_grp0_ld1_dn_tr_addr} ${leg00_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt}" | ssh ${cn_server_ip_A}
# delete leg01
echo "sudo /tmp/cn_leg_delete.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg01_id} ${leg01_grp0_id} ${leg01_grp0_ld0_id} ${leg01_grp0_ld0_dn_mgr_id} ${leg01_grp0_ld0_dn_tr_addr} ${leg01_grp0_ld0_dn_tr_svc_id} ${leg01_grp0_ld1_id} ${leg01_grp0_ld1_dn_mgr_id} ${leg01_grp0_ld1_dn_tr_addr} ${leg01_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt}" | ssh ${cn_server_ip_A}
# delete leg02
echo "sudo /tmp/cn_leg_delete.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg02_id} ${leg02_grp0_id} ${leg02_grp0_ld0_id} ${leg02_grp0_ld0_dn_mgr_id} ${leg02_grp0_ld0_dn_tr_addr} ${leg02_grp0_ld0_dn_tr_svc_id} ${leg02_grp0_ld1_id} ${leg02_grp0_ld1_dn_mgr_id} ${leg02_grp0_ld1_dn_tr_addr} ${leg02_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt}" | ssh ${cn_server_ip_A}
# delete leg03
echo "sudo /tmp/cn_leg_delete.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg03_id} ${leg03_grp0_id} ${leg03_grp0_ld0_id} ${leg03_grp0_ld0_dn_mgr_id} ${leg03_grp0_ld0_dn_tr_addr} ${leg03_grp0_ld0_dn_tr_svc_id} ${leg03_grp0_ld1_id} ${leg03_grp0_ld1_dn_mgr_id} ${leg03_grp0_ld1_dn_tr_addr} ${leg03_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt}" | ssh ${cn_server_ip_A}

# create cntl00
echo "sudo /tmp/cn_cntl_create.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${cntl00_cntlid_min} ${cntl00_cntlid_max} ${vd_id} ${external_host_nqn} ${thin_dev_size_mb} ${leg_cnt} ${leg00_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg01_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg02_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg03_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id}" | ssh ${cn_server_ip_A}

# delete cntl00
echo "sudo /tmp/cn_cntl_delete.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${external_host_nqn} ${leg_cnt} ${leg00_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg01_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg02_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg03_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id}" | ssh ${cn_server_ip_A}

sudo nvme connect --nqn ${vd_nqn} --transport tcp --traddr ${cntl00_cn_tr_addr} --trsvcid ${cntl00_cn_tr_svc_id} --hostnqn ${external_host_nqn}
subsys_name=$(sudo nvme list-subsys --output-format json | jq -rM ".Subsystems[] | select(.NQN==\"${vd_nqn}\") | .Name")
sudo bash -c "echo round-robin > /sys/class/nvme-subsystem/${subsys_name}/iopolicy"

sudo parted -s "${dev_path}" unit s print

sudo fio --filename=${dev_path} --name mytest --rw=randwrite --ioengine=libaio --iodepth=4 --bs=4k --numjobs=72 --direct=1 --time_based --runtime=60 --group_reporting --norandommap
# mytest: (g=0): rw=randwrite, bs=(R) 4096B-4096B, (W) 4096B-4096B, (T) 4096B-4096B, ioengine=libaio, iodepth=4
# ...
# fio-3.32
# Starting 72 processes
# Jobs: 72 (f=72): [w(72)][100.0%][w=1639MiB/s][w=420k IOPS][eta 00m:00s]
# mytest: (groupid=0, jobs=72): err= 0: pid=8287: Fri Feb 23 23:56:04 2024
#   write: IOPS=418k, BW=1631MiB/s (1710MB/s)(95.6GiB/60003msec); 0 zone resets
#     slat (nsec): min=1603, max=528640, avg=6213.62, stdev=3460.95
#     clat (usec): min=42, max=11135, avg=682.62, stdev=207.70
#      lat (usec): min=202, max=11137, avg=688.84, stdev=207.47
#     clat percentiles (usec):
#      |  1.00th=[  392],  5.00th=[  449], 10.00th=[  482], 20.00th=[  529],
#      | 30.00th=[  562], 40.00th=[  603], 50.00th=[  644], 60.00th=[  685],
#      | 70.00th=[  742], 80.00th=[  799], 90.00th=[  930], 95.00th=[ 1057],
#      | 99.00th=[ 1401], 99.50th=[ 1549], 99.90th=[ 1958], 99.95th=[ 2212],
#      | 99.99th=[ 3556]
#    bw (  MiB/s): min= 1196, max= 1733, per=100.00%, avg=1631.78, stdev= 0.77, samples=8568
#    iops        : min=306228, max=443814, avg=417734.39, stdev=197.48, samples=8568
#   lat (usec)   : 50=0.01%, 250=0.01%, 500=13.72%, 750=58.01%, 1000=21.35%
#   lat (msec)   : 2=6.83%, 4=0.08%, 10=0.01%, 20=0.01%
#   cpu          : usr=1.34%, sys=4.32%, ctx=18926543, majf=1, minf=669
#   IO depths    : 1=0.1%, 2=0.1%, 4=100.0%, 8=0.0%, 16=0.0%, 32=0.0%, >=64=0.0%
#      submit    : 0=0.0%, 4=100.0%, 8=0.0%, 16=0.0%, 32=0.0%, 64=0.0%, >=64=0.0%
#      complete  : 0=0.0%, 4=100.0%, 8=0.0%, 16=0.0%, 32=0.0%, 64=0.0%, >=64=0.0%
#      issued rwts: total=0,25053746,0,0 short=0,0,0,0 dropped=0,0,0,0
#      latency   : target=0, window=0, percentile=100.00%, depth=4

# Run status group 0 (all jobs):
#   WRITE: bw=1631MiB/s (1710MB/s), 1631MiB/s-1631MiB/s (1710MB/s-1710MB/s), io=95.6GiB (103GB), run=60003-60003msec

sudo fio --filename=${dev_path} --name mytest --rw=randread --ioengine=libaio --iodepth=4 --bs=4k --numjobs=72 --direct=1 --time_based --runtime=60 --group_reporting --norandommap
# mytest: (g=0): rw=randread, bs=(R) 4096B-4096B, (W) 4096B-4096B, (T) 4096B-4096B, ioengine=libaio, iodepth=4
# ...
# fio-3.32
# Starting 72 processes
# Jobs: 72 (f=72): [r(72)][100.0%][r=1849MiB/s][r=473k IOPS][eta 00m:00s]
# mytest: (groupid=0, jobs=72): err= 0: pid=8498: Fri Feb 23 23:57:43 2024
#   read: IOPS=471k, BW=1839MiB/s (1928MB/s)(108GiB/60002msec)
#     slat (nsec): min=1491, max=613592, avg=6166.95, stdev=3892.33
#     clat (usec): min=18, max=9261, avg=604.69, stdev=144.98
#      lat (usec): min=202, max=9266, avg=610.86, stdev=144.72
#     clat percentiles (usec):
#      |  1.00th=[  379],  5.00th=[  424], 10.00th=[  453], 20.00th=[  490],
#      | 30.00th=[  519], 40.00th=[  545], 50.00th=[  578], 60.00th=[  611],
#      | 70.00th=[  652], 80.00th=[  709], 90.00th=[  783], 95.00th=[  873],
#      | 99.00th=[ 1057], 99.50th=[ 1139], 99.90th=[ 1336], 99.95th=[ 1434],
#      | 99.99th=[ 1663]
#    bw (  MiB/s): min= 1767, max= 1915, per=100.00%, avg=1840.05, stdev= 0.39, samples=8568
#    iops        : min=452446, max=490268, avg=471051.71, stdev=100.47, samples=8568
#   lat (usec)   : 20=0.01%, 250=0.01%, 500=23.63%, 750=61.83%, 1000=12.75%
#   lat (msec)   : 2=1.79%, 4=0.01%, 10=0.01%
#   cpu          : usr=1.54%, sys=4.97%, ctx=22896617, majf=1, minf=950
#   IO depths    : 1=0.1%, 2=0.1%, 4=100.0%, 8=0.0%, 16=0.0%, 32=0.0%, >=64=0.0%
#      submit    : 0=0.0%, 4=100.0%, 8=0.0%, 16=0.0%, 32=0.0%, 64=0.0%, >=64=0.0%
#      complete  : 0=0.0%, 4=100.0%, 8=0.0%, 16=0.0%, 32=0.0%, 64=0.0%, >=64=0.0%
#      issued rwts: total=28245083,0,0,0 short=0,0,0,0 dropped=0,0,0,0
#      latency   : target=0, window=0, percentile=100.00%, depth=4

# Run status group 0 (all jobs):
#    READ: bw=1839MiB/s (1928MB/s), 1839MiB/s-1839MiB/s (1928MB/s-1928MB/s), io=108GiB (116GB), run=60002-60002msec

sudo nvme disconnect --nqn ${vd_nqn}
