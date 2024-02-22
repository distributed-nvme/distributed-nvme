#!/bin/bash

server_ip=127.0.0.1
host_name="host_A"
leg_cnt=1
cntl_cnt=1

global_id=1

leg0_grp0_ld0_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg0_grp0_ld0_pd_path=/dev/loop240
leg0_grp0_ld0_dn_port_num=1
leg0_grp0_ld0_dn_tr_addr=${server_ip}
leg0_grp0_ld0_dn_tr_svc_id=4410

leg0_grp0_ld1_dn_mgr_id=${global_id} && global_id=$((global_id+1))
leg0_grp0_ld1_pd_path=/dev/loop241
leg0_grp0_ld1_dn_port_num=2
leg0_grp0_ld1_dn_tr_addr=${server_ip}
leg0_grp0_ld1_dn_tr_svc_id=4411

cntl0_cn_mgr_id=${global_id} && global_id=$((global_id+1))
cntl0_cn_host_name=${host_name}
cntl0_cn_port_num=3
cntl0_cn_tr_addr=${server_ip}
cntl0_cn_tr_svc_id=4420
cntl0_cntlid_min=20000
cntl0_cntlid_max=24999

forward_cn_cnt=0

vd_id=${global_id} && global_id=$((global_id+1))
vd_internal_id=1

ld_start_mb=0
ld_size_mb=1024
thin_dev_size_mb=1024

leg0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg0_grp0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg0_grp0_ld0_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))
leg0_grp0_ld1_id=${vd_internal_id} && vd_internal_id=$((vd_internal_id+1))

external_host_nqn="nqn.2014-08.org.nvmexpress:uuid:16c2fe2c-94fd-4a9b-b0d2-fab74d3fb38b"
UUID_NAMESPACE="37833e01-35d4-4e5a-b0a1-fff158b9d03b"
RANDOM_PREFIX=$(echo "${UUID_NAMESPACE}" | tr -d "-")
vd_nqn="nqn.2024-01.io.dnv.${RANDOM_PREFIX}:00000000:$(printf %08x ${vd_id}):1200:00000000"
dev_uuid=$(uuidgen --md5 --namespace "${UUID_NAMESPACE}" --name "${vd_nqn}")
dev_path="/dev/disk/by-id/nvme-uuid.${dev_uuid}"

scp *.sh ${server_ip}:/tmp/

# prepare leg0 grp0 ld0
echo "sudo /tmp/dn_prepare.sh ${leg0_grp0_ld0_dn_port_num} ${leg0_grp0_ld0_dn_tr_addr} ${leg0_grp0_ld0_dn_tr_svc_id}" | ssh ${server_ip}
# prepare leg0 grp0 ld1
echo "sudo /tmp/dn_prepare.sh ${leg0_grp0_ld1_dn_port_num} ${leg0_grp0_ld1_dn_tr_addr} ${leg0_grp0_ld1_dn_tr_svc_id}" | ssh ${server_ip}

# cleanup leg0 grp0 ld0
echo "sudo /tmp/dn_cleanup.sh ${leg0_grp0_ld0_dn_port_num}" | ssh ${server_ip}
# cleanup leg0 grp0 ld1
echo "sudo /tmp/dn_cleanup.sh ${leg0_grp0_ld1_dn_port_num}" | ssh ${server_ip}

# prepare cntl0
echo "sudo /tmp/cn_prepare.sh ${cntl0_cn_port_num} ${cntl0_cn_tr_addr} ${cntl0_cn_tr_svc_id}" | ssh ${server_ip}

# cleanup cntl0
echo "sudo /tmp/cn_cleanup.sh ${cntl0_cn_port_num}" | ssh ${server_ip}

# create leg0 grp0 ld0
echo "sudo /tmp/dn_ld_create.sh ${leg0_grp0_ld0_dn_mgr_id} ${leg0_grp0_ld0_dn_port_num} ${leg0_grp0_ld0_pd_path} ${vd_id} ${leg0_grp0_ld0_id} ${ld_start_mb} ${ld_size_mb} ${cntl0_cn_mgr_id} ${cntl0_cn_host_name}" | ssh ${server_ip}
# create leg0 grp0 ld1
echo "sudo /tmp/dn_ld_create.sh ${leg0_grp0_ld1_dn_mgr_id} ${leg0_grp0_ld1_dn_port_num} ${leg0_grp0_ld1_pd_path} ${vd_id} ${leg0_grp0_ld1_id} ${ld_start_mb} ${ld_size_mb} ${cntl0_cn_mgr_id} ${cntl0_cn_host_name}" | ssh ${server_ip}

# delete leg0 grp0 ld0
echo "sudo /tmp/dn_ld_delete.sh ${leg0_grp0_ld0_dn_mgr_id} ${leg0_grp0_ld0_dn_port_num} ${vd_id} ${leg0_grp0_ld0_id} ${cntl0_cn_mgr_id} ${cntl0_cn_host_name}" | ssh ${server_ip}
# delete leg0 grp0 ld1
echo "sudo /tmp/dn_ld_delete.sh ${leg0_grp0_ld1_dn_mgr_id} ${leg0_grp0_ld1_dn_port_num} ${vd_id} ${leg0_grp0_ld1_id} ${cntl0_cn_mgr_id} ${cntl0_cn_host_name}" | ssh ${server_ip}

# create leg0
echo "sudo /tmp/cn_leg_create.sh ${cntl0_cn_mgr_id} ${cntl0_cn_port_num} ${cntl0_cn_host_name} ${vd_id} ${leg0_id} ${leg0_grp0_id} ${leg0_grp0_ld0_id} ${leg0_grp0_ld0_dn_mgr_id} ${leg0_grp0_ld0_dn_tr_addr} ${leg0_grp0_ld0_dn_tr_svc_id} ${leg0_grp0_ld1_id} ${leg0_grp0_ld1_dn_mgr_id} ${leg0_grp0_ld1_dn_tr_addr} ${leg0_grp0_ld1_dn_tr_svc_id} ${ld_size_mb} ${thin_dev_size_mb} ${forward_cn_cnt}" | ssh ${server_ip}

# delete leg0
echo "sudo /tmp/cn_leg_delete.sh ${cntl0_cn_mgr_id} ${cntl0_cn_port_num} ${cntl0_cn_host_name} ${vd_id} ${leg0_id} ${leg0_grp0_id} ${leg0_grp0_ld0_id} ${leg0_grp0_ld0_dn_mgr_id} ${leg0_grp0_ld0_dn_tr_addr} ${leg0_grp0_ld0_dn_tr_svc_id} ${leg0_grp0_ld1_id} ${leg0_grp0_ld1_dn_mgr_id} ${leg0_grp0_ld1_dn_tr_addr} ${leg0_grp0_ld1_dn_tr_svc_id} ${forward_cn_cnt}" | ssh ${server_ip}

# create cntl0
echo "sudo /tmp/cn_cntl_create.sh ${cntl0_cn_mgr_id} ${cntl0_cn_port_num} ${cntl0_cn_host_name} ${cntl0_cntlid_min} ${cntl0_cntlid_max} ${vd_id} ${external_host_nqn} ${thin_dev_size_mb} ${leg_cnt} ${leg0_id} ${cntl0_cn_mgr_id} ${cntl0_cn_tr_addr} ${cntl0_cn_tr_svc_id}" | ssh ${server_ip}

# delete cntl0
echo "sudo /tmp/cn_cntl_delete.sh ${cntl0_cn_mgr_id} ${cntl0_cn_port_num} ${cntl0_cn_host_name} ${vd_id} ${external_host_nqn} ${leg_cnt} ${leg0_id} ${cntl0_cn_mgr_id} ${cntl0_cn_tr_addr} ${cntl0_cn_tr_svc_id}" | ssh ${server_ip}

sudo nvme connect --nqn ${vd_nqn} --transport tcp --traddr ${cntl0_cn_tr_addr} --trsvcid ${cntl0_cn_tr_svc_id} --hostnqn ${external_host_nqn}

sudo parted -s "${dev_path}" unit s print

sudo nvme disconnect --nqn ${vd_nqn}
