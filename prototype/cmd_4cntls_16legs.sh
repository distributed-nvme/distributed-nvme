#!/bin/bash

dn_server_ip_A=192.168.0.157
dn_server_ip_B=192.168.0.127
dn_server_ip_C=192.168.0.78
dn_server_ip_D=192.168.0.147
cn_server_ip_A=192.168.0.39
cn_server_ip_B=192.168.0.212
cn_server_ip_C=192.168.0.114
cn_server_ip_D=192.168.0.42

dn_host_name_A="dn_host_A"
dn_host_name_B="dn_host_B"
dn_host_name_C="dn_host_C"
dn_host_name_D="dn_host_D"
cn_host_name_A="cn_host_A"
cn_host_name_B="cn_host_B"
cn_host_name_C="cn_host_C"
cn_host_name_D="cn_host_D"

leg_cnt=16
cntl_cnt=4

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

leg08_grp0_ld0_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg08_grp0_ld0_pd_path="/dev/disk/by-path/pci-0000:00:18.0-nvme-1"
leg08_grp0_ld0_dn_port_num=1
leg08_grp0_ld0_dn_tr_addr=${dn_server_ip_C}
leg08_grp0_ld0_dn_tr_svc_id=4410

leg08_grp0_ld1_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg08_grp0_ld1_pd_path="/dev/disk/by-path/pci-0000:00:18.0-nvme-1"
leg08_grp0_ld1_dn_port_num=1
leg08_grp0_ld1_dn_tr_addr=${dn_server_ip_D}
leg08_grp0_ld1_dn_tr_svc_id=4410

leg09_grp0_ld0_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg09_grp0_ld0_pd_path="/dev/disk/by-path/pci-0000:00:19.0-nvme-1"
leg09_grp0_ld0_dn_port_num=2
leg09_grp0_ld0_dn_tr_addr=${dn_server_ip_C}
leg09_grp0_ld0_dn_tr_svc_id=4411

leg09_grp0_ld1_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg09_grp0_ld1_pd_path="/dev/disk/by-path/pci-0000:00:19.0-nvme-1"
leg09_grp0_ld1_dn_port_num=2
leg09_grp0_ld1_dn_tr_addr=${dn_server_ip_D}
leg09_grp0_ld1_dn_tr_svc_id=4411

leg10_grp0_ld0_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg10_grp0_ld0_pd_path="/dev/disk/by-path/pci-0000:00:1a.0-nvme-1"
leg10_grp0_ld0_dn_port_num=3
leg10_grp0_ld0_dn_tr_addr=${dn_server_ip_C}
leg10_grp0_ld0_dn_tr_svc_id=4412

leg10_grp0_ld1_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg10_grp0_ld1_pd_path="/dev/disk/by-path/pci-0000:00:1a.0-nvme-1"
leg10_grp0_ld1_dn_port_num=3
leg10_grp0_ld1_dn_tr_addr=${dn_server_ip_D}
leg10_grp0_ld1_dn_tr_svc_id=4412

leg11_grp0_ld0_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg11_grp0_ld0_pd_path="/dev/disk/by-path/pci-0000:00:1b.0-nvme-1"
leg11_grp0_ld0_dn_port_num=4
leg11_grp0_ld0_dn_tr_addr=${dn_server_ip_C}
leg11_grp0_ld0_dn_tr_svc_id=4413

leg11_grp0_ld1_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg11_grp0_ld1_pd_path="/dev/disk/by-path/pci-0000:00:1b.0-nvme-1"
leg11_grp0_ld1_dn_port_num=4
leg11_grp0_ld1_dn_tr_addr=${dn_server_ip_D}
leg11_grp0_ld1_dn_tr_svc_id=4413

leg12_grp0_ld0_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg12_grp0_ld0_pd_path="/dev/disk/by-path/pci-0000:00:1c.0-nvme-1"
leg12_grp0_ld0_dn_port_num=5
leg12_grp0_ld0_dn_tr_addr=${dn_server_ip_C}
leg12_grp0_ld0_dn_tr_svc_id=4414

leg12_grp0_ld1_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg12_grp0_ld1_pd_path="/dev/disk/by-path/pci-0000:00:1c.0-nvme-1"
leg12_grp0_ld1_dn_port_num=5
leg12_grp0_ld1_dn_tr_addr=${dn_server_ip_D}
leg12_grp0_ld1_dn_tr_svc_id=4414

leg13_grp0_ld0_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg13_grp0_ld0_pd_path="/dev/disk/by-path/pci-0000:00:1d.0-nvme-1"
leg13_grp0_ld0_dn_port_num=6
leg13_grp0_ld0_dn_tr_addr=${dn_server_ip_C}
leg13_grp0_ld0_dn_tr_svc_id=4415

leg13_grp0_ld1_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg13_grp0_ld1_pd_path="/dev/disk/by-path/pci-0000:00:1d.0-nvme-1"
leg13_grp0_ld1_dn_port_num=6
leg13_grp0_ld1_dn_tr_addr=${dn_server_ip_D}
leg13_grp0_ld1_dn_tr_svc_id=4415

leg14_grp0_ld0_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg14_grp0_ld0_pd_path="/dev/disk/by-path/pci-0000:00:1e.0-nvme-1"
leg14_grp0_ld0_dn_port_num=7
leg14_grp0_ld0_dn_tr_addr=${dn_server_ip_C}
leg14_grp0_ld0_dn_tr_svc_id=4416

leg14_grp0_ld1_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg14_grp0_ld1_pd_path="/dev/disk/by-path/pci-0000:00:1e.0-nvme-1"
leg14_grp0_ld1_dn_port_num=7
leg14_grp0_ld1_dn_tr_addr=${dn_server_ip_D}
leg14_grp0_ld1_dn_tr_svc_id=4416

leg15_grp0_ld0_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg15_grp0_ld0_pd_path="/dev/disk/by-path/pci-0000:00:1f.0-nvme-1"
leg15_grp0_ld0_dn_port_num=8
leg15_grp0_ld0_dn_tr_addr=${dn_server_ip_C}
leg15_grp0_ld0_dn_tr_svc_id=4417

leg15_grp0_ld1_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg15_grp0_ld1_pd_path="/dev/disk/by-path/pci-0000:00:1f.0-nvme-1"
leg15_grp0_ld1_dn_port_num=8
leg15_grp0_ld1_dn_tr_addr=${dn_server_ip_D}
leg15_grp0_ld1_dn_tr_svc_id=4417

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

cntl02_cn_mgr_id=${global_id} && global_id=$((global_id+1))
cntl02_cn_host_name=${cn_host_name_C}
cntl02_cn_port_num=1
cntl02_cn_tr_addr=${cn_server_ip_C}
cntl02_cn_tr_svc_id=4420
cntl02_cntlid_min=30000
cntl02_cntlid_max=34999

cntl03_cn_mgr_id=${global_id} && global_id=$((global_id+1))
cntl03_cn_host_name=${cn_host_name_D}
cntl03_cn_port_num=1
cntl03_cn_tr_addr=${cn_server_ip_D}
cntl03_cn_tr_svc_id=4420
cntl03_cntlid_min=35000
cntl03_cntlid_max=39999

forward_cn_cnt=3

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

leg08_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg08_grp0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg08_grp0_ld0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg08_grp0_ld1_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))

leg09_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg09_grp0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg09_grp0_ld0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg09_grp0_ld1_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))

leg10_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg10_grp0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg10_grp0_ld0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg10_grp0_ld1_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))

leg11_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg11_grp0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg11_grp0_ld0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg11_grp0_ld1_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))

leg12_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg12_grp0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg12_grp0_ld0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg12_grp0_ld1_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))

leg13_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg13_grp0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg13_grp0_ld0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg13_grp0_ld1_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))

leg14_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg14_grp0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg14_grp0_ld0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg14_grp0_ld1_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))

leg15_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg15_grp0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg15_grp0_ld0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg15_grp0_ld1_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))

external_host_nqn="nqn.2014-08.org.nvmexpress:uuid:16c2fe2c-94fd-4a9b-b0d2-fab74d3fb38b"
UUID_NAMESPACE="37833e01-35d4-4e5a-b0a1-fff158b9d03b"
RANDOM_PREFIX=$(echo "${UUID_NAMESPACE}" | tr -d "-")
vd_nqn="nqn.2024-01.io.dnv.${RANDOM_PREFIX}:00000000:$(printf %08x ${vd_id}):1200:00000000"
dev_uuid=$(uuidgen --md5 --namespace "${UUID_NAMESPACE}" --name "${vd_nqn}")
dev_path="/dev/disk/by-id/nvme-uuid.${dev_uuid}"

echo "sudo modprobe nvme-tcp && sudo modprobe nvmet && sudo modprobe nvmet-tcp" | ssh ${dn_server_ip_A}
echo "sudo modprobe nvme-tcp && sudo modprobe nvmet && sudo modprobe nvmet-tcp" | ssh ${dn_server_ip_B}
echo "sudo modprobe nvme-tcp && sudo modprobe nvmet && sudo modprobe nvmet-tcp" | ssh ${dn_server_ip_C}
echo "sudo modprobe nvme-tcp && sudo modprobe nvmet && sudo modprobe nvmet-tcp" | ssh ${dn_server_ip_D}
echo "sudo modprobe nvme-tcp && sudo modprobe nvmet && sudo modprobe nvmet-tcp" | ssh ${cn_server_ip_A}
echo "sudo modprobe nvme-tcp && sudo modprobe nvmet && sudo modprobe nvmet-tcp" | ssh ${cn_server_ip_B}
echo "sudo modprobe nvme-tcp && sudo modprobe nvmet && sudo modprobe nvmet-tcp" | ssh ${cn_server_ip_C}
echo "sudo modprobe nvme-tcp && sudo modprobe nvmet && sudo modprobe nvmet-tcp" | ssh ${cn_server_ip_D}

scp *.sh ${dn_server_ip_A}:/tmp/ && scp *.sh ${dn_server_ip_B}:/tmp/ && scp *.sh ${dn_server_ip_C}:/tmp/ && scp *.sh ${dn_server_ip_D}:/tmp/ && scp *.sh ${cn_server_ip_A}:/tmp/ && scp *.sh ${cn_server_ip_B}:/tmp/ && scp *.sh ${cn_server_ip_C}:/tmp/ && scp *.sh ${cn_server_ip_D}:/tmp/

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
# prepare leg08 grp0 ld0
echo "sudo /tmp/dn_prepare.sh ${leg08_grp0_ld0_dn_port_num} ${leg08_grp0_ld0_dn_tr_addr} ${leg08_grp0_ld0_dn_tr_svc_id}" | ssh ${dn_server_ip_C}
# prepare leg08 grp0 ld1
echo "sudo /tmp/dn_prepare.sh ${leg08_grp0_ld1_dn_port_num} ${leg08_grp0_ld1_dn_tr_addr} ${leg08_grp0_ld1_dn_tr_svc_id}" | ssh ${dn_server_ip_D}
# prepare leg09 grp0 ld0
echo "sudo /tmp/dn_prepare.sh ${leg09_grp0_ld0_dn_port_num} ${leg09_grp0_ld0_dn_tr_addr} ${leg09_grp0_ld0_dn_tr_svc_id}" | ssh ${dn_server_ip_C}
# prepare leg09 grp0 ld1
echo "sudo /tmp/dn_prepare.sh ${leg09_grp0_ld1_dn_port_num} ${leg09_grp0_ld1_dn_tr_addr} ${leg09_grp0_ld1_dn_tr_svc_id}" | ssh ${dn_server_ip_D}
# prepare leg10 grp0 ld0
echo "sudo /tmp/dn_prepare.sh ${leg10_grp0_ld0_dn_port_num} ${leg10_grp0_ld0_dn_tr_addr} ${leg10_grp0_ld0_dn_tr_svc_id}" | ssh ${dn_server_ip_C}
# prepare leg10 grp0 ld1
echo "sudo /tmp/dn_prepare.sh ${leg10_grp0_ld1_dn_port_num} ${leg10_grp0_ld1_dn_tr_addr} ${leg10_grp0_ld1_dn_tr_svc_id}" | ssh ${dn_server_ip_D}
# prepare leg11 grp0 ld0
echo "sudo /tmp/dn_prepare.sh ${leg11_grp0_ld0_dn_port_num} ${leg11_grp0_ld0_dn_tr_addr} ${leg11_grp0_ld0_dn_tr_svc_id}" | ssh ${dn_server_ip_C}
# prepare leg11 grp0 ld1
echo "sudo /tmp/dn_prepare.sh ${leg11_grp0_ld1_dn_port_num} ${leg11_grp0_ld1_dn_tr_addr} ${leg11_grp0_ld1_dn_tr_svc_id}" | ssh ${dn_server_ip_D}
# prepare leg12 grp0 ld0
echo "sudo /tmp/dn_prepare.sh ${leg12_grp0_ld0_dn_port_num} ${leg12_grp0_ld0_dn_tr_addr} ${leg12_grp0_ld0_dn_tr_svc_id}" | ssh ${dn_server_ip_C}
# prepare leg12 grp0 ld1
echo "sudo /tmp/dn_prepare.sh ${leg12_grp0_ld1_dn_port_num} ${leg12_grp0_ld1_dn_tr_addr} ${leg12_grp0_ld1_dn_tr_svc_id}" | ssh ${dn_server_ip_D}
# prepare leg13 grp0 ld0
echo "sudo /tmp/dn_prepare.sh ${leg13_grp0_ld0_dn_port_num} ${leg13_grp0_ld0_dn_tr_addr} ${leg13_grp0_ld0_dn_tr_svc_id}" | ssh ${dn_server_ip_C}
# prepare leg13 grp0 ld1
echo "sudo /tmp/dn_prepare.sh ${leg13_grp0_ld1_dn_port_num} ${leg13_grp0_ld1_dn_tr_addr} ${leg13_grp0_ld1_dn_tr_svc_id}" | ssh ${dn_server_ip_D}
# prepare leg14 grp0 ld0
echo "sudo /tmp/dn_prepare.sh ${leg14_grp0_ld0_dn_port_num} ${leg14_grp0_ld0_dn_tr_addr} ${leg14_grp0_ld0_dn_tr_svc_id}" | ssh ${dn_server_ip_C}
# prepare leg14 grp0 ld1
echo "sudo /tmp/dn_prepare.sh ${leg14_grp0_ld1_dn_port_num} ${leg14_grp0_ld1_dn_tr_addr} ${leg14_grp0_ld1_dn_tr_svc_id}" | ssh ${dn_server_ip_D}
# prepare leg15 grp0 ld0
echo "sudo /tmp/dn_prepare.sh ${leg15_grp0_ld0_dn_port_num} ${leg15_grp0_ld0_dn_tr_addr} ${leg15_grp0_ld0_dn_tr_svc_id}" | ssh ${dn_server_ip_C}
# prepare leg15 grp0 ld1
echo "sudo /tmp/dn_prepare.sh ${leg15_grp0_ld1_dn_port_num} ${leg15_grp0_ld1_dn_tr_addr} ${leg15_grp0_ld1_dn_tr_svc_id}" | ssh ${dn_server_ip_D}

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
# cleanup leg08 grp0 ld0
echo "sudo /tmp/dn_cleanup.sh ${leg08_grp0_ld0_dn_port_num}" | ssh ${dn_server_ip_C}
# cleanup leg08 grp0 ld1
echo "sudo /tmp/dn_cleanup.sh ${leg08_grp0_ld1_dn_port_num}" | ssh ${dn_server_ip_D}
# cleanup leg09 grp0 ld0
echo "sudo /tmp/dn_cleanup.sh ${leg09_grp0_ld0_dn_port_num}" | ssh ${dn_server_ip_C}
# cleanup leg09 grp0 ld1
echo "sudo /tmp/dn_cleanup.sh ${leg09_grp0_ld1_dn_port_num}" | ssh ${dn_server_ip_D}
# cleanup leg10 grp0 ld0
echo "sudo /tmp/dn_cleanup.sh ${leg10_grp0_ld0_dn_port_num}" | ssh ${dn_server_ip_C}
# cleanup leg10 grp0 ld1
echo "sudo /tmp/dn_cleanup.sh ${leg10_grp0_ld1_dn_port_num}" | ssh ${dn_server_ip_D}
# cleanup leg11 grp0 ld0
echo "sudo /tmp/dn_cleanup.sh ${leg11_grp0_ld0_dn_port_num}" | ssh ${dn_server_ip_C}
# cleanup leg11 grp0 ld1
echo "sudo /tmp/dn_cleanup.sh ${leg11_grp0_ld1_dn_port_num}" | ssh ${dn_server_ip_D}
# cleanup leg12 grp0 ld0
echo "sudo /tmp/dn_cleanup.sh ${leg12_grp0_ld0_dn_port_num}" | ssh ${dn_server_ip_C}
# cleanup leg12 grp0 ld1
echo "sudo /tmp/dn_cleanup.sh ${leg12_grp0_ld1_dn_port_num}" | ssh ${dn_server_ip_D}
# cleanup leg13 grp0 ld0
echo "sudo /tmp/dn_cleanup.sh ${leg13_grp0_ld0_dn_port_num}" | ssh ${dn_server_ip_C}
# cleanup leg13 grp0 ld1
echo "sudo /tmp/dn_cleanup.sh ${leg13_grp0_ld1_dn_port_num}" | ssh ${dn_server_ip_D}
# cleanup leg14 grp0 ld0
echo "sudo /tmp/dn_cleanup.sh ${leg14_grp0_ld0_dn_port_num}" | ssh ${dn_server_ip_C}
# cleanup leg14 grp0 ld1
echo "sudo /tmp/dn_cleanup.sh ${leg14_grp0_ld1_dn_port_num}" | ssh ${dn_server_ip_D}
# cleanup leg15 grp0 ld0
echo "sudo /tmp/dn_cleanup.sh ${leg15_grp0_ld0_dn_port_num}" | ssh ${dn_server_ip_C}
# cleanup leg15 grp0 ld1
echo "sudo /tmp/dn_cleanup.sh ${leg15_grp0_ld1_dn_port_num}" | ssh ${dn_server_ip_D}

# prepare cntl00
echo "sudo /tmp/cn_prepare.sh ${cntl00_cn_port_num} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id}" | ssh ${cn_server_ip_A}
# prepare cntl01
echo "sudo /tmp/cn_prepare.sh ${cntl01_cn_port_num} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id}" | ssh ${cn_server_ip_B}
# prepare cntl02
echo "sudo /tmp/cn_prepare.sh ${cntl02_cn_port_num} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id}" | ssh ${cn_server_ip_C}
# prepare cntl03
echo "sudo /tmp/cn_prepare.sh ${cntl03_cn_port_num} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id}" | ssh ${cn_server_ip_D}

# cleanup cntl00
echo "sudo /tmp/cn_cleanup.sh ${cntl00_cn_port_num}" | ssh ${cn_server_ip_A}
# cleanup cntl01
echo "sudo /tmp/cn_cleanup.sh ${cntl01_cn_port_num}" | ssh ${cn_server_ip_B}
# cleanup cntl02
echo "sudo /tmp/cn_cleanup.sh ${cntl02_cn_port_num}" | ssh ${cn_server_ip_C}
# cleanup cntl03
echo "sudo /tmp/cn_cleanup.sh ${cntl03_cn_port_num}" | ssh ${cn_server_ip_D}

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
# create leg08 grp0 ld0
echo "sudo /tmp/dn_ld_create.sh ${leg08_grp0_ld0_dn_mgr_id} ${leg08_grp0_ld0_dn_port_num} ${leg08_grp0_ld0_pd_path} ${vd_id} ${leg08_grp0_ld0_id} ${ld_start_mb} ${ld_size_mb} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name}" | ssh ${dn_server_ip_C}
# create leg08 grp0 ld1
echo "sudo /tmp/dn_ld_create.sh ${leg08_grp0_ld1_dn_mgr_id} ${leg08_grp0_ld1_dn_port_num} ${leg08_grp0_ld1_pd_path} ${vd_id} ${leg08_grp0_ld1_id} ${ld_start_mb} ${ld_size_mb} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name}" | ssh ${dn_server_ip_D}
# create leg09 grp0 ld0
echo "sudo /tmp/dn_ld_create.sh ${leg09_grp0_ld0_dn_mgr_id} ${leg09_grp0_ld0_dn_port_num} ${leg09_grp0_ld0_pd_path} ${vd_id} ${leg09_grp0_ld0_id} ${ld_start_mb} ${ld_size_mb} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name}" | ssh ${dn_server_ip_C}
# create leg09 grp0 ld1
echo "sudo /tmp/dn_ld_create.sh ${leg09_grp0_ld1_dn_mgr_id} ${leg09_grp0_ld1_dn_port_num} ${leg09_grp0_ld1_pd_path} ${vd_id} ${leg09_grp0_ld1_id} ${ld_start_mb} ${ld_size_mb} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name}" | ssh ${dn_server_ip_D}
# create leg10 grp0 ld0
echo "sudo /tmp/dn_ld_create.sh ${leg10_grp0_ld0_dn_mgr_id} ${leg10_grp0_ld0_dn_port_num} ${leg10_grp0_ld0_pd_path} ${vd_id} ${leg10_grp0_ld0_id} ${ld_start_mb} ${ld_size_mb} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name}" | ssh ${dn_server_ip_C}
# create leg10 grp0 ld1
echo "sudo /tmp/dn_ld_create.sh ${leg10_grp0_ld1_dn_mgr_id} ${leg10_grp0_ld1_dn_port_num} ${leg10_grp0_ld1_pd_path} ${vd_id} ${leg10_grp0_ld1_id} ${ld_start_mb} ${ld_size_mb} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name}" | ssh ${dn_server_ip_D}
# create leg11 grp0 ld0
echo "sudo /tmp/dn_ld_create.sh ${leg11_grp0_ld0_dn_mgr_id} ${leg11_grp0_ld0_dn_port_num} ${leg11_grp0_ld0_pd_path} ${vd_id} ${leg11_grp0_ld0_id} ${ld_start_mb} ${ld_size_mb} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name}" | ssh ${dn_server_ip_C}
# create leg11 grp0 ld1
echo "sudo /tmp/dn_ld_create.sh ${leg11_grp0_ld1_dn_mgr_id} ${leg11_grp0_ld1_dn_port_num} ${leg11_grp0_ld1_pd_path} ${vd_id} ${leg11_grp0_ld1_id} ${ld_start_mb} ${ld_size_mb} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name}" | ssh ${dn_server_ip_D}
# create leg12 grp0 ld0
echo "sudo /tmp/dn_ld_create.sh ${leg12_grp0_ld0_dn_mgr_id} ${leg12_grp0_ld0_dn_port_num} ${leg12_grp0_ld0_pd_path} ${vd_id} ${leg12_grp0_ld0_id} ${ld_start_mb} ${ld_size_mb} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${dn_server_ip_C}
# create leg12 grp0 ld1
echo "sudo /tmp/dn_ld_create.sh ${leg12_grp0_ld1_dn_mgr_id} ${leg12_grp0_ld1_dn_port_num} ${leg12_grp0_ld1_pd_path} ${vd_id} ${leg12_grp0_ld1_id} ${ld_start_mb} ${ld_size_mb} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${dn_server_ip_D}
# create leg13 grp0 ld0
echo "sudo /tmp/dn_ld_create.sh ${leg13_grp0_ld0_dn_mgr_id} ${leg13_grp0_ld0_dn_port_num} ${leg13_grp0_ld0_pd_path} ${vd_id} ${leg13_grp0_ld0_id} ${ld_start_mb} ${ld_size_mb} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${dn_server_ip_C}
# create leg13 grp0 ld1
echo "sudo /tmp/dn_ld_create.sh ${leg13_grp0_ld1_dn_mgr_id} ${leg13_grp0_ld1_dn_port_num} ${leg13_grp0_ld1_pd_path} ${vd_id} ${leg13_grp0_ld1_id} ${ld_start_mb} ${ld_size_mb} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${dn_server_ip_D}
# create leg14 grp0 ld0
echo "sudo /tmp/dn_ld_create.sh ${leg14_grp0_ld0_dn_mgr_id} ${leg14_grp0_ld0_dn_port_num} ${leg14_grp0_ld0_pd_path} ${vd_id} ${leg14_grp0_ld0_id} ${ld_start_mb} ${ld_size_mb} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${dn_server_ip_C}
# create leg14 grp0 ld1
echo "sudo /tmp/dn_ld_create.sh ${leg14_grp0_ld1_dn_mgr_id} ${leg14_grp0_ld1_dn_port_num} ${leg14_grp0_ld1_pd_path} ${vd_id} ${leg14_grp0_ld1_id} ${ld_start_mb} ${ld_size_mb} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${dn_server_ip_D}
# create leg15 grp0 ld0
echo "sudo /tmp/dn_ld_create.sh ${leg15_grp0_ld0_dn_mgr_id} ${leg15_grp0_ld0_dn_port_num} ${leg15_grp0_ld0_pd_path} ${vd_id} ${leg15_grp0_ld0_id} ${ld_start_mb} ${ld_size_mb} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${dn_server_ip_C}
# create leg15 grp0 ld1
echo "sudo /tmp/dn_ld_create.sh ${leg15_grp0_ld1_dn_mgr_id} ${leg15_grp0_ld1_dn_port_num} ${leg15_grp0_ld1_pd_path} ${vd_id} ${leg15_grp0_ld1_id} ${ld_start_mb} ${ld_size_mb} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${dn_server_ip_D}

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
# delete leg08 grp0 ld0
echo "sudo /tmp/dn_ld_delete.sh ${leg08_grp0_ld0_dn_mgr_id} ${leg08_grp0_ld0_dn_port_num} ${vd_id} ${leg08_grp0_ld0_id} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name}" | ssh ${dn_server_ip_C}
# delete leg08 grp0 ld1
echo "sudo /tmp/dn_ld_delete.sh ${leg08_grp0_ld1_dn_mgr_id} ${leg08_grp0_ld1_dn_port_num} ${vd_id} ${leg08_grp0_ld1_id} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name}" | ssh ${dn_server_ip_D}
# delete leg09 grp0 ld0
echo "sudo /tmp/dn_ld_delete.sh ${leg09_grp0_ld0_dn_mgr_id} ${leg09_grp0_ld0_dn_port_num} ${vd_id} ${leg09_grp0_ld0_id} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name}" | ssh ${dn_server_ip_C}
# delete leg09 grp0 ld1
echo "sudo /tmp/dn_ld_delete.sh ${leg09_grp0_ld1_dn_mgr_id} ${leg09_grp0_ld1_dn_port_num} ${vd_id} ${leg09_grp0_ld1_id} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name}" | ssh ${dn_server_ip_D}
# delete leg10 grp0 ld0
echo "sudo /tmp/dn_ld_delete.sh ${leg10_grp0_ld0_dn_mgr_id} ${leg10_grp0_ld0_dn_port_num} ${vd_id} ${leg10_grp0_ld0_id} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name}" | ssh ${dn_server_ip_C}
# delete leg10 grp0 ld1
echo "sudo /tmp/dn_ld_delete.sh ${leg10_grp0_ld1_dn_mgr_id} ${leg10_grp0_ld1_dn_port_num} ${vd_id} ${leg10_grp0_ld1_id} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name}" | ssh ${dn_server_ip_D}
# delete leg11 grp0 ld0
echo "sudo /tmp/dn_ld_delete.sh ${leg11_grp0_ld0_dn_mgr_id} ${leg11_grp0_ld0_dn_port_num} ${vd_id} ${leg11_grp0_ld0_id} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name}" | ssh ${dn_server_ip_C}
# delete leg11 grp0 ld1
echo "sudo /tmp/dn_ld_delete.sh ${leg11_grp0_ld1_dn_mgr_id} ${leg11_grp0_ld1_dn_port_num} ${vd_id} ${leg11_grp0_ld1_id} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name}" | ssh ${dn_server_ip_D}
# delete leg12 grp0 ld0
echo "sudo /tmp/dn_ld_delete.sh ${leg12_grp0_ld0_dn_mgr_id} ${leg12_grp0_ld0_dn_port_num} ${vd_id} ${leg12_grp0_ld0_id} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${dn_server_ip_C}
# delete leg12 grp0 ld1
echo "sudo /tmp/dn_ld_delete.sh ${leg12_grp0_ld1_dn_mgr_id} ${leg12_grp0_ld1_dn_port_num} ${vd_id} ${leg12_grp0_ld1_id} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${dn_server_ip_D}
# delete leg13 grp0 ld0
echo "sudo /tmp/dn_ld_delete.sh ${leg13_grp0_ld0_dn_mgr_id} ${leg13_grp0_ld0_dn_port_num} ${vd_id} ${leg13_grp0_ld0_id} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${dn_server_ip_C}
# delete leg13 grp0 ld1
echo "sudo /tmp/dn_ld_delete.sh ${leg13_grp0_ld1_dn_mgr_id} ${leg13_grp0_ld1_dn_port_num} ${vd_id} ${leg13_grp0_ld1_id} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${dn_server_ip_D}
# delete leg14 grp0 ld0
echo "sudo /tmp/dn_ld_delete.sh ${leg14_grp0_ld0_dn_mgr_id} ${leg14_grp0_ld0_dn_port_num} ${vd_id} ${leg14_grp0_ld0_id} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${dn_server_ip_C}
# delete leg14 grp0 ld1
echo "sudo /tmp/dn_ld_delete.sh ${leg14_grp0_ld1_dn_mgr_id} ${leg14_grp0_ld1_dn_port_num} ${vd_id} ${leg14_grp0_ld1_id} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${dn_server_ip_D}
# delete leg15 grp0 ld0
echo "sudo /tmp/dn_ld_delete.sh ${leg15_grp0_ld0_dn_mgr_id} ${leg15_grp0_ld0_dn_port_num} ${vd_id} ${leg15_grp0_ld0_id} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${dn_server_ip_C}
# delete leg15 grp0 ld1
echo "sudo /tmp/dn_ld_delete.sh ${leg15_grp0_ld1_dn_mgr_id} ${leg15_grp0_ld1_dn_port_num} ${vd_id} ${leg15_grp0_ld1_id} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${dn_server_ip_D}

# create leg00
echo "sudo /tmp/cn_leg_create.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg00_id} ${leg00_grp0_id} ${leg00_grp0_ld0_id} ${leg00_grp0_ld0_dn_mgr_id} ${leg00_grp0_ld0_dn_tr_addr} ${leg00_grp0_ld0_dn_tr_svc_id} ${leg00_grp0_ld1_id} ${leg00_grp0_ld1_dn_mgr_id} ${leg00_grp0_ld1_dn_tr_addr} ${leg00_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${cn_server_ip_A}
# create leg01
echo "sudo /tmp/cn_leg_create.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg01_id} ${leg01_grp0_id} ${leg01_grp0_ld0_id} ${leg01_grp0_ld0_dn_mgr_id} ${leg01_grp0_ld0_dn_tr_addr} ${leg01_grp0_ld0_dn_tr_svc_id} ${leg01_grp0_ld1_id} ${leg01_grp0_ld1_dn_mgr_id} ${leg01_grp0_ld1_dn_tr_addr} ${leg01_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${cn_server_ip_A}
# create leg02
echo "sudo /tmp/cn_leg_create.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg02_id} ${leg02_grp0_id} ${leg02_grp0_ld0_id} ${leg02_grp0_ld0_dn_mgr_id} ${leg02_grp0_ld0_dn_tr_addr} ${leg02_grp0_ld0_dn_tr_svc_id} ${leg02_grp0_ld1_id} ${leg02_grp0_ld1_dn_mgr_id} ${leg02_grp0_ld1_dn_tr_addr} ${leg02_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${cn_server_ip_A}
# create leg03
echo "sudo /tmp/cn_leg_create.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg03_id} ${leg03_grp0_id} ${leg03_grp0_ld0_id} ${leg03_grp0_ld0_dn_mgr_id} ${leg03_grp0_ld0_dn_tr_addr} ${leg03_grp0_ld0_dn_tr_svc_id} ${leg03_grp0_ld1_id} ${leg03_grp0_ld1_dn_mgr_id} ${leg03_grp0_ld1_dn_tr_addr} ${leg03_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${cn_server_ip_A}
# create leg04
echo "sudo /tmp/cn_leg_create.sh ${cntl01_cn_mgr_id} ${cntl01_cn_port_num} ${cntl01_cn_host_name} ${vd_id} ${leg04_id} ${leg04_grp0_id} ${leg04_grp0_ld0_id} ${leg04_grp0_ld0_dn_mgr_id} ${leg04_grp0_ld0_dn_tr_addr} ${leg04_grp0_ld0_dn_tr_svc_id} ${leg04_grp0_ld1_id} ${leg04_grp0_ld1_dn_mgr_id} ${leg04_grp0_ld1_dn_tr_addr} ${leg04_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${cn_server_ip_B}
# create leg05
echo "sudo /tmp/cn_leg_create.sh ${cntl01_cn_mgr_id} ${cntl01_cn_port_num} ${cntl01_cn_host_name} ${vd_id} ${leg05_id} ${leg05_grp0_id} ${leg05_grp0_ld0_id} ${leg05_grp0_ld0_dn_mgr_id} ${leg05_grp0_ld0_dn_tr_addr} ${leg05_grp0_ld0_dn_tr_svc_id} ${leg05_grp0_ld1_id} ${leg05_grp0_ld1_dn_mgr_id} ${leg05_grp0_ld1_dn_tr_addr} ${leg05_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${cn_server_ip_B}
# create leg06
echo "sudo /tmp/cn_leg_create.sh ${cntl01_cn_mgr_id} ${cntl01_cn_port_num} ${cntl01_cn_host_name} ${vd_id} ${leg06_id} ${leg06_grp0_id} ${leg06_grp0_ld0_id} ${leg06_grp0_ld0_dn_mgr_id} ${leg06_grp0_ld0_dn_tr_addr} ${leg06_grp0_ld0_dn_tr_svc_id} ${leg06_grp0_ld1_id} ${leg06_grp0_ld1_dn_mgr_id} ${leg06_grp0_ld1_dn_tr_addr} ${leg06_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${cn_server_ip_B}
# create leg07
echo "sudo /tmp/cn_leg_create.sh ${cntl01_cn_mgr_id} ${cntl01_cn_port_num} ${cntl01_cn_host_name} ${vd_id} ${leg07_id} ${leg07_grp0_id} ${leg07_grp0_ld0_id} ${leg07_grp0_ld0_dn_mgr_id} ${leg07_grp0_ld0_dn_tr_addr} ${leg07_grp0_ld0_dn_tr_svc_id} ${leg07_grp0_ld1_id} ${leg07_grp0_ld1_dn_mgr_id} ${leg07_grp0_ld1_dn_tr_addr} ${leg07_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${cn_server_ip_B}
# create leg08
echo "sudo /tmp/cn_leg_create.sh ${cntl02_cn_mgr_id} ${cntl02_cn_port_num} ${cntl02_cn_host_name} ${vd_id} ${leg08_id} ${leg08_grp0_id} ${leg08_grp0_ld0_id} ${leg08_grp0_ld0_dn_mgr_id} ${leg08_grp0_ld0_dn_tr_addr} ${leg08_grp0_ld0_dn_tr_svc_id} ${leg08_grp0_ld1_id} ${leg08_grp0_ld1_dn_mgr_id} ${leg08_grp0_ld1_dn_tr_addr} ${leg08_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${cn_server_ip_C}
# create leg09
echo "sudo /tmp/cn_leg_create.sh ${cntl02_cn_mgr_id} ${cntl02_cn_port_num} ${cntl02_cn_host_name} ${vd_id} ${leg09_id} ${leg09_grp0_id} ${leg09_grp0_ld0_id} ${leg09_grp0_ld0_dn_mgr_id} ${leg09_grp0_ld0_dn_tr_addr} ${leg09_grp0_ld0_dn_tr_svc_id} ${leg09_grp0_ld1_id} ${leg09_grp0_ld1_dn_mgr_id} ${leg09_grp0_ld1_dn_tr_addr} ${leg09_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${cn_server_ip_C}
# create leg10
echo "sudo /tmp/cn_leg_create.sh ${cntl02_cn_mgr_id} ${cntl02_cn_port_num} ${cntl02_cn_host_name} ${vd_id} ${leg10_id} ${leg10_grp0_id} ${leg10_grp0_ld0_id} ${leg10_grp0_ld0_dn_mgr_id} ${leg10_grp0_ld0_dn_tr_addr} ${leg10_grp0_ld0_dn_tr_svc_id} ${leg10_grp0_ld1_id} ${leg10_grp0_ld1_dn_mgr_id} ${leg10_grp0_ld1_dn_tr_addr} ${leg10_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${cn_server_ip_C}
# create leg11
echo "sudo /tmp/cn_leg_create.sh ${cntl02_cn_mgr_id} ${cntl02_cn_port_num} ${cntl02_cn_host_name} ${vd_id} ${leg11_id} ${leg11_grp0_id} ${leg11_grp0_ld0_id} ${leg11_grp0_ld0_dn_mgr_id} ${leg11_grp0_ld0_dn_tr_addr} ${leg11_grp0_ld0_dn_tr_svc_id} ${leg11_grp0_ld1_id} ${leg11_grp0_ld1_dn_mgr_id} ${leg11_grp0_ld1_dn_tr_addr} ${leg11_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${cn_server_ip_C}
# create leg12
echo "sudo /tmp/cn_leg_create.sh ${cntl03_cn_mgr_id} ${cntl03_cn_port_num} ${cntl03_cn_host_name} ${vd_id} ${leg12_id} ${leg12_grp0_id} ${leg12_grp0_ld0_id} ${leg12_grp0_ld0_dn_mgr_id} ${leg12_grp0_ld0_dn_tr_addr} ${leg12_grp0_ld0_dn_tr_svc_id} ${leg12_grp0_ld1_id} ${leg12_grp0_ld1_dn_mgr_id} ${leg12_grp0_ld1_dn_tr_addr} ${leg12_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name}" | ssh ${cn_server_ip_D}
# create leg13
echo "sudo /tmp/cn_leg_create.sh ${cntl03_cn_mgr_id} ${cntl03_cn_port_num} ${cntl03_cn_host_name} ${vd_id} ${leg13_id} ${leg13_grp0_id} ${leg13_grp0_ld0_id} ${leg13_grp0_ld0_dn_mgr_id} ${leg13_grp0_ld0_dn_tr_addr} ${leg13_grp0_ld0_dn_tr_svc_id} ${leg13_grp0_ld1_id} ${leg13_grp0_ld1_dn_mgr_id} ${leg13_grp0_ld1_dn_tr_addr} ${leg13_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name}" | ssh ${cn_server_ip_D}
# create leg14
echo "sudo /tmp/cn_leg_create.sh ${cntl03_cn_mgr_id} ${cntl03_cn_port_num} ${cntl03_cn_host_name} ${vd_id} ${leg14_id} ${leg14_grp0_id} ${leg14_grp0_ld0_id} ${leg14_grp0_ld0_dn_mgr_id} ${leg14_grp0_ld0_dn_tr_addr} ${leg14_grp0_ld0_dn_tr_svc_id} ${leg14_grp0_ld1_id} ${leg14_grp0_ld1_dn_mgr_id} ${leg14_grp0_ld1_dn_tr_addr} ${leg14_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name}" | ssh ${cn_server_ip_D}
# create leg15
echo "sudo /tmp/cn_leg_create.sh ${cntl03_cn_mgr_id} ${cntl03_cn_port_num} ${cntl03_cn_host_name} ${vd_id} ${leg15_id} ${leg15_grp0_id} ${leg15_grp0_ld0_id} ${leg15_grp0_ld0_dn_mgr_id} ${leg15_grp0_ld0_dn_tr_addr} ${leg15_grp0_ld0_dn_tr_svc_id} ${leg15_grp0_ld1_id} ${leg15_grp0_ld1_dn_mgr_id} ${leg15_grp0_ld1_dn_tr_addr} ${leg15_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name}" | ssh ${cn_server_ip_D}

# delete leg00
echo "sudo /tmp/cn_leg_delete.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg00_id} ${leg00_grp0_id} ${leg00_grp0_ld0_id} ${leg00_grp0_ld0_dn_mgr_id} ${leg00_grp0_ld0_dn_tr_addr} ${leg00_grp0_ld0_dn_tr_svc_id} ${leg00_grp0_ld1_id} ${leg00_grp0_ld1_dn_mgr_id} ${leg00_grp0_ld1_dn_tr_addr} ${leg00_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${cn_server_ip_A}
# delete leg01
echo "sudo /tmp/cn_leg_delete.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg01_id} ${leg01_grp0_id} ${leg01_grp0_ld0_id} ${leg01_grp0_ld0_dn_mgr_id} ${leg01_grp0_ld0_dn_tr_addr} ${leg01_grp0_ld0_dn_tr_svc_id} ${leg01_grp0_ld1_id} ${leg01_grp0_ld1_dn_mgr_id} ${leg01_grp0_ld1_dn_tr_addr} ${leg01_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${cn_server_ip_A}
# delete leg02
echo "sudo /tmp/cn_leg_delete.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg02_id} ${leg02_grp0_id} ${leg02_grp0_ld0_id} ${leg02_grp0_ld0_dn_mgr_id} ${leg02_grp0_ld0_dn_tr_addr} ${leg02_grp0_ld0_dn_tr_svc_id} ${leg02_grp0_ld1_id} ${leg02_grp0_ld1_dn_mgr_id} ${leg02_grp0_ld1_dn_tr_addr} ${leg02_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${cn_server_ip_A}
# delete leg03
echo "sudo /tmp/cn_leg_delete.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg03_id} ${leg03_grp0_id} ${leg03_grp0_ld0_id} ${leg03_grp0_ld0_dn_mgr_id} ${leg03_grp0_ld0_dn_tr_addr} ${leg03_grp0_ld0_dn_tr_svc_id} ${leg03_grp0_ld1_id} ${leg03_grp0_ld1_dn_mgr_id} ${leg03_grp0_ld1_dn_tr_addr} ${leg03_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${cn_server_ip_A}
# delete leg04
echo "sudo /tmp/cn_leg_delete.sh ${cntl01_cn_mgr_id} ${cntl01_cn_port_num} ${cntl01_cn_host_name} ${vd_id} ${leg04_id} ${leg04_grp0_id} ${leg04_grp0_ld0_id} ${leg04_grp0_ld0_dn_mgr_id} ${leg04_grp0_ld0_dn_tr_addr} ${leg04_grp0_ld0_dn_tr_svc_id} ${leg04_grp0_ld1_id} ${leg04_grp0_ld1_dn_mgr_id} ${leg04_grp0_ld1_dn_tr_addr} ${leg04_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${cn_server_ip_B}
# delete leg05
echo "sudo /tmp/cn_leg_delete.sh ${cntl01_cn_mgr_id} ${cntl01_cn_port_num} ${cntl01_cn_host_name} ${vd_id} ${leg05_id} ${leg05_grp0_id} ${leg05_grp0_ld0_id} ${leg05_grp0_ld0_dn_mgr_id} ${leg05_grp0_ld0_dn_tr_addr} ${leg05_grp0_ld0_dn_tr_svc_id} ${leg05_grp0_ld1_id} ${leg05_grp0_ld1_dn_mgr_id} ${leg05_grp0_ld1_dn_tr_addr} ${leg05_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${cn_server_ip_B}
# delete leg06
echo "sudo /tmp/cn_leg_delete.sh ${cntl01_cn_mgr_id} ${cntl01_cn_port_num} ${cntl01_cn_host_name} ${vd_id} ${leg06_id} ${leg06_grp0_id} ${leg06_grp0_ld0_id} ${leg06_grp0_ld0_dn_mgr_id} ${leg06_grp0_ld0_dn_tr_addr} ${leg06_grp0_ld0_dn_tr_svc_id} ${leg06_grp0_ld1_id} ${leg06_grp0_ld1_dn_mgr_id} ${leg06_grp0_ld1_dn_tr_addr} ${leg06_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${cn_server_ip_B}
# delete leg07
echo "sudo /tmp/cn_leg_delete.sh ${cntl01_cn_mgr_id} ${cntl01_cn_port_num} ${cntl01_cn_host_name} ${vd_id} ${leg07_id} ${leg07_grp0_id} ${leg07_grp0_ld0_id} ${leg07_grp0_ld0_dn_mgr_id} ${leg07_grp0_ld0_dn_tr_addr} ${leg07_grp0_ld0_dn_tr_svc_id} ${leg07_grp0_ld1_id} ${leg07_grp0_ld1_dn_mgr_id} ${leg07_grp0_ld1_dn_tr_addr} ${leg07_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${cn_server_ip_B}
# delete leg08
echo "sudo /tmp/cn_leg_delete.sh ${cntl02_cn_mgr_id} ${cntl02_cn_port_num} ${cntl02_cn_host_name} ${vd_id} ${leg08_id} ${leg08_grp0_id} ${leg08_grp0_ld0_id} ${leg08_grp0_ld0_dn_mgr_id} ${leg08_grp0_ld0_dn_tr_addr} ${leg08_grp0_ld0_dn_tr_svc_id} ${leg08_grp0_ld1_id} ${leg08_grp0_ld1_dn_mgr_id} ${leg08_grp0_ld1_dn_tr_addr} ${leg08_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${cn_server_ip_C}
# delete leg09
echo "sudo /tmp/cn_leg_delete.sh ${cntl02_cn_mgr_id} ${cntl02_cn_port_num} ${cntl02_cn_host_name} ${vd_id} ${leg09_id} ${leg09_grp0_id} ${leg09_grp0_ld0_id} ${leg09_grp0_ld0_dn_mgr_id} ${leg09_grp0_ld0_dn_tr_addr} ${leg09_grp0_ld0_dn_tr_svc_id} ${leg09_grp0_ld1_id} ${leg09_grp0_ld1_dn_mgr_id} ${leg09_grp0_ld1_dn_tr_addr} ${leg09_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${cn_server_ip_C}
# delete leg10
echo "sudo /tmp/cn_leg_delete.sh ${cntl02_cn_mgr_id} ${cntl02_cn_port_num} ${cntl02_cn_host_name} ${vd_id} ${leg10_id} ${leg10_grp0_id} ${leg10_grp0_ld0_id} ${leg10_grp0_ld0_dn_mgr_id} ${leg10_grp0_ld0_dn_tr_addr} ${leg10_grp0_ld0_dn_tr_svc_id} ${leg10_grp0_ld1_id} ${leg10_grp0_ld1_dn_mgr_id} ${leg10_grp0_ld1_dn_tr_addr} ${leg10_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${cn_server_ip_C}
# delete leg11
echo "sudo /tmp/cn_leg_delete.sh ${cntl02_cn_mgr_id} ${cntl02_cn_port_num} ${cntl02_cn_host_name} ${vd_id} ${leg11_id} ${leg11_grp0_id} ${leg11_grp0_ld0_id} ${leg11_grp0_ld0_dn_mgr_id} ${leg11_grp0_ld0_dn_tr_addr} ${leg11_grp0_ld0_dn_tr_svc_id} ${leg11_grp0_ld1_id} ${leg11_grp0_ld1_dn_mgr_id} ${leg11_grp0_ld1_dn_tr_addr} ${leg11_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name}" | ssh ${cn_server_ip_C}
# delete leg12
echo "sudo /tmp/cn_leg_delete.sh ${cntl03_cn_mgr_id} ${cntl03_cn_port_num} ${cntl03_cn_host_name} ${vd_id} ${leg12_id} ${leg12_grp0_id} ${leg12_grp0_ld0_id} ${leg12_grp0_ld0_dn_mgr_id} ${leg12_grp0_ld0_dn_tr_addr} ${leg12_grp0_ld0_dn_tr_svc_id} ${leg12_grp0_ld1_id} ${leg12_grp0_ld1_dn_mgr_id} ${leg12_grp0_ld1_dn_tr_addr} ${leg12_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name}" | ssh ${cn_server_ip_D}
# delete leg13
echo "sudo /tmp/cn_leg_delete.sh ${cntl03_cn_mgr_id} ${cntl03_cn_port_num} ${cntl03_cn_host_name} ${vd_id} ${leg13_id} ${leg13_grp0_id} ${leg13_grp0_ld0_id} ${leg13_grp0_ld0_dn_mgr_id} ${leg13_grp0_ld0_dn_tr_addr} ${leg13_grp0_ld0_dn_tr_svc_id} ${leg13_grp0_ld1_id} ${leg13_grp0_ld1_dn_mgr_id} ${leg13_grp0_ld1_dn_tr_addr} ${leg13_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name}" | ssh ${cn_server_ip_D}
# delete leg14
echo "sudo /tmp/cn_leg_delete.sh ${cntl03_cn_mgr_id} ${cntl03_cn_port_num} ${cntl03_cn_host_name} ${vd_id} ${leg14_id} ${leg14_grp0_id} ${leg14_grp0_ld0_id} ${leg14_grp0_ld0_dn_mgr_id} ${leg14_grp0_ld0_dn_tr_addr} ${leg14_grp0_ld0_dn_tr_svc_id} ${leg14_grp0_ld1_id} ${leg14_grp0_ld1_dn_mgr_id} ${leg14_grp0_ld1_dn_tr_addr} ${leg14_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name}" | ssh ${cn_server_ip_D}
# delete leg15
echo "sudo /tmp/cn_leg_delete.sh ${cntl03_cn_mgr_id} ${cntl03_cn_port_num} ${cntl03_cn_host_name} ${vd_id} ${leg15_id} ${leg15_grp0_id} ${leg15_grp0_ld0_id} ${leg15_grp0_ld0_dn_mgr_id} ${leg15_grp0_ld0_dn_tr_addr} ${leg15_grp0_ld0_dn_tr_svc_id} ${leg15_grp0_ld1_id} ${leg15_grp0_ld1_dn_mgr_id} ${leg15_grp0_ld1_dn_tr_addr} ${leg15_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name}" | ssh ${cn_server_ip_D}

# create cntl00
echo "sudo /tmp/cn_cntl_create.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${cntl00_cntlid_min} ${cntl00_cntlid_max} ${vd_id} ${external_host_nqn} ${thin_dev_size_mb} ${leg_cnt} ${leg00_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg01_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg02_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg03_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg04_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg05_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg06_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg07_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg08_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg09_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg10_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg11_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg12_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg13_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg14_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg15_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id}" | ssh ${cn_server_ip_A}
# create cntl01
echo "sudo /tmp/cn_cntl_create.sh ${cntl01_cn_mgr_id} ${cntl01_cn_port_num} ${cntl01_cn_host_name} ${cntl01_cntlid_min} ${cntl01_cntlid_max} ${vd_id} ${external_host_nqn} ${thin_dev_size_mb} ${leg_cnt} ${leg00_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg01_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg02_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg03_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg04_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg05_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg06_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg07_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg08_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg09_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg10_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg11_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg12_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg13_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg14_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg15_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id}" | ssh ${cn_server_ip_B}
# create cntl02
echo "sudo /tmp/cn_cntl_create.sh ${cntl02_cn_mgr_id} ${cntl02_cn_port_num} ${cntl02_cn_host_name} ${cntl02_cntlid_min} ${cntl02_cntlid_max} ${vd_id} ${external_host_nqn} ${thin_dev_size_mb} ${leg_cnt} ${leg00_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg01_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg02_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg03_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg04_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg05_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg06_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg07_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg08_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg09_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg10_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg11_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg12_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg13_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg14_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg15_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id}" | ssh ${cn_server_ip_C}
# create cntl03
echo "sudo /tmp/cn_cntl_create.sh ${cntl03_cn_mgr_id} ${cntl03_cn_port_num} ${cntl03_cn_host_name} ${cntl03_cntlid_min} ${cntl03_cntlid_max} ${vd_id} ${external_host_nqn} ${thin_dev_size_mb} ${leg_cnt} ${leg00_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg01_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg02_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg03_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg04_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg05_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg06_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg07_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg08_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg09_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg10_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg11_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg12_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg13_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg14_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg15_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id}" | ssh ${cn_server_ip_D}

# delete cntl00
echo "sudo /tmp/cn_cntl_delete.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${external_host_nqn} ${leg_cnt} ${leg00_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg01_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg02_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg03_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg04_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg05_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg06_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg07_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg08_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg09_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg10_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg11_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg12_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg13_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg14_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg15_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id}" | ssh ${cn_server_ip_A}
# delete cntl01
echo "sudo /tmp/cn_cntl_delete.sh ${cntl01_cn_mgr_id} ${cntl01_cn_port_num} ${cntl01_cn_host_name} ${vd_id} ${external_host_nqn} ${leg_cnt} ${leg00_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg01_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg02_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg03_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg04_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg05_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg06_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg07_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg08_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg09_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg10_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg11_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg12_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg13_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg14_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg15_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id}" | ssh ${cn_server_ip_B}
# delete cntl02
echo "sudo /tmp/cn_cntl_delete.sh ${cntl02_cn_mgr_id} ${cntl02_cn_port_num} ${cntl02_cn_host_name} ${vd_id} ${external_host_nqn} ${leg_cnt} ${leg00_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg01_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg02_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg03_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg04_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg05_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg06_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg07_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg08_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg09_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg10_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg11_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg12_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg13_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg14_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg15_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id}" | ssh ${cn_server_ip_C}
# delete cntl03
echo "sudo /tmp/cn_cntl_delete.sh ${cntl03_cn_mgr_id} ${cntl03_cn_port_num} ${cntl03_cn_host_name} ${vd_id} ${external_host_nqn} ${leg_cnt} ${leg00_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg01_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg02_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg03_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg04_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg05_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg06_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg07_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg08_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg09_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg10_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg11_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg12_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg13_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg14_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg15_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id}" | ssh ${cn_server_ip_D}

sudo nvme connect --nqn ${vd_nqn} --transport tcp --traddr ${cntl00_cn_tr_addr} --trsvcid ${cntl00_cn_tr_svc_id} --hostnqn ${external_host_nqn}
sudo nvme connect --nqn ${vd_nqn} --transport tcp --traddr ${cntl01_cn_tr_addr} --trsvcid ${cntl01_cn_tr_svc_id} --hostnqn ${external_host_nqn}
sudo nvme connect --nqn ${vd_nqn} --transport tcp --traddr ${cntl02_cn_tr_addr} --trsvcid ${cntl00_cn_tr_svc_id} --hostnqn ${external_host_nqn}
sudo nvme connect --nqn ${vd_nqn} --transport tcp --traddr ${cntl03_cn_tr_addr} --trsvcid ${cntl01_cn_tr_svc_id} --hostnqn ${external_host_nqn}

subsys_name=$(sudo nvme list-subsys --output-format json | jq -rM ".Subsystems[] | select(.NQN==\"${vd_nqn}\") | .Name")
sudo bash -c "echo round-robin > /sys/class/nvme-subsystem/${subsys_name}/iopolicy"

sudo parted -s "${dev_path}" unit s print
sudo fio --filename=${dev_path} --name mytest --rw=randwrite --ioengine=libaio --iodepth=8 --bs=4k --numjobs=144 --direct=1 --time_based --runtime=60 --group_reporting --norandommap
# mytest: (g=0): rw=randwrite, bs=(R) 4096B-4096B, (W) 4096B-4096B, (T) 4096B-4096B, ioengine=libaio, iodepth=8
# ...
# fio-3.32
# Starting 144 processes
# Jobs: 144 (f=144): [w(144)][100.0%][w=4985MiB/s][w=1276k IOPS][eta 00m:00s]
# mytest: (groupid=0, jobs=144): err= 0: pid=11104: Fri Feb 23 20:17:50 2024
#   write: IOPS=1274k, BW=4975MiB/s (5216MB/s)(292GiB/60003msec); 0 zone resets
#     slat (nsec): min=1835, max=3538.5k, avg=11318.04, stdev=9667.08
#     clat (usec): min=40, max=118398, avg=891.65, stdev=553.34
#      lat (usec): min=203, max=118408, avg=902.97, stdev=553.18
#     clat percentiles (usec):
#      |  1.00th=[  457],  5.00th=[  537], 10.00th=[  594], 20.00th=[  685],
#      | 30.00th=[  758], 40.00th=[  816], 50.00th=[  857], 60.00th=[  906],
#      | 70.00th=[  963], 80.00th=[ 1037], 90.00th=[ 1156], 95.00th=[ 1303],
#      | 99.00th=[ 1844], 99.50th=[ 2180], 99.90th=[ 3654], 99.95th=[ 5604],
#      | 99.99th=[17957]
#    bw (  MiB/s): min= 3812, max= 5093, per=100.00%, avg=4980.74, stdev= 1.18, samples=17136
#    iops        : min=975992, max=1303914, avg=1275053.98, stdev=301.85, samples=17136
#   lat (usec)   : 50=0.01%, 100=0.01%, 250=0.01%, 500=2.78%, 750=26.25%
#   lat (usec)   : 1000=46.53%
#   lat (msec)   : 2=23.73%, 4=0.62%, 10=0.06%, 20=0.01%, 50=0.01%
#   lat (msec)   : 100=0.01%, 250=0.01%
#   cpu          : usr=2.81%, sys=11.38%, ctx=73836361, majf=0, minf=1361
#   IO depths    : 1=0.1%, 2=0.1%, 4=0.1%, 8=100.0%, 16=0.0%, 32=0.0%, >=64=0.0%
#      submit    : 0=0.0%, 4=100.0%, 8=0.0%, 16=0.0%, 32=0.0%, 64=0.0%, >=64=0.0%
#      complete  : 0=0.0%, 4=100.0%, 8=0.1%, 16=0.0%, 32=0.0%, 64=0.0%, >=64=0.0%
#      issued rwts: total=0,76416964,0,0 short=0,0,0,0 dropped=0,0,0,0
#      latency   : target=0, window=0, percentile=100.00%, depth=8

# Run status group 0 (all jobs):
#   WRITE: bw=4975MiB/s (5216MB/s), 4975MiB/s-4975MiB/s (5216MB/s-5216MB/s), io=292GiB (313GB), run=60003-60003msec

sudo fio --filename=${dev_path} --name mytest --rw=randread --ioengine=libaio --iodepth=8 --bs=4k --numjobs=144 --direct=1 --time_based --runtime=60 --group_reporting --norandommap
# mytest: (g=0): rw=randread, bs=(R) 4096B-4096B, (W) 4096B-4096B, (T) 4096B-4096B, ioengine=libaio, iodepth=8
# ...
# fio-3.32
# Starting 144 processes
# Jobs: 144 (f=144): [r(144)][100.0%][r=5443MiB/s][r=1393k IOPS][eta 00m:00s]
# mytest: (groupid=0, jobs=144): err= 0: pid=12201: Fri Feb 23 20:21:59 2024
#   read: IOPS=1382k, BW=5399MiB/s (5661MB/s)(316GiB/60002msec)
#     slat (nsec): min=1682, max=7736.8k, avg=11881.61, stdev=13202.10
#     clat (usec): min=51, max=22208, avg=819.83, stdev=183.51
#      lat (usec): min=238, max=22215, avg=831.71, stdev=182.93
#     clat percentiles (usec):
#      |  1.00th=[  469],  5.00th=[  545], 10.00th=[  594], 20.00th=[  676],
#      | 30.00th=[  734], 40.00th=[  775], 50.00th=[  816], 60.00th=[  857],
#      | 70.00th=[  898], 80.00th=[  947], 90.00th=[ 1029], 95.00th=[ 1106],
#      | 99.00th=[ 1319], 99.50th=[ 1467], 99.90th=[ 1893], 99.95th=[ 2089],
#      | 99.99th=[ 2671]
#    bw (  MiB/s): min= 5335, max= 5486, per=100.00%, avg=5404.80, stdev= 0.19, samples=17136
#    iops        : min=1365884, max=1404617, avg=1383606.12, stdev=48.00, samples=17136
#   lat (usec)   : 100=0.01%, 250=0.01%, 500=2.24%, 750=31.91%, 1000=52.84%
#   lat (msec)   : 2=12.94%, 4=0.06%, 10=0.01%, 20=0.01%, 50=0.01%
#   cpu          : usr=2.97%, sys=12.62%, ctx=79118249, majf=3, minf=2628
#   IO depths    : 1=0.1%, 2=0.1%, 4=0.1%, 8=100.0%, 16=0.0%, 32=0.0%, >=64=0.0%
#      submit    : 0=0.0%, 4=100.0%, 8=0.0%, 16=0.0%, 32=0.0%, 64=0.0%, >=64=0.0%
#      complete  : 0=0.0%, 4=100.0%, 8=0.1%, 16=0.0%, 32=0.0%, 64=0.0%, >=64=0.0%
#      issued rwts: total=82926788,0,0,0 short=0,0,0,0 dropped=0,0,0,0
#      latency   : target=0, window=0, percentile=100.00%, depth=8

# Run status group 0 (all jobs):
#    READ: bw=5399MiB/s (5661MB/s), 5399MiB/s-5399MiB/s (5661MB/s-5661MB/s), io=316GiB (340GB), run=60002-60002msec


sudo nvme disconnect --nqn ${vd_nqn}
