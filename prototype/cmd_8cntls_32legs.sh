#!/bin/bash

dn_server_ip_A=192.168.0.157
dn_server_ip_B=192.168.0.127
dn_server_ip_C=192.168.0.78
dn_server_ip_D=192.168.0.147
dn_server_ip_E=192.168.0.247
dn_server_ip_F=192.168.0.191
dn_server_ip_G=192.168.0.214
dn_server_ip_H=192.168.0.232
cn_server_ip_A=192.168.0.39
cn_server_ip_B=192.168.0.212
cn_server_ip_C=192.168.0.114
cn_server_ip_D=192.168.0.42
cn_server_ip_E=192.168.0.153
cn_server_ip_F=192.168.0.143
cn_server_ip_G=192.168.0.12
cn_server_ip_H=192.168.0.118

dn_host_name_A="dn_host_A"
dn_host_name_B="dn_host_B"
dn_host_name_C="dn_host_C"
dn_host_name_D="dn_host_D"
dn_host_name_E="dn_host_E"
dn_host_name_F="dn_host_F"
dn_host_name_G="dn_host_G"
dn_host_name_H="dn_host_H"
cn_host_name_A="cn_host_A"
cn_host_name_B="cn_host_B"
cn_host_name_C="cn_host_C"
cn_host_name_D="cn_host_D"
cn_host_name_E="cn_host_E"
cn_host_name_F="cn_host_F"
cn_host_name_G="cn_host_G"
cn_host_name_H="cn_host_H"

leg_cnt=32
cntl_cnt=8

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

leg16_grp0_ld0_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg16_grp0_ld0_pd_path="/dev/disk/by-path/pci-0000:00:18.0-nvme-1"
leg16_grp0_ld0_dn_port_num=1
leg16_grp0_ld0_dn_tr_addr=${dn_server_ip_E}
leg16_grp0_ld0_dn_tr_svc_id=4410

leg16_grp0_ld1_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg16_grp0_ld1_pd_path="/dev/disk/by-path/pci-0000:00:18.0-nvme-1"
leg16_grp0_ld1_dn_port_num=1
leg16_grp0_ld1_dn_tr_addr=${dn_server_ip_F}
leg16_grp0_ld1_dn_tr_svc_id=4410

leg17_grp0_ld0_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg17_grp0_ld0_pd_path="/dev/disk/by-path/pci-0000:00:19.0-nvme-1"
leg17_grp0_ld0_dn_port_num=2
leg17_grp0_ld0_dn_tr_addr=${dn_server_ip_E}
leg17_grp0_ld0_dn_tr_svc_id=4411

leg17_grp0_ld1_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg17_grp0_ld1_pd_path="/dev/disk/by-path/pci-0000:00:19.0-nvme-1"
leg17_grp0_ld1_dn_port_num=2
leg17_grp0_ld1_dn_tr_addr=${dn_server_ip_F}
leg17_grp0_ld1_dn_tr_svc_id=4411

leg18_grp0_ld0_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg18_grp0_ld0_pd_path="/dev/disk/by-path/pci-0000:00:1a.0-nvme-1"
leg18_grp0_ld0_dn_port_num=3
leg18_grp0_ld0_dn_tr_addr=${dn_server_ip_E}
leg18_grp0_ld0_dn_tr_svc_id=4412

leg18_grp0_ld1_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg18_grp0_ld1_pd_path="/dev/disk/by-path/pci-0000:00:1a.0-nvme-1"
leg18_grp0_ld1_dn_port_num=3
leg18_grp0_ld1_dn_tr_addr=${dn_server_ip_F}
leg18_grp0_ld1_dn_tr_svc_id=4412

leg19_grp0_ld0_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg19_grp0_ld0_pd_path="/dev/disk/by-path/pci-0000:00:1b.0-nvme-1"
leg19_grp0_ld0_dn_port_num=4
leg19_grp0_ld0_dn_tr_addr=${dn_server_ip_E}
leg19_grp0_ld0_dn_tr_svc_id=4413

leg19_grp0_ld1_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg19_grp0_ld1_pd_path="/dev/disk/by-path/pci-0000:00:1b.0-nvme-1"
leg19_grp0_ld1_dn_port_num=4
leg19_grp0_ld1_dn_tr_addr=${dn_server_ip_F}
leg19_grp0_ld1_dn_tr_svc_id=4413

leg20_grp0_ld0_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg20_grp0_ld0_pd_path="/dev/disk/by-path/pci-0000:00:1c.0-nvme-1"
leg20_grp0_ld0_dn_port_num=5
leg20_grp0_ld0_dn_tr_addr=${dn_server_ip_E}
leg20_grp0_ld0_dn_tr_svc_id=4414

leg20_grp0_ld1_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg20_grp0_ld1_pd_path="/dev/disk/by-path/pci-0000:00:1c.0-nvme-1"
leg20_grp0_ld1_dn_port_num=5
leg20_grp0_ld1_dn_tr_addr=${dn_server_ip_F}
leg20_grp0_ld1_dn_tr_svc_id=4414

leg21_grp0_ld0_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg21_grp0_ld0_pd_path="/dev/disk/by-path/pci-0000:00:1d.0-nvme-1"
leg21_grp0_ld0_dn_port_num=6
leg21_grp0_ld0_dn_tr_addr=${dn_server_ip_E}
leg21_grp0_ld0_dn_tr_svc_id=4415

leg21_grp0_ld1_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg21_grp0_ld1_pd_path="/dev/disk/by-path/pci-0000:00:1d.0-nvme-1"
leg21_grp0_ld1_dn_port_num=6
leg21_grp0_ld1_dn_tr_addr=${dn_server_ip_F}
leg21_grp0_ld1_dn_tr_svc_id=4415

leg22_grp0_ld0_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg22_grp0_ld0_pd_path="/dev/disk/by-path/pci-0000:00:1e.0-nvme-1"
leg22_grp0_ld0_dn_port_num=7
leg22_grp0_ld0_dn_tr_addr=${dn_server_ip_E}
leg22_grp0_ld0_dn_tr_svc_id=4416

leg22_grp0_ld1_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg22_grp0_ld1_pd_path="/dev/disk/by-path/pci-0000:00:1e.0-nvme-1"
leg22_grp0_ld1_dn_port_num=7
leg22_grp0_ld1_dn_tr_addr=${dn_server_ip_F}
leg22_grp0_ld1_dn_tr_svc_id=4416

leg23_grp0_ld0_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg23_grp0_ld0_pd_path="/dev/disk/by-path/pci-0000:00:1f.0-nvme-1"
leg23_grp0_ld0_dn_port_num=8
leg23_grp0_ld0_dn_tr_addr=${dn_server_ip_E}
leg23_grp0_ld0_dn_tr_svc_id=4417

leg23_grp0_ld1_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg23_grp0_ld1_pd_path="/dev/disk/by-path/pci-0000:00:1f.0-nvme-1"
leg23_grp0_ld1_dn_port_num=8
leg23_grp0_ld1_dn_tr_addr=${dn_server_ip_F}
leg23_grp0_ld1_dn_tr_svc_id=4417

leg24_grp0_ld0_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg24_grp0_ld0_pd_path="/dev/disk/by-path/pci-0000:00:18.0-nvme-1"
leg24_grp0_ld0_dn_port_num=1
leg24_grp0_ld0_dn_tr_addr=${dn_server_ip_G}
leg24_grp0_ld0_dn_tr_svc_id=4410

leg24_grp0_ld1_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg24_grp0_ld1_pd_path="/dev/disk/by-path/pci-0000:00:18.0-nvme-1"
leg24_grp0_ld1_dn_port_num=1
leg24_grp0_ld1_dn_tr_addr=${dn_server_ip_H}
leg24_grp0_ld1_dn_tr_svc_id=4410

leg25_grp0_ld0_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg25_grp0_ld0_pd_path="/dev/disk/by-path/pci-0000:00:19.0-nvme-1"
leg25_grp0_ld0_dn_port_num=2
leg25_grp0_ld0_dn_tr_addr=${dn_server_ip_G}
leg25_grp0_ld0_dn_tr_svc_id=4411

leg25_grp0_ld1_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg25_grp0_ld1_pd_path="/dev/disk/by-path/pci-0000:00:19.0-nvme-1"
leg25_grp0_ld1_dn_port_num=2
leg25_grp0_ld1_dn_tr_addr=${dn_server_ip_H}
leg25_grp0_ld1_dn_tr_svc_id=4411

leg26_grp0_ld0_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg26_grp0_ld0_pd_path="/dev/disk/by-path/pci-0000:00:1a.0-nvme-1"
leg26_grp0_ld0_dn_port_num=3
leg26_grp0_ld0_dn_tr_addr=${dn_server_ip_G}
leg26_grp0_ld0_dn_tr_svc_id=4412

leg26_grp0_ld1_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg26_grp0_ld1_pd_path="/dev/disk/by-path/pci-0000:00:1a.0-nvme-1"
leg26_grp0_ld1_dn_port_num=3
leg26_grp0_ld1_dn_tr_addr=${dn_server_ip_H}
leg26_grp0_ld1_dn_tr_svc_id=4412

leg27_grp0_ld0_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg27_grp0_ld0_pd_path="/dev/disk/by-path/pci-0000:00:1b.0-nvme-1"
leg27_grp0_ld0_dn_port_num=4
leg27_grp0_ld0_dn_tr_addr=${dn_server_ip_G}
leg27_grp0_ld0_dn_tr_svc_id=4413

leg27_grp0_ld1_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg27_grp0_ld1_pd_path="/dev/disk/by-path/pci-0000:00:1b.0-nvme-1"
leg27_grp0_ld1_dn_port_num=4
leg27_grp0_ld1_dn_tr_addr=${dn_server_ip_H}
leg27_grp0_ld1_dn_tr_svc_id=4413

leg28_grp0_ld0_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg28_grp0_ld0_pd_path="/dev/disk/by-path/pci-0000:00:1c.0-nvme-1"
leg28_grp0_ld0_dn_port_num=5
leg28_grp0_ld0_dn_tr_addr=${dn_server_ip_G}
leg28_grp0_ld0_dn_tr_svc_id=4414

leg28_grp0_ld1_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg28_grp0_ld1_pd_path="/dev/disk/by-path/pci-0000:00:1c.0-nvme-1"
leg28_grp0_ld1_dn_port_num=5
leg28_grp0_ld1_dn_tr_addr=${dn_server_ip_H}
leg28_grp0_ld1_dn_tr_svc_id=4414

leg29_grp0_ld0_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg29_grp0_ld0_pd_path="/dev/disk/by-path/pci-0000:00:1d.0-nvme-1"
leg29_grp0_ld0_dn_port_num=6
leg29_grp0_ld0_dn_tr_addr=${dn_server_ip_G}
leg29_grp0_ld0_dn_tr_svc_id=4415

leg29_grp0_ld1_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg29_grp0_ld1_pd_path="/dev/disk/by-path/pci-0000:00:1d.0-nvme-1"
leg29_grp0_ld1_dn_port_num=6
leg29_grp0_ld1_dn_tr_addr=${dn_server_ip_H}
leg29_grp0_ld1_dn_tr_svc_id=4415

leg30_grp0_ld0_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg30_grp0_ld0_pd_path="/dev/disk/by-path/pci-0000:00:1e.0-nvme-1"
leg30_grp0_ld0_dn_port_num=7
leg30_grp0_ld0_dn_tr_addr=${dn_server_ip_G}
leg30_grp0_ld0_dn_tr_svc_id=4416

leg30_grp0_ld1_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg30_grp0_ld1_pd_path="/dev/disk/by-path/pci-0000:00:1e.0-nvme-1"
leg30_grp0_ld1_dn_port_num=7
leg30_grp0_ld1_dn_tr_addr=${dn_server_ip_H}
leg30_grp0_ld1_dn_tr_svc_id=4416

leg31_grp0_ld0_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg31_grp0_ld0_pd_path="/dev/disk/by-path/pci-0000:00:1f.0-nvme-1"
leg31_grp0_ld0_dn_port_num=8
leg31_grp0_ld0_dn_tr_addr=${dn_server_ip_G}
leg31_grp0_ld0_dn_tr_svc_id=4417

leg31_grp0_ld1_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg31_grp0_ld1_pd_path="/dev/disk/by-path/pci-0000:00:1f.0-nvme-1"
leg31_grp0_ld1_dn_port_num=8
leg31_grp0_ld1_dn_tr_addr=${dn_server_ip_H}
leg31_grp0_ld1_dn_tr_svc_id=4417

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

cntl04_cn_mgr_id=${global_id} && global_id=$((global_id+1))
cntl04_cn_host_name=${cn_host_name_E}
cntl04_cn_port_num=1
cntl04_cn_tr_addr=${cn_server_ip_E}
cntl04_cn_tr_svc_id=4420
cntl04_cntlid_min=40000
cntl04_cntlid_max=41999

cntl05_cn_mgr_id=${global_id} && global_id=$((global_id+1))
cntl05_cn_host_name=${cn_host_name_F}
cntl05_cn_port_num=1
cntl05_cn_tr_addr=${cn_server_ip_F}
cntl05_cn_tr_svc_id=4420
cntl05_cntlid_min=42000
cntl05_cntlid_max=43999

cntl06_cn_mgr_id=${global_id} && global_id=$((global_id+1))
cntl06_cn_host_name=${cn_host_name_G}
cntl06_cn_port_num=1
cntl06_cn_tr_addr=${cn_server_ip_G}
cntl06_cn_tr_svc_id=4420
cntl06_cntlid_min=44000
cntl06_cntlid_max=45999

cntl07_cn_mgr_id=${global_id} && global_id=$((global_id+1))
cntl07_cn_host_name=${cn_host_name_H}
cntl07_cn_port_num=1
cntl07_cn_tr_addr=${cn_server_ip_H}
cntl07_cn_tr_svc_id=4420
cntl07_cntlid_min=46000
cntl07_cntlid_max=47999

forward_cn_cnt=7

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

leg16_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg16_grp0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg16_grp0_ld0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg16_grp0_ld1_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))

leg17_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg17_grp0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg17_grp0_ld0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg17_grp0_ld1_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))

leg18_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg18_grp0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg18_grp0_ld0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg18_grp0_ld1_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))

leg19_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg19_grp0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg19_grp0_ld0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg19_grp0_ld1_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))

leg20_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg20_grp0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg20_grp0_ld0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg20_grp0_ld1_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))

leg21_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg21_grp0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg21_grp0_ld0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg21_grp0_ld1_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))

leg22_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg22_grp0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg22_grp0_ld0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg22_grp0_ld1_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))

leg23_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg23_grp0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg23_grp0_ld0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg23_grp0_ld1_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))

leg24_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg24_grp0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg24_grp0_ld0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg24_grp0_ld1_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))

leg25_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg25_grp0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg25_grp0_ld0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg25_grp0_ld1_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))

leg26_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg26_grp0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg26_grp0_ld0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg26_grp0_ld1_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))

leg27_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg27_grp0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg27_grp0_ld0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg27_grp0_ld1_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))

leg28_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg28_grp0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg28_grp0_ld0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg28_grp0_ld1_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))

leg29_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg29_grp0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg29_grp0_ld0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg29_grp0_ld1_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))

leg30_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg30_grp0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg30_grp0_ld0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg30_grp0_ld1_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))

leg31_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg31_grp0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg31_grp0_ld0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg31_grp0_ld1_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))

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
echo "sudo modprobe nvme-tcp && sudo modprobe nvmet && sudo modprobe nvmet-tcp" | ssh ${dn_server_ip_E}
echo "sudo modprobe nvme-tcp && sudo modprobe nvmet && sudo modprobe nvmet-tcp" | ssh ${dn_server_ip_F}
echo "sudo modprobe nvme-tcp && sudo modprobe nvmet && sudo modprobe nvmet-tcp" | ssh ${dn_server_ip_G}
echo "sudo modprobe nvme-tcp && sudo modprobe nvmet && sudo modprobe nvmet-tcp" | ssh ${dn_server_ip_H}
echo "sudo modprobe nvme-tcp && sudo modprobe nvmet && sudo modprobe nvmet-tcp" | ssh ${cn_server_ip_A}
echo "sudo modprobe nvme-tcp && sudo modprobe nvmet && sudo modprobe nvmet-tcp" | ssh ${cn_server_ip_B}
echo "sudo modprobe nvme-tcp && sudo modprobe nvmet && sudo modprobe nvmet-tcp" | ssh ${cn_server_ip_C}
echo "sudo modprobe nvme-tcp && sudo modprobe nvmet && sudo modprobe nvmet-tcp" | ssh ${cn_server_ip_D}
echo "sudo modprobe nvme-tcp && sudo modprobe nvmet && sudo modprobe nvmet-tcp" | ssh ${cn_server_ip_E}
echo "sudo modprobe nvme-tcp && sudo modprobe nvmet && sudo modprobe nvmet-tcp" | ssh ${cn_server_ip_F}
echo "sudo modprobe nvme-tcp && sudo modprobe nvmet && sudo modprobe nvmet-tcp" | ssh ${cn_server_ip_G}
echo "sudo modprobe nvme-tcp && sudo modprobe nvmet && sudo modprobe nvmet-tcp" | ssh ${cn_server_ip_H}

scp *.sh ${dn_server_ip_A}:/tmp/ && scp *.sh ${dn_server_ip_B}:/tmp/ && scp *.sh ${dn_server_ip_C}:/tmp/ && scp *.sh ${dn_server_ip_D}:/tmp/ && scp *.sh ${cn_server_ip_A}:/tmp/ && scp *.sh ${cn_server_ip_B}:/tmp/ && scp *.sh ${cn_server_ip_C}:/tmp/ && scp *.sh ${cn_server_ip_D}:/tmp/
scp *.sh ${dn_server_ip_E}:/tmp/ && scp *.sh ${dn_server_ip_F}:/tmp/ && scp *.sh ${dn_server_ip_G}:/tmp/ && scp *.sh ${dn_server_ip_H}:/tmp/ && scp *.sh ${cn_server_ip_E}:/tmp/ && scp *.sh ${cn_server_ip_F}:/tmp/ && scp *.sh ${cn_server_ip_G}:/tmp/ && scp *.sh ${cn_server_ip_H}:/tmp/

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
# prepare leg16 grp0 ld0
echo "sudo /tmp/dn_prepare.sh ${leg16_grp0_ld0_dn_port_num} ${leg16_grp0_ld0_dn_tr_addr} ${leg16_grp0_ld0_dn_tr_svc_id}" | ssh ${dn_server_ip_E}
# prepare leg16 grp0 ld1
echo "sudo /tmp/dn_prepare.sh ${leg16_grp0_ld1_dn_port_num} ${leg16_grp0_ld1_dn_tr_addr} ${leg16_grp0_ld1_dn_tr_svc_id}" | ssh ${dn_server_ip_F}
# prepare leg17 grp0 ld0
echo "sudo /tmp/dn_prepare.sh ${leg17_grp0_ld0_dn_port_num} ${leg17_grp0_ld0_dn_tr_addr} ${leg17_grp0_ld0_dn_tr_svc_id}" | ssh ${dn_server_ip_E}
# prepare leg17 grp0 ld1
echo "sudo /tmp/dn_prepare.sh ${leg17_grp0_ld1_dn_port_num} ${leg17_grp0_ld1_dn_tr_addr} ${leg17_grp0_ld1_dn_tr_svc_id}" | ssh ${dn_server_ip_F}
# prepare leg18 grp0 ld0
echo "sudo /tmp/dn_prepare.sh ${leg18_grp0_ld0_dn_port_num} ${leg18_grp0_ld0_dn_tr_addr} ${leg18_grp0_ld0_dn_tr_svc_id}" | ssh ${dn_server_ip_E}
# prepare leg18 grp0 ld1
echo "sudo /tmp/dn_prepare.sh ${leg18_grp0_ld1_dn_port_num} ${leg18_grp0_ld1_dn_tr_addr} ${leg18_grp0_ld1_dn_tr_svc_id}" | ssh ${dn_server_ip_F}
# prepare leg19 grp0 ld0
echo "sudo /tmp/dn_prepare.sh ${leg19_grp0_ld0_dn_port_num} ${leg19_grp0_ld0_dn_tr_addr} ${leg19_grp0_ld0_dn_tr_svc_id}" | ssh ${dn_server_ip_E}
# prepare leg19 grp0 ld1
echo "sudo /tmp/dn_prepare.sh ${leg19_grp0_ld1_dn_port_num} ${leg19_grp0_ld1_dn_tr_addr} ${leg19_grp0_ld1_dn_tr_svc_id}" | ssh ${dn_server_ip_F}
# prepare leg20 grp0 ld0
echo "sudo /tmp/dn_prepare.sh ${leg20_grp0_ld0_dn_port_num} ${leg20_grp0_ld0_dn_tr_addr} ${leg20_grp0_ld0_dn_tr_svc_id}" | ssh ${dn_server_ip_E}
# prepare leg20 grp0 ld1
echo "sudo /tmp/dn_prepare.sh ${leg20_grp0_ld1_dn_port_num} ${leg20_grp0_ld1_dn_tr_addr} ${leg20_grp0_ld1_dn_tr_svc_id}" | ssh ${dn_server_ip_F}
# prepare leg21 grp0 ld0
echo "sudo /tmp/dn_prepare.sh ${leg21_grp0_ld0_dn_port_num} ${leg21_grp0_ld0_dn_tr_addr} ${leg21_grp0_ld0_dn_tr_svc_id}" | ssh ${dn_server_ip_E}
# prepare leg21 grp0 ld1
echo "sudo /tmp/dn_prepare.sh ${leg21_grp0_ld1_dn_port_num} ${leg21_grp0_ld1_dn_tr_addr} ${leg21_grp0_ld1_dn_tr_svc_id}" | ssh ${dn_server_ip_F}
# prepare leg22 grp0 ld0
echo "sudo /tmp/dn_prepare.sh ${leg22_grp0_ld0_dn_port_num} ${leg22_grp0_ld0_dn_tr_addr} ${leg22_grp0_ld0_dn_tr_svc_id}" | ssh ${dn_server_ip_E}
# prepare leg22 grp0 ld1
echo "sudo /tmp/dn_prepare.sh ${leg22_grp0_ld1_dn_port_num} ${leg22_grp0_ld1_dn_tr_addr} ${leg22_grp0_ld1_dn_tr_svc_id}" | ssh ${dn_server_ip_F}
# prepare leg23 grp0 ld0
echo "sudo /tmp/dn_prepare.sh ${leg23_grp0_ld0_dn_port_num} ${leg23_grp0_ld0_dn_tr_addr} ${leg23_grp0_ld0_dn_tr_svc_id}" | ssh ${dn_server_ip_E}
# prepare leg23 grp0 ld1
echo "sudo /tmp/dn_prepare.sh ${leg23_grp0_ld1_dn_port_num} ${leg23_grp0_ld1_dn_tr_addr} ${leg23_grp0_ld1_dn_tr_svc_id}" | ssh ${dn_server_ip_F}
# prepare leg24 grp0 ld0
echo "sudo /tmp/dn_prepare.sh ${leg24_grp0_ld0_dn_port_num} ${leg24_grp0_ld0_dn_tr_addr} ${leg24_grp0_ld0_dn_tr_svc_id}" | ssh ${dn_server_ip_G}
# prepare leg24 grp0 ld1
echo "sudo /tmp/dn_prepare.sh ${leg24_grp0_ld1_dn_port_num} ${leg24_grp0_ld1_dn_tr_addr} ${leg24_grp0_ld1_dn_tr_svc_id}" | ssh ${dn_server_ip_H}
# prepare leg25 grp0 ld0
echo "sudo /tmp/dn_prepare.sh ${leg25_grp0_ld0_dn_port_num} ${leg25_grp0_ld0_dn_tr_addr} ${leg25_grp0_ld0_dn_tr_svc_id}" | ssh ${dn_server_ip_G}
# prepare leg25 grp0 ld1
echo "sudo /tmp/dn_prepare.sh ${leg25_grp0_ld1_dn_port_num} ${leg25_grp0_ld1_dn_tr_addr} ${leg25_grp0_ld1_dn_tr_svc_id}" | ssh ${dn_server_ip_H}
# prepare leg26 grp0 ld0
echo "sudo /tmp/dn_prepare.sh ${leg26_grp0_ld0_dn_port_num} ${leg26_grp0_ld0_dn_tr_addr} ${leg26_grp0_ld0_dn_tr_svc_id}" | ssh ${dn_server_ip_G}
# prepare leg26 grp0 ld1
echo "sudo /tmp/dn_prepare.sh ${leg26_grp0_ld1_dn_port_num} ${leg26_grp0_ld1_dn_tr_addr} ${leg26_grp0_ld1_dn_tr_svc_id}" | ssh ${dn_server_ip_H}
# prepare leg27 grp0 ld0
echo "sudo /tmp/dn_prepare.sh ${leg27_grp0_ld0_dn_port_num} ${leg27_grp0_ld0_dn_tr_addr} ${leg27_grp0_ld0_dn_tr_svc_id}" | ssh ${dn_server_ip_G}
# prepare leg27 grp0 ld1
echo "sudo /tmp/dn_prepare.sh ${leg27_grp0_ld1_dn_port_num} ${leg27_grp0_ld1_dn_tr_addr} ${leg27_grp0_ld1_dn_tr_svc_id}" | ssh ${dn_server_ip_H}
# prepare leg28 grp0 ld0
echo "sudo /tmp/dn_prepare.sh ${leg28_grp0_ld0_dn_port_num} ${leg28_grp0_ld0_dn_tr_addr} ${leg28_grp0_ld0_dn_tr_svc_id}" | ssh ${dn_server_ip_G}
# prepare leg28 grp0 ld1
echo "sudo /tmp/dn_prepare.sh ${leg28_grp0_ld1_dn_port_num} ${leg28_grp0_ld1_dn_tr_addr} ${leg28_grp0_ld1_dn_tr_svc_id}" | ssh ${dn_server_ip_H}
# prepare leg29 grp0 ld0
echo "sudo /tmp/dn_prepare.sh ${leg29_grp0_ld0_dn_port_num} ${leg29_grp0_ld0_dn_tr_addr} ${leg29_grp0_ld0_dn_tr_svc_id}" | ssh ${dn_server_ip_G}
# prepare leg29 grp0 ld1
echo "sudo /tmp/dn_prepare.sh ${leg29_grp0_ld1_dn_port_num} ${leg29_grp0_ld1_dn_tr_addr} ${leg29_grp0_ld1_dn_tr_svc_id}" | ssh ${dn_server_ip_H}
# prepare leg30 grp0 ld0
echo "sudo /tmp/dn_prepare.sh ${leg30_grp0_ld0_dn_port_num} ${leg30_grp0_ld0_dn_tr_addr} ${leg30_grp0_ld0_dn_tr_svc_id}" | ssh ${dn_server_ip_G}
# prepare leg30 grp0 ld1
echo "sudo /tmp/dn_prepare.sh ${leg30_grp0_ld1_dn_port_num} ${leg30_grp0_ld1_dn_tr_addr} ${leg30_grp0_ld1_dn_tr_svc_id}" | ssh ${dn_server_ip_H}
# prepare leg31 grp0 ld0
echo "sudo /tmp/dn_prepare.sh ${leg31_grp0_ld0_dn_port_num} ${leg31_grp0_ld0_dn_tr_addr} ${leg31_grp0_ld0_dn_tr_svc_id}" | ssh ${dn_server_ip_G}
# prepare leg31 grp0 ld1
echo "sudo /tmp/dn_prepare.sh ${leg31_grp0_ld1_dn_port_num} ${leg31_grp0_ld1_dn_tr_addr} ${leg31_grp0_ld1_dn_tr_svc_id}" | ssh ${dn_server_ip_H}

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
# cleanup leg16 grp0 ld0
echo "sudo /tmp/dn_cleanup.sh ${leg16_grp0_ld0_dn_port_num}" | ssh ${dn_server_ip_E}
# cleanup leg16 grp0 ld1
echo "sudo /tmp/dn_cleanup.sh ${leg16_grp0_ld1_dn_port_num}" | ssh ${dn_server_ip_F}
# cleanup leg17 grp0 ld0
echo "sudo /tmp/dn_cleanup.sh ${leg17_grp0_ld0_dn_port_num}" | ssh ${dn_server_ip_E}
# cleanup leg17 grp0 ld1
echo "sudo /tmp/dn_cleanup.sh ${leg17_grp0_ld1_dn_port_num}" | ssh ${dn_server_ip_F}
# cleanup leg18 grp0 ld0
echo "sudo /tmp/dn_cleanup.sh ${leg18_grp0_ld0_dn_port_num}" | ssh ${dn_server_ip_E}
# cleanup leg18 grp0 ld1
echo "sudo /tmp/dn_cleanup.sh ${leg18_grp0_ld1_dn_port_num}" | ssh ${dn_server_ip_F}
# cleanup leg19 grp0 ld0
echo "sudo /tmp/dn_cleanup.sh ${leg19_grp0_ld0_dn_port_num}" | ssh ${dn_server_ip_E}
# cleanup leg19 grp0 ld1
echo "sudo /tmp/dn_cleanup.sh ${leg19_grp0_ld1_dn_port_num}" | ssh ${dn_server_ip_F}
# cleanup leg20 grp0 ld0
echo "sudo /tmp/dn_cleanup.sh ${leg20_grp0_ld0_dn_port_num}" | ssh ${dn_server_ip_E}
# cleanup leg20 grp0 ld1
echo "sudo /tmp/dn_cleanup.sh ${leg20_grp0_ld1_dn_port_num}" | ssh ${dn_server_ip_F}
# cleanup leg21 grp0 ld0
echo "sudo /tmp/dn_cleanup.sh ${leg21_grp0_ld0_dn_port_num}" | ssh ${dn_server_ip_E}
# cleanup leg21 grp0 ld1
echo "sudo /tmp/dn_cleanup.sh ${leg21_grp0_ld1_dn_port_num}" | ssh ${dn_server_ip_F}
# cleanup leg22 grp0 ld0
echo "sudo /tmp/dn_cleanup.sh ${leg22_grp0_ld0_dn_port_num}" | ssh ${dn_server_ip_E}
# cleanup leg22 grp0 ld1
echo "sudo /tmp/dn_cleanup.sh ${leg22_grp0_ld1_dn_port_num}" | ssh ${dn_server_ip_F}
# cleanup leg23 grp0 ld0
echo "sudo /tmp/dn_cleanup.sh ${leg23_grp0_ld0_dn_port_num}" | ssh ${dn_server_ip_E}
# cleanup leg23 grp0 ld1
echo "sudo /tmp/dn_cleanup.sh ${leg23_grp0_ld1_dn_port_num}" | ssh ${dn_server_ip_F}
# cleanup leg24 grp0 ld0
echo "sudo /tmp/dn_cleanup.sh ${leg24_grp0_ld0_dn_port_num}" | ssh ${dn_server_ip_G}
# cleanup leg24 grp0 ld1
echo "sudo /tmp/dn_cleanup.sh ${leg24_grp0_ld1_dn_port_num}" | ssh ${dn_server_ip_H}
# cleanup leg25 grp0 ld0
echo "sudo /tmp/dn_cleanup.sh ${leg25_grp0_ld0_dn_port_num}" | ssh ${dn_server_ip_G}
# cleanup leg25 grp0 ld1
echo "sudo /tmp/dn_cleanup.sh ${leg25_grp0_ld1_dn_port_num}" | ssh ${dn_server_ip_H}
# cleanup leg26 grp0 ld0
echo "sudo /tmp/dn_cleanup.sh ${leg26_grp0_ld0_dn_port_num}" | ssh ${dn_server_ip_G}
# cleanup leg26 grp0 ld1
echo "sudo /tmp/dn_cleanup.sh ${leg26_grp0_ld1_dn_port_num}" | ssh ${dn_server_ip_H}
# cleanup leg27 grp0 ld0
echo "sudo /tmp/dn_cleanup.sh ${leg27_grp0_ld0_dn_port_num}" | ssh ${dn_server_ip_G}
# cleanup leg27 grp0 ld1
echo "sudo /tmp/dn_cleanup.sh ${leg27_grp0_ld1_dn_port_num}" | ssh ${dn_server_ip_H}
# cleanup leg28 grp0 ld0
echo "sudo /tmp/dn_cleanup.sh ${leg28_grp0_ld0_dn_port_num}" | ssh ${dn_server_ip_G}
# cleanup leg28 grp0 ld1
echo "sudo /tmp/dn_cleanup.sh ${leg28_grp0_ld1_dn_port_num}" | ssh ${dn_server_ip_H}
# cleanup leg29 grp0 ld0
echo "sudo /tmp/dn_cleanup.sh ${leg29_grp0_ld0_dn_port_num}" | ssh ${dn_server_ip_G}
# cleanup leg29 grp0 ld1
echo "sudo /tmp/dn_cleanup.sh ${leg29_grp0_ld1_dn_port_num}" | ssh ${dn_server_ip_H}
# cleanup leg30 grp0 ld0
echo "sudo /tmp/dn_cleanup.sh ${leg30_grp0_ld0_dn_port_num}" | ssh ${dn_server_ip_G}
# cleanup leg30 grp0 ld1
echo "sudo /tmp/dn_cleanup.sh ${leg30_grp0_ld1_dn_port_num}" | ssh ${dn_server_ip_H}
# cleanup leg31 grp0 ld0
echo "sudo /tmp/dn_cleanup.sh ${leg31_grp0_ld0_dn_port_num}" | ssh ${dn_server_ip_G}
# cleanup leg31 grp0 ld1
echo "sudo /tmp/dn_cleanup.sh ${leg31_grp0_ld1_dn_port_num}" | ssh ${dn_server_ip_H}

# prepare cntl00
echo "sudo /tmp/cn_prepare.sh ${cntl00_cn_port_num} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id}" | ssh ${cn_server_ip_A}
# prepare cntl01
echo "sudo /tmp/cn_prepare.sh ${cntl01_cn_port_num} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id}" | ssh ${cn_server_ip_B}
# prepare cntl02
echo "sudo /tmp/cn_prepare.sh ${cntl02_cn_port_num} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id}" | ssh ${cn_server_ip_C}
# prepare cntl03
echo "sudo /tmp/cn_prepare.sh ${cntl03_cn_port_num} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id}" | ssh ${cn_server_ip_D}
# prepare cntl04
echo "sudo /tmp/cn_prepare.sh ${cntl04_cn_port_num} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id}" | ssh ${cn_server_ip_E}
# prepare cntl05
echo "sudo /tmp/cn_prepare.sh ${cntl05_cn_port_num} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id}" | ssh ${cn_server_ip_F}
# prepare cntl06
echo "sudo /tmp/cn_prepare.sh ${cntl06_cn_port_num} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id}" | ssh ${cn_server_ip_G}
# prepare cntl07
echo "sudo /tmp/cn_prepare.sh ${cntl07_cn_port_num} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id}" | ssh ${cn_server_ip_H}

# cleanup cntl00
echo "sudo /tmp/cn_cleanup.sh ${cntl00_cn_port_num}" | ssh ${cn_server_ip_A}
# cleanup cntl01
echo "sudo /tmp/cn_cleanup.sh ${cntl01_cn_port_num}" | ssh ${cn_server_ip_B}
# cleanup cntl02
echo "sudo /tmp/cn_cleanup.sh ${cntl02_cn_port_num}" | ssh ${cn_server_ip_C}
# cleanup cntl03
echo "sudo /tmp/cn_cleanup.sh ${cntl03_cn_port_num}" | ssh ${cn_server_ip_D}
# cleanup cntl04
echo "sudo /tmp/cn_cleanup.sh ${cntl04_cn_port_num}" | ssh ${cn_server_ip_E}
# cleanup cntl05
echo "sudo /tmp/cn_cleanup.sh ${cntl05_cn_port_num}" | ssh ${cn_server_ip_F}
# cleanup cntl06
echo "sudo /tmp/cn_cleanup.sh ${cntl06_cn_port_num}" | ssh ${cn_server_ip_G}
# cleanup cntl07
echo "sudo /tmp/cn_cleanup.sh ${cntl07_cn_port_num}" | ssh ${cn_server_ip_H}

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
# create leg16 grp0 ld0
echo "sudo /tmp/dn_ld_create.sh ${leg16_grp0_ld0_dn_mgr_id} ${leg16_grp0_ld0_dn_port_num} ${leg16_grp0_ld0_pd_path} ${vd_id} ${leg16_grp0_ld0_id} ${ld_start_mb} ${ld_size_mb} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name}" | ssh ${dn_server_ip_E}
# create leg16 grp0 ld1
echo "sudo /tmp/dn_ld_create.sh ${leg16_grp0_ld1_dn_mgr_id} ${leg16_grp0_ld1_dn_port_num} ${leg16_grp0_ld1_pd_path} ${vd_id} ${leg16_grp0_ld1_id} ${ld_start_mb} ${ld_size_mb} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name}" | ssh ${dn_server_ip_F}
# create leg17 grp0 ld0
echo "sudo /tmp/dn_ld_create.sh ${leg17_grp0_ld0_dn_mgr_id} ${leg17_grp0_ld0_dn_port_num} ${leg17_grp0_ld0_pd_path} ${vd_id} ${leg17_grp0_ld0_id} ${ld_start_mb} ${ld_size_mb} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name}" | ssh ${dn_server_ip_E}
# create leg17 grp0 ld1
echo "sudo /tmp/dn_ld_create.sh ${leg17_grp0_ld1_dn_mgr_id} ${leg17_grp0_ld1_dn_port_num} ${leg17_grp0_ld1_pd_path} ${vd_id} ${leg17_grp0_ld1_id} ${ld_start_mb} ${ld_size_mb} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name}" | ssh ${dn_server_ip_F}
# create leg18 grp0 ld0
echo "sudo /tmp/dn_ld_create.sh ${leg18_grp0_ld0_dn_mgr_id} ${leg18_grp0_ld0_dn_port_num} ${leg18_grp0_ld0_pd_path} ${vd_id} ${leg18_grp0_ld0_id} ${ld_start_mb} ${ld_size_mb} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name}" | ssh ${dn_server_ip_E}
# create leg18 grp0 ld1
echo "sudo /tmp/dn_ld_create.sh ${leg18_grp0_ld1_dn_mgr_id} ${leg18_grp0_ld1_dn_port_num} ${leg18_grp0_ld1_pd_path} ${vd_id} ${leg18_grp0_ld1_id} ${ld_start_mb} ${ld_size_mb} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name}" | ssh ${dn_server_ip_F}
# create leg19 grp0 ld0
echo "sudo /tmp/dn_ld_create.sh ${leg19_grp0_ld0_dn_mgr_id} ${leg19_grp0_ld0_dn_port_num} ${leg19_grp0_ld0_pd_path} ${vd_id} ${leg19_grp0_ld0_id} ${ld_start_mb} ${ld_size_mb} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name}" | ssh ${dn_server_ip_E}
# create leg19 grp0 ld1
echo "sudo /tmp/dn_ld_create.sh ${leg19_grp0_ld1_dn_mgr_id} ${leg19_grp0_ld1_dn_port_num} ${leg19_grp0_ld1_pd_path} ${vd_id} ${leg19_grp0_ld1_id} ${ld_start_mb} ${ld_size_mb} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name}" | ssh ${dn_server_ip_F}
# create leg20 grp0 ld0
echo "sudo /tmp/dn_ld_create.sh ${leg20_grp0_ld0_dn_mgr_id} ${leg20_grp0_ld0_dn_port_num} ${leg20_grp0_ld0_pd_path} ${vd_id} ${leg20_grp0_ld0_id} ${ld_start_mb} ${ld_size_mb} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name}" | ssh ${dn_server_ip_E}
# create leg20 grp0 ld1
echo "sudo /tmp/dn_ld_create.sh ${leg20_grp0_ld1_dn_mgr_id} ${leg20_grp0_ld1_dn_port_num} ${leg20_grp0_ld1_pd_path} ${vd_id} ${leg20_grp0_ld1_id} ${ld_start_mb} ${ld_size_mb} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name}" | ssh ${dn_server_ip_F}
# create leg21 grp0 ld0
echo "sudo /tmp/dn_ld_create.sh ${leg21_grp0_ld0_dn_mgr_id} ${leg21_grp0_ld0_dn_port_num} ${leg21_grp0_ld0_pd_path} ${vd_id} ${leg21_grp0_ld0_id} ${ld_start_mb} ${ld_size_mb} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name}" | ssh ${dn_server_ip_E}
# create leg21 grp0 ld1
echo "sudo /tmp/dn_ld_create.sh ${leg21_grp0_ld1_dn_mgr_id} ${leg21_grp0_ld1_dn_port_num} ${leg21_grp0_ld1_pd_path} ${vd_id} ${leg21_grp0_ld1_id} ${ld_start_mb} ${ld_size_mb} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name}" | ssh ${dn_server_ip_F}
# create leg22 grp0 ld0
echo "sudo /tmp/dn_ld_create.sh ${leg22_grp0_ld0_dn_mgr_id} ${leg22_grp0_ld0_dn_port_num} ${leg22_grp0_ld0_pd_path} ${vd_id} ${leg22_grp0_ld0_id} ${ld_start_mb} ${ld_size_mb} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name}" | ssh ${dn_server_ip_E}
# create leg22 grp0 ld1
echo "sudo /tmp/dn_ld_create.sh ${leg22_grp0_ld1_dn_mgr_id} ${leg22_grp0_ld1_dn_port_num} ${leg22_grp0_ld1_pd_path} ${vd_id} ${leg22_grp0_ld1_id} ${ld_start_mb} ${ld_size_mb} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name}" | ssh ${dn_server_ip_F}
# create leg23 grp0 ld0
echo "sudo /tmp/dn_ld_create.sh ${leg23_grp0_ld0_dn_mgr_id} ${leg23_grp0_ld0_dn_port_num} ${leg23_grp0_ld0_pd_path} ${vd_id} ${leg23_grp0_ld0_id} ${ld_start_mb} ${ld_size_mb} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name}" | ssh ${dn_server_ip_E}
# create leg23 grp0 ld1
echo "sudo /tmp/dn_ld_create.sh ${leg23_grp0_ld1_dn_mgr_id} ${leg23_grp0_ld1_dn_port_num} ${leg23_grp0_ld1_pd_path} ${vd_id} ${leg23_grp0_ld1_id} ${ld_start_mb} ${ld_size_mb} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name}" | ssh ${dn_server_ip_F}
# create leg24 grp0 ld0
echo "sudo /tmp/dn_ld_create.sh ${leg24_grp0_ld0_dn_mgr_id} ${leg24_grp0_ld0_dn_port_num} ${leg24_grp0_ld0_pd_path} ${vd_id} ${leg24_grp0_ld0_id} ${ld_start_mb} ${ld_size_mb} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name}" | ssh ${dn_server_ip_G}
# create leg24 grp0 ld1
echo "sudo /tmp/dn_ld_create.sh ${leg24_grp0_ld1_dn_mgr_id} ${leg24_grp0_ld1_dn_port_num} ${leg24_grp0_ld1_pd_path} ${vd_id} ${leg24_grp0_ld1_id} ${ld_start_mb} ${ld_size_mb} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name}" | ssh ${dn_server_ip_H}
# create leg25 grp0 ld0
echo "sudo /tmp/dn_ld_create.sh ${leg25_grp0_ld0_dn_mgr_id} ${leg25_grp0_ld0_dn_port_num} ${leg25_grp0_ld0_pd_path} ${vd_id} ${leg25_grp0_ld0_id} ${ld_start_mb} ${ld_size_mb} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name}" | ssh ${dn_server_ip_G}
# create leg25 grp0 ld1
echo "sudo /tmp/dn_ld_create.sh ${leg25_grp0_ld1_dn_mgr_id} ${leg25_grp0_ld1_dn_port_num} ${leg25_grp0_ld1_pd_path} ${vd_id} ${leg25_grp0_ld1_id} ${ld_start_mb} ${ld_size_mb} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name}" | ssh ${dn_server_ip_H}
# create leg26 grp0 ld0
echo "sudo /tmp/dn_ld_create.sh ${leg26_grp0_ld0_dn_mgr_id} ${leg26_grp0_ld0_dn_port_num} ${leg26_grp0_ld0_pd_path} ${vd_id} ${leg26_grp0_ld0_id} ${ld_start_mb} ${ld_size_mb} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name}" | ssh ${dn_server_ip_G}
# create leg26 grp0 ld1
echo "sudo /tmp/dn_ld_create.sh ${leg26_grp0_ld1_dn_mgr_id} ${leg26_grp0_ld1_dn_port_num} ${leg26_grp0_ld1_pd_path} ${vd_id} ${leg26_grp0_ld1_id} ${ld_start_mb} ${ld_size_mb} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name}" | ssh ${dn_server_ip_H}
# create leg27 grp0 ld0
echo "sudo /tmp/dn_ld_create.sh ${leg27_grp0_ld0_dn_mgr_id} ${leg27_grp0_ld0_dn_port_num} ${leg27_grp0_ld0_pd_path} ${vd_id} ${leg27_grp0_ld0_id} ${ld_start_mb} ${ld_size_mb} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name}" | ssh ${dn_server_ip_G}
# create leg27 grp0 ld1
echo "sudo /tmp/dn_ld_create.sh ${leg27_grp0_ld1_dn_mgr_id} ${leg27_grp0_ld1_dn_port_num} ${leg27_grp0_ld1_pd_path} ${vd_id} ${leg27_grp0_ld1_id} ${ld_start_mb} ${ld_size_mb} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name}" | ssh ${dn_server_ip_H}
# create leg28 grp0 ld0
echo "sudo /tmp/dn_ld_create.sh ${leg28_grp0_ld0_dn_mgr_id} ${leg28_grp0_ld0_dn_port_num} ${leg28_grp0_ld0_pd_path} ${vd_id} ${leg28_grp0_ld0_id} ${ld_start_mb} ${ld_size_mb} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${dn_server_ip_G}
# create leg28 grp0 ld1
echo "sudo /tmp/dn_ld_create.sh ${leg28_grp0_ld1_dn_mgr_id} ${leg28_grp0_ld1_dn_port_num} ${leg28_grp0_ld1_pd_path} ${vd_id} ${leg28_grp0_ld1_id} ${ld_start_mb} ${ld_size_mb} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${dn_server_ip_H}
# create leg29 grp0 ld0
echo "sudo /tmp/dn_ld_create.sh ${leg29_grp0_ld0_dn_mgr_id} ${leg29_grp0_ld0_dn_port_num} ${leg29_grp0_ld0_pd_path} ${vd_id} ${leg29_grp0_ld0_id} ${ld_start_mb} ${ld_size_mb} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${dn_server_ip_G}
# create leg29 grp0 ld1
echo "sudo /tmp/dn_ld_create.sh ${leg29_grp0_ld1_dn_mgr_id} ${leg29_grp0_ld1_dn_port_num} ${leg29_grp0_ld1_pd_path} ${vd_id} ${leg29_grp0_ld1_id} ${ld_start_mb} ${ld_size_mb} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${dn_server_ip_H}
# create leg30 grp0 ld0
echo "sudo /tmp/dn_ld_create.sh ${leg30_grp0_ld0_dn_mgr_id} ${leg30_grp0_ld0_dn_port_num} ${leg30_grp0_ld0_pd_path} ${vd_id} ${leg30_grp0_ld0_id} ${ld_start_mb} ${ld_size_mb} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${dn_server_ip_G}
# create leg30 grp0 ld1
echo "sudo /tmp/dn_ld_create.sh ${leg30_grp0_ld1_dn_mgr_id} ${leg30_grp0_ld1_dn_port_num} ${leg30_grp0_ld1_pd_path} ${vd_id} ${leg30_grp0_ld1_id} ${ld_start_mb} ${ld_size_mb} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${dn_server_ip_H}
# create leg31 grp0 ld0
echo "sudo /tmp/dn_ld_create.sh ${leg31_grp0_ld0_dn_mgr_id} ${leg31_grp0_ld0_dn_port_num} ${leg31_grp0_ld0_pd_path} ${vd_id} ${leg31_grp0_ld0_id} ${ld_start_mb} ${ld_size_mb} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${dn_server_ip_G}
# create leg31 grp0 ld1
echo "sudo /tmp/dn_ld_create.sh ${leg31_grp0_ld1_dn_mgr_id} ${leg31_grp0_ld1_dn_port_num} ${leg31_grp0_ld1_pd_path} ${vd_id} ${leg31_grp0_ld1_id} ${ld_start_mb} ${ld_size_mb} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${dn_server_ip_H}

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
# delete leg16 grp0 ld0
echo "sudo /tmp/dn_ld_delete.sh ${leg16_grp0_ld0_dn_mgr_id} ${leg16_grp0_ld0_dn_port_num} ${vd_id} ${leg16_grp0_ld0_id} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name}" | ssh ${dn_server_ip_E}
# delete leg16 grp0 ld1
echo "sudo /tmp/dn_ld_delete.sh ${leg16_grp0_ld1_dn_mgr_id} ${leg16_grp0_ld1_dn_port_num} ${vd_id} ${leg16_grp0_ld1_id} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name}" | ssh ${dn_server_ip_F}
# delete leg17 grp0 ld0
echo "sudo /tmp/dn_ld_delete.sh ${leg17_grp0_ld0_dn_mgr_id} ${leg17_grp0_ld0_dn_port_num} ${vd_id} ${leg17_grp0_ld0_id} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name}" | ssh ${dn_server_ip_E}
# delete leg17 grp0 ld1
echo "sudo /tmp/dn_ld_delete.sh ${leg17_grp0_ld1_dn_mgr_id} ${leg17_grp0_ld1_dn_port_num} ${vd_id} ${leg17_grp0_ld1_id} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name}" | ssh ${dn_server_ip_F}
# delete leg18 grp0 ld0
echo "sudo /tmp/dn_ld_delete.sh ${leg18_grp0_ld0_dn_mgr_id} ${leg18_grp0_ld0_dn_port_num} ${vd_id} ${leg18_grp0_ld0_id} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name}" | ssh ${dn_server_ip_E}
# delete leg18 grp0 ld1
echo "sudo /tmp/dn_ld_delete.sh ${leg18_grp0_ld1_dn_mgr_id} ${leg18_grp0_ld1_dn_port_num} ${vd_id} ${leg18_grp0_ld1_id} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name}" | ssh ${dn_server_ip_F}
# delete leg19 grp0 ld0
echo "sudo /tmp/dn_ld_delete.sh ${leg19_grp0_ld0_dn_mgr_id} ${leg19_grp0_ld0_dn_port_num} ${vd_id} ${leg19_grp0_ld0_id} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name}" | ssh ${dn_server_ip_E}
# delete leg19 grp0 ld1
echo "sudo /tmp/dn_ld_delete.sh ${leg19_grp0_ld1_dn_mgr_id} ${leg19_grp0_ld1_dn_port_num} ${vd_id} ${leg19_grp0_ld1_id} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name}" | ssh ${dn_server_ip_F}
# delete leg20 grp0 ld0
echo "sudo /tmp/dn_ld_delete.sh ${leg20_grp0_ld0_dn_mgr_id} ${leg20_grp0_ld0_dn_port_num} ${vd_id} ${leg20_grp0_ld0_id} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name}" | ssh ${dn_server_ip_E}
# delete leg20 grp0 ld1
echo "sudo /tmp/dn_ld_delete.sh ${leg20_grp0_ld1_dn_mgr_id} ${leg20_grp0_ld1_dn_port_num} ${vd_id} ${leg20_grp0_ld1_id} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name}" | ssh ${dn_server_ip_F}
# delete leg21 grp0 ld0
echo "sudo /tmp/dn_ld_delete.sh ${leg21_grp0_ld0_dn_mgr_id} ${leg21_grp0_ld0_dn_port_num} ${vd_id} ${leg21_grp0_ld0_id} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name}" | ssh ${dn_server_ip_E}
# delete leg21 grp0 ld1
echo "sudo /tmp/dn_ld_delete.sh ${leg21_grp0_ld1_dn_mgr_id} ${leg21_grp0_ld1_dn_port_num} ${vd_id} ${leg21_grp0_ld1_id} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name}" | ssh ${dn_server_ip_F}
# delete leg22 grp0 ld0
echo "sudo /tmp/dn_ld_delete.sh ${leg22_grp0_ld0_dn_mgr_id} ${leg22_grp0_ld0_dn_port_num} ${vd_id} ${leg22_grp0_ld0_id} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name}" | ssh ${dn_server_ip_E}
# delete leg22 grp0 ld1
echo "sudo /tmp/dn_ld_delete.sh ${leg22_grp0_ld1_dn_mgr_id} ${leg22_grp0_ld1_dn_port_num} ${vd_id} ${leg22_grp0_ld1_id} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name}" | ssh ${dn_server_ip_F}
# delete leg23 grp0 ld0
echo "sudo /tmp/dn_ld_delete.sh ${leg23_grp0_ld0_dn_mgr_id} ${leg23_grp0_ld0_dn_port_num} ${vd_id} ${leg23_grp0_ld0_id} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name}" | ssh ${dn_server_ip_E}
# delete leg23 grp0 ld1
echo "sudo /tmp/dn_ld_delete.sh ${leg23_grp0_ld1_dn_mgr_id} ${leg23_grp0_ld1_dn_port_num} ${vd_id} ${leg23_grp0_ld1_id} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name}" | ssh ${dn_server_ip_F}
# delete leg24 grp0 ld0
echo "sudo /tmp/dn_ld_delete.sh ${leg24_grp0_ld0_dn_mgr_id} ${leg24_grp0_ld0_dn_port_num} ${vd_id} ${leg24_grp0_ld0_id} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name}" | ssh ${dn_server_ip_G}
# delete leg24 grp0 ld1
echo "sudo /tmp/dn_ld_delete.sh ${leg24_grp0_ld1_dn_mgr_id} ${leg24_grp0_ld1_dn_port_num} ${vd_id} ${leg24_grp0_ld1_id} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name}" | ssh ${dn_server_ip_H}
# delete leg25 grp0 ld0
echo "sudo /tmp/dn_ld_delete.sh ${leg25_grp0_ld0_dn_mgr_id} ${leg25_grp0_ld0_dn_port_num} ${vd_id} ${leg25_grp0_ld0_id} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name}" | ssh ${dn_server_ip_G}
# delete leg25 grp0 ld1
echo "sudo /tmp/dn_ld_delete.sh ${leg25_grp0_ld1_dn_mgr_id} ${leg25_grp0_ld1_dn_port_num} ${vd_id} ${leg25_grp0_ld1_id} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name}" | ssh ${dn_server_ip_H}
# delete leg26 grp0 ld0
echo "sudo /tmp/dn_ld_delete.sh ${leg26_grp0_ld0_dn_mgr_id} ${leg26_grp0_ld0_dn_port_num} ${vd_id} ${leg26_grp0_ld0_id} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name}" | ssh ${dn_server_ip_G}
# delete leg26 grp0 ld1
echo "sudo /tmp/dn_ld_delete.sh ${leg26_grp0_ld1_dn_mgr_id} ${leg26_grp0_ld1_dn_port_num} ${vd_id} ${leg26_grp0_ld1_id} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name}" | ssh ${dn_server_ip_H}
# delete leg27 grp0 ld0
echo "sudo /tmp/dn_ld_delete.sh ${leg27_grp0_ld0_dn_mgr_id} ${leg27_grp0_ld0_dn_port_num} ${vd_id} ${leg27_grp0_ld0_id} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name}" | ssh ${dn_server_ip_G}
# delete leg27 grp0 ld1
echo "sudo /tmp/dn_ld_delete.sh ${leg27_grp0_ld1_dn_mgr_id} ${leg27_grp0_ld1_dn_port_num} ${vd_id} ${leg27_grp0_ld1_id} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name}" | ssh ${dn_server_ip_H}
# delete leg28 grp0 ld0
echo "sudo /tmp/dn_ld_delete.sh ${leg28_grp0_ld0_dn_mgr_id} ${leg28_grp0_ld0_dn_port_num} ${vd_id} ${leg28_grp0_ld0_id} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${dn_server_ip_G}
# delete leg28 grp0 ld1
echo "sudo /tmp/dn_ld_delete.sh ${leg28_grp0_ld1_dn_mgr_id} ${leg28_grp0_ld1_dn_port_num} ${vd_id} ${leg28_grp0_ld1_id} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${dn_server_ip_H}
# delete leg29 grp0 ld0
echo "sudo /tmp/dn_ld_delete.sh ${leg29_grp0_ld0_dn_mgr_id} ${leg29_grp0_ld0_dn_port_num} ${vd_id} ${leg29_grp0_ld0_id} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${dn_server_ip_G}
# delete leg29 grp0 ld1
echo "sudo /tmp/dn_ld_delete.sh ${leg29_grp0_ld1_dn_mgr_id} ${leg29_grp0_ld1_dn_port_num} ${vd_id} ${leg29_grp0_ld1_id} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${dn_server_ip_H}
# delete leg30 grp0 ld0
echo "sudo /tmp/dn_ld_delete.sh ${leg30_grp0_ld0_dn_mgr_id} ${leg30_grp0_ld0_dn_port_num} ${vd_id} ${leg30_grp0_ld0_id} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${dn_server_ip_G}
# delete leg30 grp0 ld1
echo "sudo /tmp/dn_ld_delete.sh ${leg30_grp0_ld1_dn_mgr_id} ${leg30_grp0_ld1_dn_port_num} ${vd_id} ${leg30_grp0_ld1_id} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${dn_server_ip_H}
# delete leg31 grp0 ld0
echo "sudo /tmp/dn_ld_delete.sh ${leg31_grp0_ld0_dn_mgr_id} ${leg31_grp0_ld0_dn_port_num} ${vd_id} ${leg31_grp0_ld0_id} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${dn_server_ip_G}
# delete leg31 grp0 ld1
echo "sudo /tmp/dn_ld_delete.sh ${leg31_grp0_ld1_dn_mgr_id} ${leg31_grp0_ld1_dn_port_num} ${vd_id} ${leg31_grp0_ld1_id} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${dn_server_ip_H}

# create leg00
echo "sudo /tmp/cn_leg_create.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg00_id} ${leg00_grp0_id} ${leg00_grp0_ld0_id} ${leg00_grp0_ld0_dn_mgr_id} ${leg00_grp0_ld0_dn_tr_addr} ${leg00_grp0_ld0_dn_tr_svc_id} ${leg00_grp0_ld1_id} ${leg00_grp0_ld1_dn_mgr_id} ${leg00_grp0_ld1_dn_tr_addr} ${leg00_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_A}
# create leg01
echo "sudo /tmp/cn_leg_create.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg01_id} ${leg01_grp0_id} ${leg01_grp0_ld0_id} ${leg01_grp0_ld0_dn_mgr_id} ${leg01_grp0_ld0_dn_tr_addr} ${leg01_grp0_ld0_dn_tr_svc_id} ${leg01_grp0_ld1_id} ${leg01_grp0_ld1_dn_mgr_id} ${leg01_grp0_ld1_dn_tr_addr} ${leg01_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_A}
# create leg02
echo "sudo /tmp/cn_leg_create.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg02_id} ${leg02_grp0_id} ${leg02_grp0_ld0_id} ${leg02_grp0_ld0_dn_mgr_id} ${leg02_grp0_ld0_dn_tr_addr} ${leg02_grp0_ld0_dn_tr_svc_id} ${leg02_grp0_ld1_id} ${leg02_grp0_ld1_dn_mgr_id} ${leg02_grp0_ld1_dn_tr_addr} ${leg02_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_A}
# create leg03
echo "sudo /tmp/cn_leg_create.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg03_id} ${leg03_grp0_id} ${leg03_grp0_ld0_id} ${leg03_grp0_ld0_dn_mgr_id} ${leg03_grp0_ld0_dn_tr_addr} ${leg03_grp0_ld0_dn_tr_svc_id} ${leg03_grp0_ld1_id} ${leg03_grp0_ld1_dn_mgr_id} ${leg03_grp0_ld1_dn_tr_addr} ${leg03_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_A}
# create leg04
echo "sudo /tmp/cn_leg_create.sh ${cntl01_cn_mgr_id} ${cntl01_cn_port_num} ${cntl01_cn_host_name} ${vd_id} ${leg04_id} ${leg04_grp0_id} ${leg04_grp0_ld0_id} ${leg04_grp0_ld0_dn_mgr_id} ${leg04_grp0_ld0_dn_tr_addr} ${leg04_grp0_ld0_dn_tr_svc_id} ${leg04_grp0_ld1_id} ${leg04_grp0_ld1_dn_mgr_id} ${leg04_grp0_ld1_dn_tr_addr} ${leg04_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_B}
# create leg05
echo "sudo /tmp/cn_leg_create.sh ${cntl01_cn_mgr_id} ${cntl01_cn_port_num} ${cntl01_cn_host_name} ${vd_id} ${leg05_id} ${leg05_grp0_id} ${leg05_grp0_ld0_id} ${leg05_grp0_ld0_dn_mgr_id} ${leg05_grp0_ld0_dn_tr_addr} ${leg05_grp0_ld0_dn_tr_svc_id} ${leg05_grp0_ld1_id} ${leg05_grp0_ld1_dn_mgr_id} ${leg05_grp0_ld1_dn_tr_addr} ${leg05_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_B}
# create leg06
echo "sudo /tmp/cn_leg_create.sh ${cntl01_cn_mgr_id} ${cntl01_cn_port_num} ${cntl01_cn_host_name} ${vd_id} ${leg06_id} ${leg06_grp0_id} ${leg06_grp0_ld0_id} ${leg06_grp0_ld0_dn_mgr_id} ${leg06_grp0_ld0_dn_tr_addr} ${leg06_grp0_ld0_dn_tr_svc_id} ${leg06_grp0_ld1_id} ${leg06_grp0_ld1_dn_mgr_id} ${leg06_grp0_ld1_dn_tr_addr} ${leg06_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_B}
# create leg07
echo "sudo /tmp/cn_leg_create.sh ${cntl01_cn_mgr_id} ${cntl01_cn_port_num} ${cntl01_cn_host_name} ${vd_id} ${leg07_id} ${leg07_grp0_id} ${leg07_grp0_ld0_id} ${leg07_grp0_ld0_dn_mgr_id} ${leg07_grp0_ld0_dn_tr_addr} ${leg07_grp0_ld0_dn_tr_svc_id} ${leg07_grp0_ld1_id} ${leg07_grp0_ld1_dn_mgr_id} ${leg07_grp0_ld1_dn_tr_addr} ${leg07_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_B}
# create leg08
echo "sudo /tmp/cn_leg_create.sh ${cntl02_cn_mgr_id} ${cntl02_cn_port_num} ${cntl02_cn_host_name} ${vd_id} ${leg08_id} ${leg08_grp0_id} ${leg08_grp0_ld0_id} ${leg08_grp0_ld0_dn_mgr_id} ${leg08_grp0_ld0_dn_tr_addr} ${leg08_grp0_ld0_dn_tr_svc_id} ${leg08_grp0_ld1_id} ${leg08_grp0_ld1_dn_mgr_id} ${leg08_grp0_ld1_dn_tr_addr} ${leg08_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_C}
# create leg09
echo "sudo /tmp/cn_leg_create.sh ${cntl02_cn_mgr_id} ${cntl02_cn_port_num} ${cntl02_cn_host_name} ${vd_id} ${leg09_id} ${leg09_grp0_id} ${leg09_grp0_ld0_id} ${leg09_grp0_ld0_dn_mgr_id} ${leg09_grp0_ld0_dn_tr_addr} ${leg09_grp0_ld0_dn_tr_svc_id} ${leg09_grp0_ld1_id} ${leg09_grp0_ld1_dn_mgr_id} ${leg09_grp0_ld1_dn_tr_addr} ${leg09_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_C}
# create leg10
echo "sudo /tmp/cn_leg_create.sh ${cntl02_cn_mgr_id} ${cntl02_cn_port_num} ${cntl02_cn_host_name} ${vd_id} ${leg10_id} ${leg10_grp0_id} ${leg10_grp0_ld0_id} ${leg10_grp0_ld0_dn_mgr_id} ${leg10_grp0_ld0_dn_tr_addr} ${leg10_grp0_ld0_dn_tr_svc_id} ${leg10_grp0_ld1_id} ${leg10_grp0_ld1_dn_mgr_id} ${leg10_grp0_ld1_dn_tr_addr} ${leg10_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_C}
# create leg11
echo "sudo /tmp/cn_leg_create.sh ${cntl02_cn_mgr_id} ${cntl02_cn_port_num} ${cntl02_cn_host_name} ${vd_id} ${leg11_id} ${leg11_grp0_id} ${leg11_grp0_ld0_id} ${leg11_grp0_ld0_dn_mgr_id} ${leg11_grp0_ld0_dn_tr_addr} ${leg11_grp0_ld0_dn_tr_svc_id} ${leg11_grp0_ld1_id} ${leg11_grp0_ld1_dn_mgr_id} ${leg11_grp0_ld1_dn_tr_addr} ${leg11_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_C}
# create leg12
echo "sudo /tmp/cn_leg_create.sh ${cntl03_cn_mgr_id} ${cntl03_cn_port_num} ${cntl03_cn_host_name} ${vd_id} ${leg12_id} ${leg12_grp0_id} ${leg12_grp0_ld0_id} ${leg12_grp0_ld0_dn_mgr_id} ${leg12_grp0_ld0_dn_tr_addr} ${leg12_grp0_ld0_dn_tr_svc_id} ${leg12_grp0_ld1_id} ${leg12_grp0_ld1_dn_mgr_id} ${leg12_grp0_ld1_dn_tr_addr} ${leg12_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_D}
# create leg13
echo "sudo /tmp/cn_leg_create.sh ${cntl03_cn_mgr_id} ${cntl03_cn_port_num} ${cntl03_cn_host_name} ${vd_id} ${leg13_id} ${leg13_grp0_id} ${leg13_grp0_ld0_id} ${leg13_grp0_ld0_dn_mgr_id} ${leg13_grp0_ld0_dn_tr_addr} ${leg13_grp0_ld0_dn_tr_svc_id} ${leg13_grp0_ld1_id} ${leg13_grp0_ld1_dn_mgr_id} ${leg13_grp0_ld1_dn_tr_addr} ${leg13_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_D}
# create leg14
echo "sudo /tmp/cn_leg_create.sh ${cntl03_cn_mgr_id} ${cntl03_cn_port_num} ${cntl03_cn_host_name} ${vd_id} ${leg14_id} ${leg14_grp0_id} ${leg14_grp0_ld0_id} ${leg14_grp0_ld0_dn_mgr_id} ${leg14_grp0_ld0_dn_tr_addr} ${leg14_grp0_ld0_dn_tr_svc_id} ${leg14_grp0_ld1_id} ${leg14_grp0_ld1_dn_mgr_id} ${leg14_grp0_ld1_dn_tr_addr} ${leg14_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_D}
# create leg15
echo "sudo /tmp/cn_leg_create.sh ${cntl03_cn_mgr_id} ${cntl03_cn_port_num} ${cntl03_cn_host_name} ${vd_id} ${leg15_id} ${leg15_grp0_id} ${leg15_grp0_ld0_id} ${leg15_grp0_ld0_dn_mgr_id} ${leg15_grp0_ld0_dn_tr_addr} ${leg15_grp0_ld0_dn_tr_svc_id} ${leg15_grp0_ld1_id} ${leg15_grp0_ld1_dn_mgr_id} ${leg15_grp0_ld1_dn_tr_addr} ${leg15_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_D}
# create leg16
echo "sudo /tmp/cn_leg_create.sh ${cntl04_cn_mgr_id} ${cntl04_cn_port_num} ${cntl04_cn_host_name} ${vd_id} ${leg16_id} ${leg16_grp0_id} ${leg16_grp0_ld0_id} ${leg16_grp0_ld0_dn_mgr_id} ${leg16_grp0_ld0_dn_tr_addr} ${leg16_grp0_ld0_dn_tr_svc_id} ${leg16_grp0_ld1_id} ${leg16_grp0_ld1_dn_mgr_id} ${leg16_grp0_ld1_dn_tr_addr} ${leg16_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_E}
# create leg17
echo "sudo /tmp/cn_leg_create.sh ${cntl04_cn_mgr_id} ${cntl04_cn_port_num} ${cntl04_cn_host_name} ${vd_id} ${leg17_id} ${leg17_grp0_id} ${leg17_grp0_ld0_id} ${leg17_grp0_ld0_dn_mgr_id} ${leg17_grp0_ld0_dn_tr_addr} ${leg17_grp0_ld0_dn_tr_svc_id} ${leg17_grp0_ld1_id} ${leg17_grp0_ld1_dn_mgr_id} ${leg17_grp0_ld1_dn_tr_addr} ${leg17_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_E}
# create leg18
echo "sudo /tmp/cn_leg_create.sh ${cntl04_cn_mgr_id} ${cntl04_cn_port_num} ${cntl04_cn_host_name} ${vd_id} ${leg18_id} ${leg18_grp0_id} ${leg18_grp0_ld0_id} ${leg18_grp0_ld0_dn_mgr_id} ${leg18_grp0_ld0_dn_tr_addr} ${leg18_grp0_ld0_dn_tr_svc_id} ${leg18_grp0_ld1_id} ${leg18_grp0_ld1_dn_mgr_id} ${leg18_grp0_ld1_dn_tr_addr} ${leg18_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_E}
# create leg19
echo "sudo /tmp/cn_leg_create.sh ${cntl04_cn_mgr_id} ${cntl04_cn_port_num} ${cntl04_cn_host_name} ${vd_id} ${leg19_id} ${leg19_grp0_id} ${leg19_grp0_ld0_id} ${leg19_grp0_ld0_dn_mgr_id} ${leg19_grp0_ld0_dn_tr_addr} ${leg19_grp0_ld0_dn_tr_svc_id} ${leg19_grp0_ld1_id} ${leg19_grp0_ld1_dn_mgr_id} ${leg19_grp0_ld1_dn_tr_addr} ${leg19_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_E}
# create leg20
echo "sudo /tmp/cn_leg_create.sh ${cntl05_cn_mgr_id} ${cntl05_cn_port_num} ${cntl05_cn_host_name} ${vd_id} ${leg20_id} ${leg20_grp0_id} ${leg20_grp0_ld0_id} ${leg20_grp0_ld0_dn_mgr_id} ${leg20_grp0_ld0_dn_tr_addr} ${leg20_grp0_ld0_dn_tr_svc_id} ${leg20_grp0_ld1_id} ${leg20_grp0_ld1_dn_mgr_id} ${leg20_grp0_ld1_dn_tr_addr} ${leg20_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_F}
# create leg21
echo "sudo /tmp/cn_leg_create.sh ${cntl05_cn_mgr_id} ${cntl05_cn_port_num} ${cntl05_cn_host_name} ${vd_id} ${leg21_id} ${leg21_grp0_id} ${leg21_grp0_ld0_id} ${leg21_grp0_ld0_dn_mgr_id} ${leg21_grp0_ld0_dn_tr_addr} ${leg21_grp0_ld0_dn_tr_svc_id} ${leg21_grp0_ld1_id} ${leg21_grp0_ld1_dn_mgr_id} ${leg21_grp0_ld1_dn_tr_addr} ${leg21_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_F}
# create leg22
echo "sudo /tmp/cn_leg_create.sh ${cntl05_cn_mgr_id} ${cntl05_cn_port_num} ${cntl05_cn_host_name} ${vd_id} ${leg22_id} ${leg22_grp0_id} ${leg22_grp0_ld0_id} ${leg22_grp0_ld0_dn_mgr_id} ${leg22_grp0_ld0_dn_tr_addr} ${leg22_grp0_ld0_dn_tr_svc_id} ${leg22_grp0_ld1_id} ${leg22_grp0_ld1_dn_mgr_id} ${leg22_grp0_ld1_dn_tr_addr} ${leg22_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_F}
# create leg23
echo "sudo /tmp/cn_leg_create.sh ${cntl05_cn_mgr_id} ${cntl05_cn_port_num} ${cntl05_cn_host_name} ${vd_id} ${leg23_id} ${leg23_grp0_id} ${leg23_grp0_ld0_id} ${leg23_grp0_ld0_dn_mgr_id} ${leg23_grp0_ld0_dn_tr_addr} ${leg23_grp0_ld0_dn_tr_svc_id} ${leg23_grp0_ld1_id} ${leg23_grp0_ld1_dn_mgr_id} ${leg23_grp0_ld1_dn_tr_addr} ${leg23_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_F}
# create leg24
echo "sudo /tmp/cn_leg_create.sh ${cntl06_cn_mgr_id} ${cntl06_cn_port_num} ${cntl06_cn_host_name} ${vd_id} ${leg24_id} ${leg24_grp0_id} ${leg24_grp0_ld0_id} ${leg24_grp0_ld0_dn_mgr_id} ${leg24_grp0_ld0_dn_tr_addr} ${leg24_grp0_ld0_dn_tr_svc_id} ${leg24_grp0_ld1_id} ${leg24_grp0_ld1_dn_mgr_id} ${leg24_grp0_ld1_dn_tr_addr} ${leg24_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_G}
# create leg25
echo "sudo /tmp/cn_leg_create.sh ${cntl06_cn_mgr_id} ${cntl06_cn_port_num} ${cntl06_cn_host_name} ${vd_id} ${leg25_id} ${leg25_grp0_id} ${leg25_grp0_ld0_id} ${leg25_grp0_ld0_dn_mgr_id} ${leg25_grp0_ld0_dn_tr_addr} ${leg25_grp0_ld0_dn_tr_svc_id} ${leg25_grp0_ld1_id} ${leg25_grp0_ld1_dn_mgr_id} ${leg25_grp0_ld1_dn_tr_addr} ${leg25_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_G}
# create leg26
echo "sudo /tmp/cn_leg_create.sh ${cntl06_cn_mgr_id} ${cntl06_cn_port_num} ${cntl06_cn_host_name} ${vd_id} ${leg26_id} ${leg26_grp0_id} ${leg26_grp0_ld0_id} ${leg26_grp0_ld0_dn_mgr_id} ${leg26_grp0_ld0_dn_tr_addr} ${leg26_grp0_ld0_dn_tr_svc_id} ${leg26_grp0_ld1_id} ${leg26_grp0_ld1_dn_mgr_id} ${leg26_grp0_ld1_dn_tr_addr} ${leg26_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_G}
# create leg27
echo "sudo /tmp/cn_leg_create.sh ${cntl06_cn_mgr_id} ${cntl06_cn_port_num} ${cntl06_cn_host_name} ${vd_id} ${leg27_id} ${leg27_grp0_id} ${leg27_grp0_ld0_id} ${leg27_grp0_ld0_dn_mgr_id} ${leg27_grp0_ld0_dn_tr_addr} ${leg27_grp0_ld0_dn_tr_svc_id} ${leg27_grp0_ld1_id} ${leg27_grp0_ld1_dn_mgr_id} ${leg27_grp0_ld1_dn_tr_addr} ${leg27_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_G}
# create leg28
echo "sudo /tmp/cn_leg_create.sh ${cntl07_cn_mgr_id} ${cntl07_cn_port_num} ${cntl07_cn_host_name} ${vd_id} ${leg28_id} ${leg28_grp0_id} ${leg28_grp0_ld0_id} ${leg28_grp0_ld0_dn_mgr_id} ${leg28_grp0_ld0_dn_tr_addr} ${leg28_grp0_ld0_dn_tr_svc_id} ${leg28_grp0_ld1_id} ${leg28_grp0_ld1_dn_mgr_id} ${leg28_grp0_ld1_dn_tr_addr} ${leg28_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name}" | ssh ${cn_server_ip_H}
# create leg29
echo "sudo /tmp/cn_leg_create.sh ${cntl07_cn_mgr_id} ${cntl07_cn_port_num} ${cntl07_cn_host_name} ${vd_id} ${leg29_id} ${leg29_grp0_id} ${leg29_grp0_ld0_id} ${leg29_grp0_ld0_dn_mgr_id} ${leg29_grp0_ld0_dn_tr_addr} ${leg29_grp0_ld0_dn_tr_svc_id} ${leg29_grp0_ld1_id} ${leg29_grp0_ld1_dn_mgr_id} ${leg29_grp0_ld1_dn_tr_addr} ${leg29_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name}" | ssh ${cn_server_ip_H}
# create leg30
echo "sudo /tmp/cn_leg_create.sh ${cntl07_cn_mgr_id} ${cntl07_cn_port_num} ${cntl07_cn_host_name} ${vd_id} ${leg30_id} ${leg30_grp0_id} ${leg30_grp0_ld0_id} ${leg30_grp0_ld0_dn_mgr_id} ${leg30_grp0_ld0_dn_tr_addr} ${leg30_grp0_ld0_dn_tr_svc_id} ${leg30_grp0_ld1_id} ${leg30_grp0_ld1_dn_mgr_id} ${leg30_grp0_ld1_dn_tr_addr} ${leg30_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name}" | ssh ${cn_server_ip_H}
# create leg31
echo "sudo /tmp/cn_leg_create.sh ${cntl07_cn_mgr_id} ${cntl07_cn_port_num} ${cntl07_cn_host_name} ${vd_id} ${leg31_id} ${leg31_grp0_id} ${leg31_grp0_ld0_id} ${leg31_grp0_ld0_dn_mgr_id} ${leg31_grp0_ld0_dn_tr_addr} ${leg31_grp0_ld0_dn_tr_svc_id} ${leg31_grp0_ld1_id} ${leg31_grp0_ld1_dn_mgr_id} ${leg31_grp0_ld1_dn_tr_addr} ${leg31_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name}" | ssh ${cn_server_ip_H}

# delete leg00
echo "sudo /tmp/cn_leg_delete.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg00_id} ${leg00_grp0_id} ${leg00_grp0_ld0_id} ${leg00_grp0_ld0_dn_mgr_id} ${leg00_grp0_ld0_dn_tr_addr} ${leg00_grp0_ld0_dn_tr_svc_id} ${leg00_grp0_ld1_id} ${leg00_grp0_ld1_dn_mgr_id} ${leg00_grp0_ld1_dn_tr_addr} ${leg00_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_A}
# delete leg01
echo "sudo /tmp/cn_leg_delete.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg01_id} ${leg01_grp0_id} ${leg01_grp0_ld0_id} ${leg01_grp0_ld0_dn_mgr_id} ${leg01_grp0_ld0_dn_tr_addr} ${leg01_grp0_ld0_dn_tr_svc_id} ${leg01_grp0_ld1_id} ${leg01_grp0_ld1_dn_mgr_id} ${leg01_grp0_ld1_dn_tr_addr} ${leg01_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_A}
# delete leg02
echo "sudo /tmp/cn_leg_delete.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg02_id} ${leg02_grp0_id} ${leg02_grp0_ld0_id} ${leg02_grp0_ld0_dn_mgr_id} ${leg02_grp0_ld0_dn_tr_addr} ${leg02_grp0_ld0_dn_tr_svc_id} ${leg02_grp0_ld1_id} ${leg02_grp0_ld1_dn_mgr_id} ${leg02_grp0_ld1_dn_tr_addr} ${leg02_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_A}
# delete leg03
echo "sudo /tmp/cn_leg_delete.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${leg03_id} ${leg03_grp0_id} ${leg03_grp0_ld0_id} ${leg03_grp0_ld0_dn_mgr_id} ${leg03_grp0_ld0_dn_tr_addr} ${leg03_grp0_ld0_dn_tr_svc_id} ${leg03_grp0_ld1_id} ${leg03_grp0_ld1_dn_mgr_id} ${leg03_grp0_ld1_dn_tr_addr} ${leg03_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_A}
# delete leg04
echo "sudo /tmp/cn_leg_delete.sh ${cntl01_cn_mgr_id} ${cntl01_cn_port_num} ${cntl01_cn_host_name} ${vd_id} ${leg04_id} ${leg04_grp0_id} ${leg04_grp0_ld0_id} ${leg04_grp0_ld0_dn_mgr_id} ${leg04_grp0_ld0_dn_tr_addr} ${leg04_grp0_ld0_dn_tr_svc_id} ${leg04_grp0_ld1_id} ${leg04_grp0_ld1_dn_mgr_id} ${leg04_grp0_ld1_dn_tr_addr} ${leg04_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_B}
# delete leg05
echo "sudo /tmp/cn_leg_delete.sh ${cntl01_cn_mgr_id} ${cntl01_cn_port_num} ${cntl01_cn_host_name} ${vd_id} ${leg05_id} ${leg05_grp0_id} ${leg05_grp0_ld0_id} ${leg05_grp0_ld0_dn_mgr_id} ${leg05_grp0_ld0_dn_tr_addr} ${leg05_grp0_ld0_dn_tr_svc_id} ${leg05_grp0_ld1_id} ${leg05_grp0_ld1_dn_mgr_id} ${leg05_grp0_ld1_dn_tr_addr} ${leg05_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_B}
# delete leg06
echo "sudo /tmp/cn_leg_delete.sh ${cntl01_cn_mgr_id} ${cntl01_cn_port_num} ${cntl01_cn_host_name} ${vd_id} ${leg06_id} ${leg06_grp0_id} ${leg06_grp0_ld0_id} ${leg06_grp0_ld0_dn_mgr_id} ${leg06_grp0_ld0_dn_tr_addr} ${leg06_grp0_ld0_dn_tr_svc_id} ${leg06_grp0_ld1_id} ${leg06_grp0_ld1_dn_mgr_id} ${leg06_grp0_ld1_dn_tr_addr} ${leg06_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_B}
# delete leg07
echo "sudo /tmp/cn_leg_delete.sh ${cntl01_cn_mgr_id} ${cntl01_cn_port_num} ${cntl01_cn_host_name} ${vd_id} ${leg07_id} ${leg07_grp0_id} ${leg07_grp0_ld0_id} ${leg07_grp0_ld0_dn_mgr_id} ${leg07_grp0_ld0_dn_tr_addr} ${leg07_grp0_ld0_dn_tr_svc_id} ${leg07_grp0_ld1_id} ${leg07_grp0_ld1_dn_mgr_id} ${leg07_grp0_ld1_dn_tr_addr} ${leg07_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_B}
# delete leg08
echo "sudo /tmp/cn_leg_delete.sh ${cntl02_cn_mgr_id} ${cntl02_cn_port_num} ${cntl02_cn_host_name} ${vd_id} ${leg08_id} ${leg08_grp0_id} ${leg08_grp0_ld0_id} ${leg08_grp0_ld0_dn_mgr_id} ${leg08_grp0_ld0_dn_tr_addr} ${leg08_grp0_ld0_dn_tr_svc_id} ${leg08_grp0_ld1_id} ${leg08_grp0_ld1_dn_mgr_id} ${leg08_grp0_ld1_dn_tr_addr} ${leg08_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_C}
# delete leg09
echo "sudo /tmp/cn_leg_delete.sh ${cntl02_cn_mgr_id} ${cntl02_cn_port_num} ${cntl02_cn_host_name} ${vd_id} ${leg09_id} ${leg09_grp0_id} ${leg09_grp0_ld0_id} ${leg09_grp0_ld0_dn_mgr_id} ${leg09_grp0_ld0_dn_tr_addr} ${leg09_grp0_ld0_dn_tr_svc_id} ${leg09_grp0_ld1_id} ${leg09_grp0_ld1_dn_mgr_id} ${leg09_grp0_ld1_dn_tr_addr} ${leg09_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_C}
# delete leg10
echo "sudo /tmp/cn_leg_delete.sh ${cntl02_cn_mgr_id} ${cntl02_cn_port_num} ${cntl02_cn_host_name} ${vd_id} ${leg10_id} ${leg10_grp0_id} ${leg10_grp0_ld0_id} ${leg10_grp0_ld0_dn_mgr_id} ${leg10_grp0_ld0_dn_tr_addr} ${leg10_grp0_ld0_dn_tr_svc_id} ${leg10_grp0_ld1_id} ${leg10_grp0_ld1_dn_mgr_id} ${leg10_grp0_ld1_dn_tr_addr} ${leg10_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_C}
# delete leg11
echo "sudo /tmp/cn_leg_delete.sh ${cntl02_cn_mgr_id} ${cntl02_cn_port_num} ${cntl02_cn_host_name} ${vd_id} ${leg11_id} ${leg11_grp0_id} ${leg11_grp0_ld0_id} ${leg11_grp0_ld0_dn_mgr_id} ${leg11_grp0_ld0_dn_tr_addr} ${leg11_grp0_ld0_dn_tr_svc_id} ${leg11_grp0_ld1_id} ${leg11_grp0_ld1_dn_mgr_id} ${leg11_grp0_ld1_dn_tr_addr} ${leg11_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_C}
# delete leg12
echo "sudo /tmp/cn_leg_delete.sh ${cntl03_cn_mgr_id} ${cntl03_cn_port_num} ${cntl03_cn_host_name} ${vd_id} ${leg12_id} ${leg12_grp0_id} ${leg12_grp0_ld0_id} ${leg12_grp0_ld0_dn_mgr_id} ${leg12_grp0_ld0_dn_tr_addr} ${leg12_grp0_ld0_dn_tr_svc_id} ${leg12_grp0_ld1_id} ${leg12_grp0_ld1_dn_mgr_id} ${leg12_grp0_ld1_dn_tr_addr} ${leg12_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_D}
# delete leg13
echo "sudo /tmp/cn_leg_delete.sh ${cntl03_cn_mgr_id} ${cntl03_cn_port_num} ${cntl03_cn_host_name} ${vd_id} ${leg13_id} ${leg13_grp0_id} ${leg13_grp0_ld0_id} ${leg13_grp0_ld0_dn_mgr_id} ${leg13_grp0_ld0_dn_tr_addr} ${leg13_grp0_ld0_dn_tr_svc_id} ${leg13_grp0_ld1_id} ${leg13_grp0_ld1_dn_mgr_id} ${leg13_grp0_ld1_dn_tr_addr} ${leg13_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_D}
# delete leg14
echo "sudo /tmp/cn_leg_delete.sh ${cntl03_cn_mgr_id} ${cntl03_cn_port_num} ${cntl03_cn_host_name} ${vd_id} ${leg14_id} ${leg14_grp0_id} ${leg14_grp0_ld0_id} ${leg14_grp0_ld0_dn_mgr_id} ${leg14_grp0_ld0_dn_tr_addr} ${leg14_grp0_ld0_dn_tr_svc_id} ${leg14_grp0_ld1_id} ${leg14_grp0_ld1_dn_mgr_id} ${leg14_grp0_ld1_dn_tr_addr} ${leg14_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_D}
# delete leg15
echo "sudo /tmp/cn_leg_delete.sh ${cntl03_cn_mgr_id} ${cntl03_cn_port_num} ${cntl03_cn_host_name} ${vd_id} ${leg15_id} ${leg15_grp0_id} ${leg15_grp0_ld0_id} ${leg15_grp0_ld0_dn_mgr_id} ${leg15_grp0_ld0_dn_tr_addr} ${leg15_grp0_ld0_dn_tr_svc_id} ${leg15_grp0_ld1_id} ${leg15_grp0_ld1_dn_mgr_id} ${leg15_grp0_ld1_dn_tr_addr} ${leg15_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_D}
# delete leg16
echo "sudo /tmp/cn_leg_delete.sh ${cntl04_cn_mgr_id} ${cntl04_cn_port_num} ${cntl04_cn_host_name} ${vd_id} ${leg16_id} ${leg16_grp0_id} ${leg16_grp0_ld0_id} ${leg16_grp0_ld0_dn_mgr_id} ${leg16_grp0_ld0_dn_tr_addr} ${leg16_grp0_ld0_dn_tr_svc_id} ${leg16_grp0_ld1_id} ${leg16_grp0_ld1_dn_mgr_id} ${leg16_grp0_ld1_dn_tr_addr} ${leg16_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_E}
# delete leg17
echo "sudo /tmp/cn_leg_delete.sh ${cntl04_cn_mgr_id} ${cntl04_cn_port_num} ${cntl04_cn_host_name} ${vd_id} ${leg17_id} ${leg17_grp0_id} ${leg17_grp0_ld0_id} ${leg17_grp0_ld0_dn_mgr_id} ${leg17_grp0_ld0_dn_tr_addr} ${leg17_grp0_ld0_dn_tr_svc_id} ${leg17_grp0_ld1_id} ${leg17_grp0_ld1_dn_mgr_id} ${leg17_grp0_ld1_dn_tr_addr} ${leg17_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_E}
# delete leg18
echo "sudo /tmp/cn_leg_delete.sh ${cntl04_cn_mgr_id} ${cntl04_cn_port_num} ${cntl04_cn_host_name} ${vd_id} ${leg18_id} ${leg18_grp0_id} ${leg18_grp0_ld0_id} ${leg18_grp0_ld0_dn_mgr_id} ${leg18_grp0_ld0_dn_tr_addr} ${leg18_grp0_ld0_dn_tr_svc_id} ${leg18_grp0_ld1_id} ${leg18_grp0_ld1_dn_mgr_id} ${leg18_grp0_ld1_dn_tr_addr} ${leg18_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_E}
# delete leg19
echo "sudo /tmp/cn_leg_delete.sh ${cntl04_cn_mgr_id} ${cntl04_cn_port_num} ${cntl04_cn_host_name} ${vd_id} ${leg19_id} ${leg19_grp0_id} ${leg19_grp0_ld0_id} ${leg19_grp0_ld0_dn_mgr_id} ${leg19_grp0_ld0_dn_tr_addr} ${leg19_grp0_ld0_dn_tr_svc_id} ${leg19_grp0_ld1_id} ${leg19_grp0_ld1_dn_mgr_id} ${leg19_grp0_ld1_dn_tr_addr} ${leg19_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_E}
# delete leg20
echo "sudo /tmp/cn_leg_delete.sh ${cntl05_cn_mgr_id} ${cntl05_cn_port_num} ${cntl05_cn_host_name} ${vd_id} ${leg20_id} ${leg20_grp0_id} ${leg20_grp0_ld0_id} ${leg20_grp0_ld0_dn_mgr_id} ${leg20_grp0_ld0_dn_tr_addr} ${leg20_grp0_ld0_dn_tr_svc_id} ${leg20_grp0_ld1_id} ${leg20_grp0_ld1_dn_mgr_id} ${leg20_grp0_ld1_dn_tr_addr} ${leg20_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_F}
# delete leg21
echo "sudo /tmp/cn_leg_delete.sh ${cntl05_cn_mgr_id} ${cntl05_cn_port_num} ${cntl05_cn_host_name} ${vd_id} ${leg21_id} ${leg21_grp0_id} ${leg21_grp0_ld0_id} ${leg21_grp0_ld0_dn_mgr_id} ${leg21_grp0_ld0_dn_tr_addr} ${leg21_grp0_ld0_dn_tr_svc_id} ${leg21_grp0_ld1_id} ${leg21_grp0_ld1_dn_mgr_id} ${leg21_grp0_ld1_dn_tr_addr} ${leg21_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_F}
# delete leg22
echo "sudo /tmp/cn_leg_delete.sh ${cntl05_cn_mgr_id} ${cntl05_cn_port_num} ${cntl05_cn_host_name} ${vd_id} ${leg22_id} ${leg22_grp0_id} ${leg22_grp0_ld0_id} ${leg22_grp0_ld0_dn_mgr_id} ${leg22_grp0_ld0_dn_tr_addr} ${leg22_grp0_ld0_dn_tr_svc_id} ${leg22_grp0_ld1_id} ${leg22_grp0_ld1_dn_mgr_id} ${leg22_grp0_ld1_dn_tr_addr} ${leg22_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_F}
# delete leg23
echo "sudo /tmp/cn_leg_delete.sh ${cntl05_cn_mgr_id} ${cntl05_cn_port_num} ${cntl05_cn_host_name} ${vd_id} ${leg23_id} ${leg23_grp0_id} ${leg23_grp0_ld0_id} ${leg23_grp0_ld0_dn_mgr_id} ${leg23_grp0_ld0_dn_tr_addr} ${leg23_grp0_ld0_dn_tr_svc_id} ${leg23_grp0_ld1_id} ${leg23_grp0_ld1_dn_mgr_id} ${leg23_grp0_ld1_dn_tr_addr} ${leg23_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_F}
# delete leg24
echo "sudo /tmp/cn_leg_delete.sh ${cntl06_cn_mgr_id} ${cntl06_cn_port_num} ${cntl06_cn_host_name} ${vd_id} ${leg24_id} ${leg24_grp0_id} ${leg24_grp0_ld0_id} ${leg24_grp0_ld0_dn_mgr_id} ${leg24_grp0_ld0_dn_tr_addr} ${leg24_grp0_ld0_dn_tr_svc_id} ${leg24_grp0_ld1_id} ${leg24_grp0_ld1_dn_mgr_id} ${leg24_grp0_ld1_dn_tr_addr} ${leg24_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_G}
# delete leg25
echo "sudo /tmp/cn_leg_delete.sh ${cntl06_cn_mgr_id} ${cntl06_cn_port_num} ${cntl06_cn_host_name} ${vd_id} ${leg25_id} ${leg25_grp0_id} ${leg25_grp0_ld0_id} ${leg25_grp0_ld0_dn_mgr_id} ${leg25_grp0_ld0_dn_tr_addr} ${leg25_grp0_ld0_dn_tr_svc_id} ${leg25_grp0_ld1_id} ${leg25_grp0_ld1_dn_mgr_id} ${leg25_grp0_ld1_dn_tr_addr} ${leg25_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_G}
# delete leg26
echo "sudo /tmp/cn_leg_delete.sh ${cntl06_cn_mgr_id} ${cntl06_cn_port_num} ${cntl06_cn_host_name} ${vd_id} ${leg26_id} ${leg26_grp0_id} ${leg26_grp0_ld0_id} ${leg26_grp0_ld0_dn_mgr_id} ${leg26_grp0_ld0_dn_tr_addr} ${leg26_grp0_ld0_dn_tr_svc_id} ${leg26_grp0_ld1_id} ${leg26_grp0_ld1_dn_mgr_id} ${leg26_grp0_ld1_dn_tr_addr} ${leg26_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_G}
# delete leg27
echo "sudo /tmp/cn_leg_delete.sh ${cntl06_cn_mgr_id} ${cntl06_cn_port_num} ${cntl06_cn_host_name} ${vd_id} ${leg27_id} ${leg27_grp0_id} ${leg27_grp0_ld0_id} ${leg27_grp0_ld0_dn_mgr_id} ${leg27_grp0_ld0_dn_tr_addr} ${leg27_grp0_ld0_dn_tr_svc_id} ${leg27_grp0_ld1_id} ${leg27_grp0_ld1_dn_mgr_id} ${leg27_grp0_ld1_dn_tr_addr} ${leg27_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl07_cn_mgr_id} ${cntl07_cn_host_name}" | ssh ${cn_server_ip_G}
# delete leg28
echo "sudo /tmp/cn_leg_delete.sh ${cntl07_cn_mgr_id} ${cntl07_cn_port_num} ${cntl07_cn_host_name} ${vd_id} ${leg28_id} ${leg28_grp0_id} ${leg28_grp0_ld0_id} ${leg28_grp0_ld0_dn_mgr_id} ${leg28_grp0_ld0_dn_tr_addr} ${leg28_grp0_ld0_dn_tr_svc_id} ${leg28_grp0_ld1_id} ${leg28_grp0_ld1_dn_mgr_id} ${leg28_grp0_ld1_dn_tr_addr} ${leg28_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name}" | ssh ${cn_server_ip_H}
# delete leg29
echo "sudo /tmp/cn_leg_delete.sh ${cntl07_cn_mgr_id} ${cntl07_cn_port_num} ${cntl07_cn_host_name} ${vd_id} ${leg29_id} ${leg29_grp0_id} ${leg29_grp0_ld0_id} ${leg29_grp0_ld0_dn_mgr_id} ${leg29_grp0_ld0_dn_tr_addr} ${leg29_grp0_ld0_dn_tr_svc_id} ${leg29_grp0_ld1_id} ${leg29_grp0_ld1_dn_mgr_id} ${leg29_grp0_ld1_dn_tr_addr} ${leg29_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name}" | ssh ${cn_server_ip_H}
# delete leg30
echo "sudo /tmp/cn_leg_delete.sh ${cntl07_cn_mgr_id} ${cntl07_cn_port_num} ${cntl07_cn_host_name} ${vd_id} ${leg30_id} ${leg30_grp0_id} ${leg30_grp0_ld0_id} ${leg30_grp0_ld0_dn_mgr_id} ${leg30_grp0_ld0_dn_tr_addr} ${leg30_grp0_ld0_dn_tr_svc_id} ${leg30_grp0_ld1_id} ${leg30_grp0_ld1_dn_mgr_id} ${leg30_grp0_ld1_dn_tr_addr} ${leg30_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name}" | ssh ${cn_server_ip_H}
# delete leg31
echo "sudo /tmp/cn_leg_delete.sh ${cntl07_cn_mgr_id} ${cntl07_cn_port_num} ${cntl07_cn_host_name} ${vd_id} ${leg31_id} ${leg31_grp0_id} ${leg31_grp0_ld0_id} ${leg31_grp0_ld0_dn_mgr_id} ${leg31_grp0_ld0_dn_tr_addr} ${leg31_grp0_ld0_dn_tr_svc_id} ${leg31_grp0_ld1_id} ${leg31_grp0_ld1_dn_mgr_id} ${leg31_grp0_ld1_dn_tr_addr} ${leg31_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt} ${cntl00_cn_mgr_id} ${cntl00_cn_host_name} ${cntl01_cn_mgr_id} ${cntl01_cn_host_name} ${cntl02_cn_mgr_id} ${cntl02_cn_host_name} ${cntl03_cn_mgr_id} ${cntl03_cn_host_name} ${cntl04_cn_mgr_id} ${cntl04_cn_host_name} ${cntl05_cn_mgr_id} ${cntl05_cn_host_name} ${cntl06_cn_mgr_id} ${cntl06_cn_host_name}" | ssh ${cn_server_ip_H}

# create cntl00
echo "sudo /tmp/cn_cntl_create.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${cntl00_cntlid_min} ${cntl00_cntlid_max} ${vd_id} ${external_host_nqn} ${thin_dev_size_mb} ${leg_cnt} ${leg00_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg01_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg02_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg03_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg04_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg05_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg06_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg07_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg08_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg09_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg10_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg11_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg12_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg13_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg14_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg15_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg16_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg17_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg18_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg19_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg20_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg21_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg22_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg23_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg24_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg25_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg26_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg27_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg28_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg29_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg30_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg31_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id}" | ssh ${cn_server_ip_A}
# create cntl01
echo "sudo /tmp/cn_cntl_create.sh ${cntl01_cn_mgr_id} ${cntl01_cn_port_num} ${cntl01_cn_host_name} ${cntl01_cntlid_min} ${cntl01_cntlid_max} ${vd_id} ${external_host_nqn} ${thin_dev_size_mb} ${leg_cnt} ${leg00_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg01_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg02_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg03_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg04_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg05_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg06_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg07_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg08_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg09_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg10_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg11_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg12_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg13_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg14_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg15_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg16_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg17_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg18_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg19_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg20_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg21_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg22_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg23_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg24_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg25_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg26_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg27_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg28_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg29_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg30_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg31_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id}" | ssh ${cn_server_ip_B}
# create cntl02
echo "sudo /tmp/cn_cntl_create.sh ${cntl02_cn_mgr_id} ${cntl02_cn_port_num} ${cntl02_cn_host_name} ${cntl02_cntlid_min} ${cntl02_cntlid_max} ${vd_id} ${external_host_nqn} ${thin_dev_size_mb} ${leg_cnt} ${leg00_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg01_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg02_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg03_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg04_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg05_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg06_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg07_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg08_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg09_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg10_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg11_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg12_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg13_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg14_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg15_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg16_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg17_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg18_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg19_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg20_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg21_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg22_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg23_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg24_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg25_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg26_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg27_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg28_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg29_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg30_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg31_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id}" | ssh ${cn_server_ip_C}
# create cntl03
echo "sudo /tmp/cn_cntl_create.sh ${cntl03_cn_mgr_id} ${cntl03_cn_port_num} ${cntl03_cn_host_name} ${cntl03_cntlid_min} ${cntl03_cntlid_max} ${vd_id} ${external_host_nqn} ${thin_dev_size_mb} ${leg_cnt} ${leg00_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg01_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg02_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg03_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg04_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg05_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg06_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg07_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg08_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg09_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg10_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg11_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg12_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg13_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg14_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg15_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg16_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg17_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg18_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg19_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg20_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg21_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg22_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg23_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg24_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg25_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg26_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg27_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg28_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg29_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg30_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg31_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id}" | ssh ${cn_server_ip_D}
# create cntl04
echo "sudo /tmp/cn_cntl_create.sh ${cntl04_cn_mgr_id} ${cntl04_cn_port_num} ${cntl04_cn_host_name} ${cntl04_cntlid_min} ${cntl04_cntlid_max} ${vd_id} ${external_host_nqn} ${thin_dev_size_mb} ${leg_cnt} ${leg00_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg01_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg02_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg03_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg04_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg05_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg06_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg07_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg08_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg09_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg10_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg11_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg12_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg13_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg14_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg15_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg16_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg17_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg18_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg19_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg20_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg21_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg22_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg23_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg24_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg25_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg26_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg27_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg28_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg29_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg30_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg31_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id}" | ssh ${cn_server_ip_E}
# create cntl05
echo "sudo /tmp/cn_cntl_create.sh ${cntl05_cn_mgr_id} ${cntl05_cn_port_num} ${cntl05_cn_host_name} ${cntl05_cntlid_min} ${cntl05_cntlid_max} ${vd_id} ${external_host_nqn} ${thin_dev_size_mb} ${leg_cnt} ${leg00_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg01_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg02_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg03_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg04_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg05_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg06_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg07_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg08_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg09_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg10_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg11_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg12_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg13_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg14_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg15_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg16_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg17_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg18_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg19_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg20_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg21_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg22_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg23_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg24_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg25_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg26_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg27_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg28_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg29_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg30_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg31_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id}" | ssh ${cn_server_ip_F}
# create cntl06
echo "sudo /tmp/cn_cntl_create.sh ${cntl06_cn_mgr_id} ${cntl06_cn_port_num} ${cntl06_cn_host_name} ${cntl06_cntlid_min} ${cntl06_cntlid_max} ${vd_id} ${external_host_nqn} ${thin_dev_size_mb} ${leg_cnt} ${leg00_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg01_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg02_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg03_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg04_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg05_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg06_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg07_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg08_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg09_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg10_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg11_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg12_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg13_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg14_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg15_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg16_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg17_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg18_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg19_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg20_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg21_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg22_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg23_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg24_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg25_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg26_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg27_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg28_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg29_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg30_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg31_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id}" | ssh ${cn_server_ip_G}
# create cntl07
echo "sudo /tmp/cn_cntl_create.sh ${cntl07_cn_mgr_id} ${cntl07_cn_port_num} ${cntl07_cn_host_name} ${cntl07_cntlid_min} ${cntl07_cntlid_max} ${vd_id} ${external_host_nqn} ${thin_dev_size_mb} ${leg_cnt} ${leg00_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg01_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg02_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg03_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg04_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg05_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg06_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg07_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg08_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg09_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg10_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg11_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg12_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg13_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg14_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg15_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg16_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg17_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg18_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg19_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg20_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg21_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg22_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg23_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg24_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg25_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg26_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg27_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg28_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg29_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg30_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg31_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id}" | ssh ${cn_server_ip_H}

# delete cntl00
echo "sudo /tmp/cn_cntl_delete.sh ${cntl00_cn_mgr_id} ${cntl00_cn_port_num} ${cntl00_cn_host_name} ${vd_id} ${external_host_nqn} ${leg_cnt} ${leg00_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg01_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg02_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg03_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg04_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg05_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg06_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg07_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg08_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg09_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg10_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg11_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg12_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg13_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg14_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg15_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg16_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg17_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg18_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg19_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg20_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg21_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg22_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg23_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg24_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg25_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg26_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg27_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg28_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg29_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg30_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg31_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id}" | ssh ${cn_server_ip_A}
# delete cntl01
echo "sudo /tmp/cn_cntl_delete.sh ${cntl01_cn_mgr_id} ${cntl01_cn_port_num} ${cntl01_cn_host_name} ${vd_id} ${external_host_nqn} ${leg_cnt} ${leg00_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg01_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg02_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg03_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg04_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg05_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg06_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg07_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg08_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg09_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg10_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg11_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg12_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg13_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg14_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg15_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg16_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg17_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg18_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg19_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg20_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg21_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg22_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg23_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg24_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg25_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg26_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg27_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg28_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg29_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg30_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg31_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id}" | ssh ${cn_server_ip_B}
# delete cntl02
echo "sudo /tmp/cn_cntl_delete.sh ${cntl02_cn_mgr_id} ${cntl02_cn_port_num} ${cntl02_cn_host_name} ${vd_id} ${external_host_nqn} ${leg_cnt} ${leg00_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg01_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg02_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg03_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg04_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg05_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg06_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg07_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg08_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg09_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg10_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg11_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg12_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg13_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg14_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg15_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg16_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg17_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg18_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg19_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg20_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg21_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg22_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg23_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg24_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg25_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg26_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg27_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg28_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg29_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg30_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg31_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id}" | ssh ${cn_server_ip_C}
# delete cntl03
echo "sudo /tmp/cn_cntl_delete.sh ${cntl03_cn_mgr_id} ${cntl03_cn_port_num} ${cntl03_cn_host_name} ${vd_id} ${external_host_nqn} ${leg_cnt} ${leg00_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg01_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg02_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg03_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg04_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg05_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg06_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg07_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg08_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg09_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg10_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg11_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg12_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg13_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg14_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg15_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg16_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg17_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg18_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg19_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg20_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg21_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg22_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg23_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg24_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg25_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg26_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg27_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg28_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg29_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg30_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg31_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id}" | ssh ${cn_server_ip_D}
# delete cntl04
echo "sudo /tmp/cn_cntl_delete.sh ${cntl04_cn_mgr_id} ${cntl04_cn_port_num} ${cntl04_cn_host_name} ${vd_id} ${external_host_nqn} ${leg_cnt} ${leg00_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg01_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg02_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg03_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg04_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg05_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg06_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg07_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg08_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg09_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg10_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg11_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg12_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg13_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg14_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg15_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg16_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg17_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg18_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg19_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg20_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg21_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg22_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg23_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg24_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg25_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg26_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg27_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg28_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg29_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg30_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg31_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id}" | ssh ${cn_server_ip_E}
# delete cntl05
echo "sudo /tmp/cn_cntl_delete.sh ${cntl05_cn_mgr_id} ${cntl05_cn_port_num} ${cntl05_cn_host_name} ${vd_id} ${external_host_nqn} ${leg_cnt} ${leg00_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg01_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg02_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg03_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg04_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg05_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg06_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg07_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg08_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg09_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg10_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg11_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg12_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg13_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg14_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg15_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg16_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg17_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg18_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg19_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg20_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg21_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg22_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg23_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg24_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg25_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg26_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg27_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg28_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg29_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg30_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg31_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id}" | ssh ${cn_server_ip_F}
# delete cntl06
echo "sudo /tmp/cn_cntl_delete.sh ${cntl06_cn_mgr_id} ${cntl06_cn_port_num} ${cntl06_cn_host_name} ${vd_id} ${external_host_nqn} ${leg_cnt} ${leg00_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg01_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg02_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg03_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg04_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg05_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg06_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg07_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg08_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg09_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg10_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg11_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg12_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg13_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg14_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg15_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg16_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg17_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg18_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg19_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg20_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg21_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg22_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg23_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg24_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg25_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg26_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg27_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg28_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg29_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg30_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg31_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id}" | ssh ${cn_server_ip_G}
# delete cntl07
echo "sudo /tmp/cn_cntl_delete.sh ${cntl07_cn_mgr_id} ${cntl07_cn_port_num} ${cntl07_cn_host_name} ${vd_id} ${external_host_nqn} ${leg_cnt} ${leg00_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg01_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg02_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg03_id} ${cntl00_cn_mgr_id} ${cntl00_cn_tr_addr} ${cntl00_cn_tr_svc_id} ${leg04_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg05_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg06_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg07_id} ${cntl01_cn_mgr_id} ${cntl01_cn_tr_addr} ${cntl01_cn_tr_svc_id} ${leg08_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg09_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg10_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg11_id} ${cntl02_cn_mgr_id} ${cntl02_cn_tr_addr} ${cntl02_cn_tr_svc_id} ${leg12_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg13_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg14_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg15_id} ${cntl03_cn_mgr_id} ${cntl03_cn_tr_addr} ${cntl03_cn_tr_svc_id} ${leg16_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg17_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg18_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg19_id} ${cntl04_cn_mgr_id} ${cntl04_cn_tr_addr} ${cntl04_cn_tr_svc_id} ${leg20_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg21_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg22_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg23_id} ${cntl05_cn_mgr_id} ${cntl05_cn_tr_addr} ${cntl05_cn_tr_svc_id} ${leg24_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg25_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg26_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg27_id} ${cntl06_cn_mgr_id} ${cntl06_cn_tr_addr} ${cntl06_cn_tr_svc_id} ${leg28_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg29_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg30_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id} ${leg31_id} ${cntl07_cn_mgr_id} ${cntl07_cn_tr_addr} ${cntl07_cn_tr_svc_id}" | ssh ${cn_server_ip_H}

sudo nvme connect --nqn ${vd_nqn} --transport tcp --traddr ${cntl00_cn_tr_addr} --trsvcid ${cntl00_cn_tr_svc_id} --hostnqn ${external_host_nqn}
sudo nvme connect --nqn ${vd_nqn} --transport tcp --traddr ${cntl01_cn_tr_addr} --trsvcid ${cntl01_cn_tr_svc_id} --hostnqn ${external_host_nqn}
sudo nvme connect --nqn ${vd_nqn} --transport tcp --traddr ${cntl02_cn_tr_addr} --trsvcid ${cntl00_cn_tr_svc_id} --hostnqn ${external_host_nqn}
sudo nvme connect --nqn ${vd_nqn} --transport tcp --traddr ${cntl03_cn_tr_addr} --trsvcid ${cntl01_cn_tr_svc_id} --hostnqn ${external_host_nqn}
sudo nvme connect --nqn ${vd_nqn} --transport tcp --traddr ${cntl04_cn_tr_addr} --trsvcid ${cntl04_cn_tr_svc_id} --hostnqn ${external_host_nqn}
sudo nvme connect --nqn ${vd_nqn} --transport tcp --traddr ${cntl05_cn_tr_addr} --trsvcid ${cntl05_cn_tr_svc_id} --hostnqn ${external_host_nqn}
sudo nvme connect --nqn ${vd_nqn} --transport tcp --traddr ${cntl06_cn_tr_addr} --trsvcid ${cntl06_cn_tr_svc_id} --hostnqn ${external_host_nqn}
sudo nvme connect --nqn ${vd_nqn} --transport tcp --traddr ${cntl07_cn_tr_addr} --trsvcid ${cntl07_cn_tr_svc_id} --hostnqn ${external_host_nqn}

subsys_name=$(sudo nvme list-subsys --output-format json | jq -rM ".Subsystems[] | select(.NQN==\"${vd_nqn}\") | .Name")
sudo bash -c "echo round-robin > /sys/class/nvme-subsystem/${subsys_name}/iopolicy"

sudo parted -s "${dev_path}" unit s print
sudo fio --filename=${dev_path} --name mytest --rw=randwrite --ioengine=libaio --iodepth=11 --bs=4k --numjobs=162 --direct=1 --time_based --runtime=60 --group_reporting --norandommap
# mytest: (g=0): rw=randwrite, bs=(R) 4096B-4096B, (W) 4096B-4096B, (T) 4096B-4096B, ioengine=libaio, iodepth=11
# ...
# fio-3.32
# Starting 162 processes
# Jobs: 162 (f=162): [w(162)][100.0%][w=7781MiB/s][w=1992k IOPS][eta 00m:00s]
# mytest: (groupid=0, jobs=162): err= 0: pid=11803: Fri Feb 23 23:17:59 2024
#   write: IOPS=2005k, BW=7833MiB/s (8214MB/s)(459GiB/60002msec); 0 zone resets
#     slat (nsec): min=1827, max=25391k, avg=15932.93, stdev=29723.72
#     clat (usec): min=17, max=87850, avg=870.74, stdev=275.77
#      lat (usec): min=211, max=87856, avg=886.67, stdev=275.02
#     clat percentiles (usec):
#      |  1.00th=[  490],  5.00th=[  594], 10.00th=[  652], 20.00th=[  717],
#      | 30.00th=[  766], 40.00th=[  816], 50.00th=[  857], 60.00th=[  898],
#      | 70.00th=[  938], 80.00th=[ 1004], 90.00th=[ 1090], 95.00th=[ 1188],
#      | 99.00th=[ 1450], 99.50th=[ 1631], 99.90th=[ 2311], 99.95th=[ 2933],
#      | 99.99th=[ 8455]
#    bw (  MiB/s): min= 6197, max= 7951, per=100.00%, avg=7843.68, stdev= 0.97, samples=19278
#    iops        : min=1586575, max=2035509, avg=2007931.61, stdev=249.13, samples=19278
#   lat (usec)   : 20=0.01%, 50=0.01%, 100=0.01%, 250=0.01%, 500=1.21%
#   lat (usec)   : 750=24.64%, 1000=54.15%
#   lat (msec)   : 2=19.81%, 4=0.15%, 10=0.02%, 20=0.01%, 50=0.01%
#   lat (msec)   : 100=0.01%
#   cpu          : usr=3.38%, sys=16.44%, ctx=102524716, majf=2, minf=1585
#   IO depths    : 1=0.1%, 2=0.1%, 4=0.1%, 8=100.0%, 16=0.0%, 32=0.0%, >=64=0.0%
#      submit    : 0=0.0%, 4=100.0%, 8=0.0%, 16=0.0%, 32=0.0%, 64=0.0%, >=64=0.0%
#      complete  : 0=0.0%, 4=100.0%, 8=0.0%, 16=0.1%, 32=0.0%, 64=0.0%, >=64=0.0%
#      issued rwts: total=0,120320038,0,0 short=0,0,0,0 dropped=0,0,0,0
#      latency   : target=0, window=0, percentile=100.00%, depth=11

# Run status group 0 (all jobs):
#   WRITE: bw=7833MiB/s (8214MB/s), 7833MiB/s-7833MiB/s (8214MB/s-8214MB/s), io=459GiB (493GB), run=60002-60002msec


sudo fio --filename=${dev_path} --name mytest --rw=randread --ioengine=libaio --iodepth=12 --bs=4k --numjobs=162 --direct=1 --time_based --runtime=60 --group_reporting --norandommap
# mytest: (g=0): rw=randread, bs=(R) 4096B-4096B, (W) 4096B-4096B, (T) 4096B-4096B, ioengine=libaio, iodepth=12
# ...
# fio-3.32
# Starting 162 processes
# Jobs: 162 (f=162): [r(162)][100.0%][r=8148MiB/s][r=2086k IOPS][eta 00m:00s]
# mytest: (groupid=0, jobs=162): err= 0: pid=12723: Fri Feb 23 23:20:39 2024
#   read: IOPS=2083k, BW=8137MiB/s (8532MB/s)(477GiB/60003msec)
#     slat (nsec): min=1608, max=7634.2k, avg=17031.59, stdev=39355.27
#     clat (nsec): min=1563, max=25011k, avg=914007.61, stdev=203981.28
#      lat (usec): min=274, max=25016, avg=931.04, stdev=201.74
#     clat percentiles (usec):
#      |  1.00th=[  529],  5.00th=[  635], 10.00th=[  693], 20.00th=[  758],
#      | 30.00th=[  807], 40.00th=[  848], 50.00th=[  898], 60.00th=[  938],
#      | 70.00th=[  988], 80.00th=[ 1057], 90.00th=[ 1156], 95.00th=[ 1254],
#      | 99.00th=[ 1500], 99.50th=[ 1631], 99.90th=[ 1958], 99.95th=[ 2147],
#      | 99.99th=[ 2868]
#    bw (  MiB/s): min= 7975, max= 8298, per=100.00%, avg=8143.49, stdev= 0.40, samples=19278
#    iops        : min=2041581, max=2124246, avg=2084706.76, stdev=101.91, samples=19278
#   lat (usec)   : 2=0.01%, 4=0.01%, 20=0.01%, 50=0.01%, 100=0.01%
#   lat (usec)   : 250=0.01%, 500=0.53%, 750=18.12%, 1000=52.91%
#   lat (msec)   : 2=28.33%, 4=0.08%, 10=0.01%, 20=0.01%, 50=0.01%
#   cpu          : usr=3.13%, sys=16.20%, ctx=95976370, majf=0, minf=3387
#   IO depths    : 1=0.1%, 2=0.1%, 4=0.1%, 8=100.0%, 16=0.0%, 32=0.0%, >=64=0.0%
#      submit    : 0=0.0%, 4=100.0%, 8=0.0%, 16=0.0%, 32=0.0%, 64=0.0%, >=64=0.0%
#      complete  : 0=0.0%, 4=100.0%, 8=0.0%, 16=0.1%, 32=0.0%, 64=0.0%, >=64=0.0%
#      issued rwts: total=124989625,0,0,0 short=0,0,0,0 dropped=0,0,0,0
#      latency   : target=0, window=0, percentile=100.00%, depth=12

# Run status group 0 (all jobs):
#    READ: bw=8137MiB/s (8532MB/s), 8137MiB/s-8137MiB/s (8532MB/s-8532MB/s), io=477GiB (512GB), run=60003-60003msec


sudo nvme disconnect --nqn ${vd_nqn}
