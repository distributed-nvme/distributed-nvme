#!/bin/bash

dn_server_ip_A=192.168.0.157
dn_server_ip_B=192.168.0.127
cn_server_ip_A=192.168.0.39
cn_server_ip_B=192.168.0.212

dn_host_name_A="dn_host_A"
dn_host_name_B="dn_host_B"
cn_host_name_A="cn_host_A"
cn_host_name_B="cn_host_B"

leg_cnt=8
cntl_cnt=2

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

leg04_grp0_ld0_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg04_grp0_ld0_pd_path="/dev/disk/by-path/pci-0000:00:1c.0-nvme-1"
leg04_grp0_ld0_dn_port_num=5
leg04_grp0_ld0_dn_tr_addr=${dn_server_ip_A}
leg04_grp0_ld0_dn_tr_svc_id=4414

leg04_grp0_ld1_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg04_grp0_ld1_pd_path="/dev/disk/by-path/pci-0000:00:1c.0-nvme-1"
leg04_grp0_ld1_dn_port_num=5
leg04_grp0_ld1_dn_tr_addr=${dn_server_ip_B}
leg04_grp0_ld1_dn_tr_svc_id=4414

leg05_grp0_ld0_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg05_grp0_ld0_pd_path="/dev/disk/by-path/pci-0000:00:1d.0-nvme-1"
leg05_grp0_ld0_dn_port_num=6
leg05_grp0_ld0_dn_tr_addr=${dn_server_ip_A}
leg05_grp0_ld0_dn_tr_svc_id=4415

leg05_grp0_ld1_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg05_grp0_ld1_pd_path="/dev/disk/by-path/pci-0000:00:1d.0-nvme-1"
leg05_grp0_ld1_dn_port_num=6
leg05_grp0_ld1_dn_tr_addr=${dn_server_ip_B}
leg05_grp0_ld1_dn_tr_svc_id=4415

leg06_grp0_ld0_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg06_grp0_ld0_pd_path="/dev/disk/by-path/pci-0000:00:1e.0-nvme-1"
leg06_grp0_ld0_dn_port_num=7
leg06_grp0_ld0_dn_tr_addr=${dn_server_ip_A}
leg06_grp0_ld0_dn_tr_svc_id=4416

leg06_grp0_ld1_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg06_grp0_ld1_pd_path="/dev/disk/by-path/pci-0000:00:1e.0-nvme-1"
leg06_grp0_ld1_dn_port_num=7
leg06_grp0_ld1_dn_tr_addr=${dn_server_ip_B}
leg06_grp0_ld1_dn_tr_svc_id=4416

leg07_grp0_ld0_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg07_grp0_ld0_pd_path="/dev/disk/by-path/pci-0000:00:1f.0-nvme-1"
leg07_grp0_ld0_dn_port_num=8
leg07_grp0_ld0_dn_tr_addr=${dn_server_ip_A}
leg07_grp0_ld0_dn_tr_svc_id=4417

leg07_grp0_ld1_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg07_grp0_ld1_pd_path="/dev/disk/by-path/pci-0000:00:1f.0-nvme-1"
leg07_grp0_ld1_dn_port_num=8
leg07_grp0_ld1_dn_tr_addr=${dn_server_ip_B}
leg07_grp0_ld1_dn_tr_svc_id=4417

cntl00_cn_mgr_id=${global_id} && global_id=$((global_id+1))
cntl00_cn_host_name=${cn_host_name_A}
cntl00_cn_port_num=1
cntl00_cn_tr_addr=${cn_server_ip_A}
cntl00_cn_tr_svc_id=4420
cntl00_cntlid_min=20000
cntl00_cntlid_max=24999

cntl01_cn_mgr_id=${global_id} && global_id=$((global_id+1))
cntl01_cn_host_name=${cn_host_name_B}
cntl01_cn_port_num=1
cntl01_cn_tr_addr=${cn_server_ip_B}
cntl01_cn_tr_svc_id=4420
cntl01_cntlid_min=25000
cntl01_cntlid_max=29999

forward_cn_cnt=1

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

leg04_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg04_grp0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg04_grp0_ld0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg04_grp0_ld1_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))

leg05_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg05_grp0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg05_grp0_ld0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg05_grp0_ld1_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))

leg06_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg06_grp0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg06_grp0_ld0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg06_grp0_ld1_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))

leg07_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg07_grp0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg07_grp0_ld0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg07_grp0_ld1_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))

external_host_nqn="nqn.2014-08.org.nvmexpress:uuid:16c2fe2c-94fd-4a9b-b0d2-fab74d3fb38b"
UUID_NAMESPACE="37833e01-35d4-4e5a-b0a1-fff158b9d03b"
RANDOM_PREFIX=$(echo "${UUID_NAMESPACE}" | tr -d "-")
vd_nqn="nqn.2024-01.io.dnv.${RANDOM_PREFIX}:00000000:$(printf %08x ${vd_id}):1200:00000000"
dev_uuid=$(uuidgen --md5 --namespace "${UUID_NAMESPACE}" --name "${vd_nqn}")
dev_path="/dev/disk/by-id/nvme-uuid.${dev_uuid}"

echo "sudo modprobe nvme-tcp && sudo modprobe nvmet && sudo modprobe nvmet-tcp" | ssh ${dn_server_ip_A}
echo "sudo modprobe nvme-tcp && sudo modprobe nvmet && sudo modprobe nvmet-tcp" | ssh ${dn_server_ip_B}
echo "sudo modprobe nvme-tcp && sudo modprobe nvmet && sudo modprobe nvmet-tcp" | ssh ${cn_server_ip_A}
echo "sudo modprobe nvme-tcp && sudo modprobe nvmet && sudo modprobe nvmet-tcp" | ssh ${cn_server_ip_B}
scp *.sh ${dn_server_ip_A}:/tmp/ && scp *.sh ${dn_server_ip_B}:/tmp/ && scp *.sh ${cn_server_ip_A}:/tmp/ && scp *.sh ${cn_server_ip_B}:/tmp/

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
# prepare leg04 grp0 ld0
echo "sudo /tmp/dn_prepare.sh ${leg04_grp0_ld0_dn_port_num} ${leg04_grp0_ld0_dn_tr_addr} ${leg04_grp0_ld0_dn_tr_svc_id}" | ssh ${dn_server_ip_A}
# prepare leg04 grp0 ld1
echo "sudo /tmp/dn_prepare.sh ${leg04_grp0_ld1_dn_port_num} ${leg04_grp0_ld1_dn_tr_addr} ${leg04_grp0_ld1_dn_tr_svc_id}" | ssh ${dn_server_ip_B}
# prepare leg05 grp0 ld0
echo "sudo /tmp/dn_prepare.sh ${leg05_grp0_ld0_dn_port_num} ${leg05_grp0_ld0_dn_tr_addr} ${leg05_grp0_ld0_dn_tr_svc_id}" | ssh ${dn_server_ip_A}
# prepare leg05 grp0 ld1
echo "sudo /tmp/dn_prepare.sh ${leg05_grp0_ld1_dn_port_num} ${leg05_grp0_ld1_dn_tr_addr} ${leg05_grp0_ld1_dn_tr_svc_id}" | ssh ${dn_server_ip_B}
# prepare leg06 grp0 ld0
echo "sudo /tmp/dn_prepare.sh ${leg06_grp0_ld0_dn_port_num} ${leg06_grp0_ld0_dn_tr_addr} ${leg06_grp0_ld0_dn_tr_svc_id}" | ssh ${dn_server_ip_A}
# prepare leg06 grp0 ld1
echo "sudo /tmp/dn_prepare.sh ${leg06_grp0_ld1_dn_port_num} ${leg06_grp0_ld1_dn_tr_addr} ${leg06_grp0_ld1_dn_tr_svc_id}" | ssh ${dn_server_ip_B}
# prepare leg07 grp0 ld0
echo "sudo /tmp/dn_prepare.sh ${leg07_grp0_ld0_dn_port_num} ${leg07_grp0_ld0_dn_tr_addr} ${leg07_grp0_ld0_dn_tr_svc_id}" | ssh ${dn_server_ip_A}
# prepare leg07 grp0 ld1
echo "sudo /tmp/dn_prepare.sh ${leg07_grp0_ld1_dn_port_num} ${leg07_grp0_ld1_dn_tr_addr} ${leg07_grp0_ld1_dn_tr_svc_id}" | ssh ${dn_server_ip_B}

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
# cleanup leg04 grp0 ld0
echo "sudo /tmp/dn_cleanup.sh ${leg04_grp0_ld0_dn_port_num}" | ssh ${dn_server_ip_A}
# cleanup leg04 grp0 ld1
echo "sudo /tmp/dn_cleanup.sh ${leg04_grp0_ld1_dn_port_num}" | ssh ${dn_server_ip_B}
# cleanup leg05 grp0 ld0
echo "sudo /tmp/dn_cleanup.sh ${leg05_grp0_ld0_dn_port_num}" | ssh ${dn_server_ip_A}
# cleanup leg05 grp0 ld1
echo "sudo /tmp/dn_cleanup.sh ${leg05_grp0_ld1_dn_port_num}" | ssh ${dn_server_ip_B}
# cleanup leg06 grp0 ld0
echo "sudo /tmp/dn_cleanup.sh ${leg06_grp0_ld0_dn_port_num}" | ssh ${dn_server_ip_A}
# cleanup leg06 grp0 ld1
echo "sudo /tmp/dn_cleanup.sh ${leg06_grp0_ld1_dn_port_num}" | ssh ${dn_server_ip_B}
# cleanup leg07 grp0 ld0
echo "sudo /tmp/dn_cleanup.sh ${leg07_grp0_ld0_dn_port_num}" | ssh ${dn_server_ip_A}
# cleanup leg07 grp0 ld1
echo "sudo /tmp/dn_cleanup.sh ${leg07_grp0_ld1_dn_port_num}" | ssh ${dn_server_ip_B}

# prepare cntl00
echo "sudo /tmp/cn_prepare.sh ${cntl00_cn_port_num} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id}" | ssh ${cn_server_ip_A}
# prepare cntl01
echo "sudo /tmp/cn_prepare.sh ${cntl01_cn_port_num} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id}" | ssh ${cn_server_ip_B}

# cleanup cntl00
echo "sudo /tmp/cn_cleanup.sh ${cntl00_cn_port_num}" | ssh ${cn_server_ip_A}
# cleanup cntl01
echo "sudo /tmp/cn_cleanup.sh ${cntl01_cn_port_num}" | ssh ${cn_server_ip_B}

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
# create leg04 grp0 ld0
echo "sudo /tmp/dn_ld_create.sh ${leg04_grp0_ld0_dn_mgr_id} ${leg04_grp0_ld0_dn_port_num} ${leg04_grp0_ld0_pd_path} ${vd_id} ${leg04_grp0_ld0_id} ${ld_start_mb} ${ld_size_mb} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name}" | ssh ${dn_server_ip_A}
# create leg04 grp0 ld1
echo "sudo /tmp/dn_ld_create.sh ${leg04_grp0_ld1_dn_mgr_id} ${leg04_grp0_ld1_dn_port_num} ${leg04_grp0_ld1_pd_path} ${vd_id} ${leg04_grp0_ld1_id} ${ld_start_mb} ${ld_size_mb} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name}" | ssh ${dn_server_ip_B}
# create leg05 grp0 ld0
echo "sudo /tmp/dn_ld_create.sh ${leg05_grp0_ld0_dn_mgr_id} ${leg05_grp0_ld0_dn_port_num} ${leg05_grp0_ld0_pd_path} ${vd_id} ${leg05_grp0_ld0_id} ${ld_start_mb} ${ld_size_mb} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name}" | ssh ${dn_server_ip_A}
# create leg05 grp0 ld1
echo "sudo /tmp/dn_ld_create.sh ${leg05_grp0_ld1_dn_mgr_id} ${leg05_grp0_ld1_dn_port_num} ${leg05_grp0_ld1_pd_path} ${vd_id} ${leg05_grp0_ld1_id} ${ld_start_mb} ${ld_size_mb} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name}" | ssh ${dn_server_ip_B}
# create leg06 grp0 ld0
echo "sudo /tmp/dn_ld_create.sh ${leg06_grp0_ld0_dn_mgr_id} ${leg06_grp0_ld0_dn_port_num} ${leg06_grp0_ld0_pd_path} ${vd_id} ${leg06_grp0_ld0_id} ${ld_start_mb} ${ld_size_mb} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name}" | ssh ${dn_server_ip_A}
# create leg06 grp0 ld1
echo "sudo /tmp/dn_ld_create.sh ${leg06_grp0_ld1_dn_mgr_id} ${leg06_grp0_ld1_dn_port_num} ${leg06_grp0_ld1_pd_path} ${vd_id} ${leg06_grp0_ld1_id} ${ld_start_mb} ${ld_size_mb} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name}" | ssh ${dn_server_ip_B}
# create leg07 grp0 ld0
echo "sudo /tmp/dn_ld_create.sh ${leg07_grp0_ld0_dn_mgr_id} ${leg07_grp0_ld0_dn_port_num} ${leg07_grp0_ld0_pd_path} ${vd_id} ${leg07_grp0_ld0_id} ${ld_start_mb} ${ld_size_mb} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name}" | ssh ${dn_server_ip_A}
# create leg07 grp0 ld1
echo "sudo /tmp/dn_ld_create.sh ${leg07_grp0_ld1_dn_mgr_id} ${leg07_grp0_ld1_dn_port_num} ${leg07_grp0_ld1_pd_path} ${vd_id} ${leg07_grp0_ld1_id} ${ld_start_mb} ${ld_size_mb} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name}" | ssh ${dn_server_ip_B}

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
# delete leg04 grp0 ld0
echo "sudo /tmp/dn_ld_delete.sh ${leg04_grp0_ld0_dn_mgr_id} ${leg04_grp0_ld0_dn_port_num} ${vd_id} ${leg04_grp0_ld0_id} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name}" | ssh ${dn_server_ip_A}
# delete leg04 grp0 ld1
echo "sudo /tmp/dn_ld_delete.sh ${leg04_grp0_ld1_dn_mgr_id} ${leg04_grp0_ld1_dn_port_num} ${vd_id} ${leg04_grp0_ld1_id} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name}" | ssh ${dn_server_ip_B}
# delete leg05 grp0 ld0
echo "sudo /tmp/dn_ld_delete.sh ${leg05_grp0_ld0_dn_mgr_id} ${leg05_grp0_ld0_dn_port_num} ${vd_id} ${leg05_grp0_ld0_id} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name}" | ssh ${dn_server_ip_A}
# delete leg05 grp0 ld1
echo "sudo /tmp/dn_ld_delete.sh ${leg05_grp0_ld1_dn_mgr_id} ${leg05_grp0_ld1_dn_port_num} ${vd_id} ${leg05_grp0_ld1_id} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name}" | ssh ${dn_server_ip_B}
# delete leg06 grp0 ld0
echo "sudo /tmp/dn_ld_delete.sh ${leg06_grp0_ld0_dn_mgr_id} ${leg06_grp0_ld0_dn_port_num} ${vd_id} ${leg06_grp0_ld0_id} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name}" | ssh ${dn_server_ip_A}
# delete leg06 grp0 ld1
echo "sudo /tmp/dn_ld_delete.sh ${leg06_grp0_ld1_dn_mgr_id} ${leg06_grp0_ld1_dn_port_num} ${vd_id} ${leg06_grp0_ld1_id} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name}" | ssh ${dn_server_ip_B}
# delete leg07 grp0 ld0
echo "sudo /tmp/dn_ld_delete.sh ${leg07_grp0_ld0_dn_mgr_id} ${leg07_grp0_ld0_dn_port_num} ${vd_id} ${leg07_grp0_ld0_id} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name}" | ssh ${dn_server_ip_A}
# delete leg07 grp0 ld1
echo "sudo /tmp/dn_ld_delete.sh ${leg07_grp0_ld1_dn_mgr_id} ${leg07_grp0_ld1_dn_port_num} ${vd_id} ${leg07_grp0_ld1_id} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name}" | ssh ${dn_server_ip_B}

# create leg00
echo "sudo /tmp/cn_leg_create.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg00_id} ${leg00_grp0_id} ${leg00_grp0_ld0_id} ${leg00_grp0_ld0_dn_mgr_id} ${leg00_grp0_ld0_dn_tr_addr} ${leg00_grp0_ld0_dn_tr_svc_id} ${leg00_grp0_ld1_id} ${leg00_grp0_ld1_dn_mgr_id} ${leg00_grp0_ld1_dn_tr_addr} ${leg00_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name}" | ssh ${cn_server_ip_A}
# create leg01
echo "sudo /tmp/cn_leg_create.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg01_id} ${leg01_grp0_id} ${leg01_grp0_ld0_id} ${leg01_grp0_ld0_dn_mgr_id} ${leg01_grp0_ld0_dn_tr_addr} ${leg01_grp0_ld0_dn_tr_svc_id} ${leg01_grp0_ld1_id} ${leg01_grp0_ld1_dn_mgr_id} ${leg01_grp0_ld1_dn_tr_addr} ${leg01_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name}" | ssh ${cn_server_ip_A}
# create leg02
echo "sudo /tmp/cn_leg_create.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg02_id} ${leg02_grp0_id} ${leg02_grp0_ld0_id} ${leg02_grp0_ld0_dn_mgr_id} ${leg02_grp0_ld0_dn_tr_addr} ${leg02_grp0_ld0_dn_tr_svc_id} ${leg02_grp0_ld1_id} ${leg02_grp0_ld1_dn_mgr_id} ${leg02_grp0_ld1_dn_tr_addr} ${leg02_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name}" | ssh ${cn_server_ip_A}
# create leg03
echo "sudo /tmp/cn_leg_create.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg03_id} ${leg03_grp0_id} ${leg03_grp0_ld0_id} ${leg03_grp0_ld0_dn_mgr_id} ${leg03_grp0_ld0_dn_tr_addr} ${leg03_grp0_ld0_dn_tr_svc_id} ${leg03_grp0_ld1_id} ${leg03_grp0_ld1_dn_mgr_id} ${leg03_grp0_ld1_dn_tr_addr} ${leg03_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name}" | ssh ${cn_server_ip_A}
# create leg04
echo "sudo /tmp/cn_leg_create.sh ${cntl01_cn_mgr_id} ${cntl01_cn_port_num} ${cntl01_cn_host_name} ${vd_id} ${leg04_id} ${leg04_grp0_id} ${leg04_grp0_ld0_id} ${leg04_grp0_ld0_dn_mgr_id} ${leg04_grp0_ld0_dn_tr_addr} ${leg04_grp0_ld0_dn_tr_svc_id} ${leg04_grp0_ld1_id} ${leg04_grp0_ld1_dn_mgr_id} ${leg04_grp0_ld1_dn_tr_addr} ${leg04_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name}" | ssh ${cn_server_ip_B}
# create leg05
echo "sudo /tmp/cn_leg_create.sh ${cntl01_cn_mgr_id} ${cntl01_cn_port_num} ${cntl01_cn_host_name} ${vd_id} ${leg05_id} ${leg05_grp0_id} ${leg05_grp0_ld0_id} ${leg05_grp0_ld0_dn_mgr_id} ${leg05_grp0_ld0_dn_tr_addr} ${leg05_grp0_ld0_dn_tr_svc_id} ${leg05_grp0_ld1_id} ${leg05_grp0_ld1_dn_mgr_id} ${leg05_grp0_ld1_dn_tr_addr} ${leg05_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name}" | ssh ${cn_server_ip_B}
# create leg06
echo "sudo /tmp/cn_leg_create.sh ${cntl01_cn_mgr_id} ${cntl01_cn_port_num} ${cntl01_cn_host_name} ${vd_id} ${leg06_id} ${leg06_grp0_id} ${leg06_grp0_ld0_id} ${leg06_grp0_ld0_dn_mgr_id} ${leg06_grp0_ld0_dn_tr_addr} ${leg06_grp0_ld0_dn_tr_svc_id} ${leg06_grp0_ld1_id} ${leg06_grp0_ld1_dn_mgr_id} ${leg06_grp0_ld1_dn_tr_addr} ${leg06_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name}" | ssh ${cn_server_ip_B}
# create leg07
echo "sudo /tmp/cn_leg_create.sh ${cntl01_cn_mgr_id} ${cntl01_cn_port_num} ${cntl01_cn_host_name} ${vd_id} ${leg07_id} ${leg07_grp0_id} ${leg07_grp0_ld0_id} ${leg07_grp0_ld0_dn_mgr_id} ${leg07_grp0_ld0_dn_tr_addr} ${leg07_grp0_ld0_dn_tr_svc_id} ${leg07_grp0_ld1_id} ${leg07_grp0_ld1_dn_mgr_id} ${leg07_grp0_ld1_dn_tr_addr} ${leg07_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name}" | ssh ${cn_server_ip_B}

# delete leg00
echo "sudo /tmp/cn_leg_delete.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg00_id} ${leg00_grp0_id} ${leg00_grp0_ld0_id} ${leg00_grp0_ld0_dn_mgr_id} ${leg00_grp0_ld0_dn_tr_addr} ${leg00_grp0_ld0_dn_tr_svc_id} ${leg00_grp0_ld1_id} ${leg00_grp0_ld1_dn_mgr_id} ${leg00_grp0_ld1_dn_tr_addr} ${leg00_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name}" | ssh ${cn_server_ip_A}
# delete leg01
echo "sudo /tmp/cn_leg_delete.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg01_id} ${leg01_grp0_id} ${leg01_grp0_ld0_id} ${leg01_grp0_ld0_dn_mgr_id} ${leg01_grp0_ld0_dn_tr_addr} ${leg01_grp0_ld0_dn_tr_svc_id} ${leg01_grp0_ld1_id} ${leg01_grp0_ld1_dn_mgr_id} ${leg01_grp0_ld1_dn_tr_addr} ${leg01_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name}" | ssh ${cn_server_ip_A}
# delete leg02
echo "sudo /tmp/cn_leg_delete.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg02_id} ${leg02_grp0_id} ${leg02_grp0_ld0_id} ${leg02_grp0_ld0_dn_mgr_id} ${leg02_grp0_ld0_dn_tr_addr} ${leg02_grp0_ld0_dn_tr_svc_id} ${leg02_grp0_ld1_id} ${leg02_grp0_ld1_dn_mgr_id} ${leg02_grp0_ld1_dn_tr_addr} ${leg02_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name}" | ssh ${cn_server_ip_A}
# delete leg03
echo "sudo /tmp/cn_leg_delete.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg03_id} ${leg03_grp0_id} ${leg03_grp0_ld0_id} ${leg03_grp0_ld0_dn_mgr_id} ${leg03_grp0_ld0_dn_tr_addr} ${leg03_grp0_ld0_dn_tr_svc_id} ${leg03_grp0_ld1_id} ${leg03_grp0_ld1_dn_mgr_id} ${leg03_grp0_ld1_dn_tr_addr} ${leg03_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name}" | ssh ${cn_server_ip_A}
# delete leg04
echo "sudo /tmp/cn_leg_delete.sh ${cntl01_cn_mgr_id} ${cntl01_cn_port_num} ${cntl01_cn_host_name} ${vd_id} ${leg04_id} ${leg04_grp0_id} ${leg04_grp0_ld0_id} ${leg04_grp0_ld0_dn_mgr_id} ${leg04_grp0_ld0_dn_tr_addr} ${leg04_grp0_ld0_dn_tr_svc_id} ${leg04_grp0_ld1_id} ${leg04_grp0_ld1_dn_mgr_id} ${leg04_grp0_ld1_dn_tr_addr} ${leg04_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name}" | ssh ${cn_server_ip_B}
# delete leg05
echo "sudo /tmp/cn_leg_delete.sh ${cntl01_cn_mgr_id} ${cntl01_cn_port_num} ${cntl01_cn_host_name} ${vd_id} ${leg05_id} ${leg05_grp0_id} ${leg05_grp0_ld0_id} ${leg05_grp0_ld0_dn_mgr_id} ${leg05_grp0_ld0_dn_tr_addr} ${leg05_grp0_ld0_dn_tr_svc_id} ${leg05_grp0_ld1_id} ${leg05_grp0_ld1_dn_mgr_id} ${leg05_grp0_ld1_dn_tr_addr} ${leg05_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name}" | ssh ${cn_server_ip_B}
# delete leg06
echo "sudo /tmp/cn_leg_delete.sh ${cntl01_cn_mgr_id} ${cntl01_cn_port_num} ${cntl01_cn_host_name} ${vd_id} ${leg06_id} ${leg06_grp0_id} ${leg06_grp0_ld0_id} ${leg06_grp0_ld0_dn_mgr_id} ${leg06_grp0_ld0_dn_tr_addr} ${leg06_grp0_ld0_dn_tr_svc_id} ${leg06_grp0_ld1_id} ${leg06_grp0_ld1_dn_mgr_id} ${leg06_grp0_ld1_dn_tr_addr} ${leg06_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name}" | ssh ${cn_server_ip_B}
# delete leg07
echo "sudo /tmp/cn_leg_delete.sh ${cntl01_cn_mgr_id} ${cntl01_cn_port_num} ${cntl01_cn_host_name} ${vd_id} ${leg07_id} ${leg07_grp0_id} ${leg07_grp0_ld0_id} ${leg07_grp0_ld0_dn_mgr_id} ${leg07_grp0_ld0_dn_tr_addr} ${leg07_grp0_ld0_dn_tr_svc_id} ${leg07_grp0_ld1_id} ${leg07_grp0_ld1_dn_mgr_id} ${leg07_grp0_ld1_dn_tr_addr} ${leg07_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name}" | ssh ${cn_server_ip_B}

# create cntl00
echo "sudo /tmp/cn_cntl_create.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${cntl00_cntlid_min} ${cntl00_cntlid_max} ${vd_id} ${external_host_nqn} ${thin_dev_size_mb} ${leg_cnt} ${leg00_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg01_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg02_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg03_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg04_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg05_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg06_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg07_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id}" | ssh ${cn_server_ip_A}
# create cntl01
echo "sudo /tmp/cn_cntl_create.sh ${cntl01_cn_mgr_id} ${cntl01_cn_port_num} ${cntl01_cn_host_name} ${cntl01_cntlid_min} ${cntl01_cntlid_max} ${vd_id} ${external_host_nqn} ${thin_dev_size_mb} ${leg_cnt} ${leg00_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg01_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg02_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg03_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg04_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg05_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg06_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg07_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id}" | ssh ${cn_server_ip_B}

# delete cntl00
echo "sudo /tmp/cn_cntl_delete.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${external_host_nqn} ${leg_cnt} ${leg00_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg01_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg02_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg03_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg04_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg05_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg06_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg07_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id}" | ssh ${cn_server_ip_A}
# delete cntl01
echo "sudo /tmp/cn_cntl_delete.sh ${cntl01_cn_mgr_id} ${cntl01_cn_port_num} ${cntl01_cn_host_name} ${vd_id} ${external_host_nqn} ${leg_cnt} ${leg00_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg01_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg02_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg03_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg04_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg05_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg06_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg07_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id}" | ssh ${cn_server_ip_B}

sudo nvme connect --nqn ${vd_nqn} --transport tcp --traddr ${cntl00_cn_tr_addr} --trsvcid ${cntl00_cn_tr_svc_id} --hostnqn ${external_host_nqn}
sudo nvme connect --nqn ${vd_nqn} --transport tcp --traddr ${cntl01_cn_tr_addr} --trsvcid ${cntl01_cn_tr_svc_id} --hostnqn ${external_host_nqn}
subsys_name=$(sudo nvme list-subsys --output-format json | jq -rM ".Subsystems[] | select(.NQN==\"${vd_nqn}\") | .Name")
sudo bash -c "echo round-robin > /sys/class/nvme-subsystem/${subsys_name}/iopolicy"

sudo parted -s "${dev_path}" unit s print
sudo fio --filename=${dev_path} --name mytest --rw=randwrite --ioengine=libaio --iodepth=4 --bs=4k --numjobs=144 --direct=1 --time_based --runtime=60 --group_reporting --norandommap
# mytest: (g=0): rw=randwrite, bs=(R) 4096B-4096B, (W) 4096B-4096B, (T) 4096B-4096B, ioengine=libaio, iodepth=4
# ...
# fio-3.32
# Starting 144 processes
# Jobs: 144 (f=144): [w(144)][100.0%][w=2797MiB/s][w=716k IOPS][eta 00m:00s]
# mytest: (groupid=0, jobs=144): err= 0: pid=11232: Fri Feb 23 05:46:38 2024
#   write: IOPS=714k, BW=2788MiB/s (2923MB/s)(163GiB/60002msec); 0 zone resets
#     slat (nsec): min=1808, max=2780.3k, avg=9112.60, stdev=5654.33
#     clat (usec): min=174, max=71244, avg=796.65, stdev=333.84
#      lat (usec): min=182, max=71250, avg=805.77, stdev=333.71
#     clat percentiles (usec):
#      |  1.00th=[  424],  5.00th=[  486], 10.00th=[  523], 20.00th=[  578],
#      | 30.00th=[  635], 40.00th=[  701], 50.00th=[  766], 60.00th=[  832],
#      | 70.00th=[  898], 80.00th=[  979], 90.00th=[ 1090], 95.00th=[ 1221],
#      | 99.00th=[ 1516], 99.50th=[ 1680], 99.90th=[ 2114], 99.95th=[ 2474],
#      | 99.99th=[ 7177]
#    bw (  MiB/s): min= 1925, max= 2860, per=100.00%, avg=2790.53, stdev= 0.59, samples=17136
#    iops        : min=492880, max=732242, avg=714371.87, stdev=149.79, samples=17136
#   lat (usec)   : 250=0.01%, 500=6.90%, 750=40.63%, 1000=34.96%
#   lat (msec)   : 2=17.37%, 4=0.12%, 10=0.02%, 20=0.01%, 50=0.01%
#   lat (msec)   : 100=0.01%
#   cpu          : usr=1.49%, sys=5.54%, ctx=41741111, majf=2, minf=1401
#   IO depths    : 1=0.1%, 2=0.1%, 4=100.0%, 8=0.0%, 16=0.0%, 32=0.0%, >=64=0.0%
#      submit    : 0=0.0%, 4=100.0%, 8=0.0%, 16=0.0%, 32=0.0%, 64=0.0%, >=64=0.0%
#      complete  : 0=0.0%, 4=100.0%, 8=0.0%, 16=0.0%, 32=0.0%, 64=0.0%, >=64=0.0%
#      issued rwts: total=0,42823101,0,0 short=0,0,0,0 dropped=0,0,0,0
#      latency   : target=0, window=0, percentile=100.00%, depth=4

sudo nvme disconnect --nqn ${vd_nqn}
