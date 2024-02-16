#!/bin/bash

scp layer*.sh $server_A_ip:/tmp/ && scp common.sh $server_A_ip:/tmp/ && scp layer*.sh $server_B_ip:/tmp/ && scp common.sh $server_B_ip:/tmp/

# create disks
echo 'for i in $(seq 4); do dd if=/dev/zero of=/tmp/disk${i}.img bs=1M count=1024 && sudo losetup /dev/loop24${i} /tmp/disk${i}.img; done' | ssh ${server_A_ip}
echo 'for i in $(seq 4); do dd if=/dev/zero of=/tmp/disk${i}.img bs=1M count=1024 && sudo losetup /dev/loop24${i} /tmp/disk${i}.img; done' | ssh ${server_B_ip}

# delete disks
echo 'for i in $(seq 4); do sudo losetup -d /dev/loop24${i} && rm /tmp/disk${i}.img; done' | ssh ${server_A_ip}
echo 'for i in $(seq 4); do sudo losetup -d /dev/loop24${i} && rm /tmp/disk${i}.img; done' | ssh ${server_B_ip}

# prepare l1 stripe0 thinmeta grp0 leg0
echo "sudo /tmp/layer1_node_prepare.sh $stripe0_thinmeta_grp0_leg0_l1_mgr_id $stripe0_thinmeta_grp0_leg0_l1_port_num $stripe0_thinmeta_grp0_leg0_l1_tr_addr $stripe0_thinmeta_grp0_leg0_l1_tr_svc_id $stripe0_thinmeta_grp0_leg0_l1_pv_path" | ssh ${server_A_ip}
# prepare l1 stripe0 thinmeta grp0 leg1
echo "sudo /tmp/layer1_node_prepare.sh $stripe0_thinmeta_grp0_leg1_l1_mgr_id $stripe0_thinmeta_grp0_leg1_l1_port_num $stripe0_thinmeta_grp0_leg1_l1_tr_addr $stripe0_thinmeta_grp0_leg1_l1_tr_svc_id $stripe0_thinmeta_grp0_leg1_l1_pv_path" | ssh ${server_B_ip}
# prepare l1 stripe0 thindata grp0 leg0
echo "sudo /tmp/layer1_node_prepare.sh $stripe0_thindata_grp0_leg0_l1_mgr_id $stripe0_thindata_grp0_leg0_l1_port_num $stripe0_thindata_grp0_leg0_l1_tr_addr $stripe0_thindata_grp0_leg0_l1_tr_svc_id $stripe0_thindata_grp0_leg0_l1_pv_path" | ssh ${server_A_ip}
# prepare l1 stripe0 thindata grp0 leg1
echo "sudo /tmp/layer1_node_prepare.sh $stripe0_thindata_grp0_leg1_l1_mgr_id $stripe0_thindata_grp0_leg1_l1_port_num $stripe0_thindata_grp0_leg1_l1_tr_addr $stripe0_thindata_grp0_leg1_l1_tr_svc_id $stripe0_thindata_grp0_leg1_l1_pv_path" | ssh ${server_B_ip}
# prepare l1 stripe1 thinmeta grp0 leg0
echo "sudo /tmp/layer1_node_prepare.sh $stripe1_thinmeta_grp0_leg0_l1_mgr_id $stripe1_thinmeta_grp0_leg0_l1_port_num $stripe1_thinmeta_grp0_leg0_l1_tr_addr $stripe1_thinmeta_grp0_leg0_l1_tr_svc_id $stripe1_thinmeta_grp0_leg0_l1_pv_path" | ssh ${server_A_ip}
# prepare l1 stripe1 thinmeta grp0 leg1
echo "sudo /tmp/layer1_node_prepare.sh $stripe1_thinmeta_grp0_leg1_l1_mgr_id $stripe1_thinmeta_grp0_leg1_l1_port_num $stripe1_thinmeta_grp0_leg1_l1_tr_addr $stripe1_thinmeta_grp0_leg1_l1_tr_svc_id $stripe1_thinmeta_grp0_leg1_l1_pv_path" | ssh ${server_B_ip}
# prepare l1 stripe1 thindata grp0 leg0
echo "sudo /tmp/layer1_node_prepare.sh $stripe1_thindata_grp0_leg0_l1_mgr_id $stripe1_thindata_grp0_leg0_l1_port_num $stripe1_thindata_grp0_leg0_l1_tr_addr $stripe1_thindata_grp0_leg0_l1_tr_svc_id $stripe1_thindata_grp0_leg0_l1_pv_path" | ssh ${server_A_ip}
# prepare l1 stripe1 thindata grp0 leg1
echo "sudo /tmp/layer1_node_prepare.sh $stripe1_thindata_grp0_leg1_l1_mgr_id $stripe1_thindata_grp0_leg1_l1_port_num $stripe1_thindata_grp0_leg1_l1_tr_addr $stripe1_thindata_grp0_leg1_l1_tr_svc_id $stripe1_thindata_grp0_leg1_l1_pv_path" | ssh ${server_B_ip}

# cleanup l1 stripe0 thinmeta grp0 leg0
echo "sudo /tmp/layer1_node_cleanup.sh $stripe0_thinmeta_grp0_leg0_l1_mgr_id $stripe0_thinmeta_grp0_leg0_l1_port_num $stripe0_thinmeta_grp0_leg0_l1_pv_path" | ssh ${server_A_ip}
# cleanup l1 stripe0 thinmeta grp0 leg1
echo "sudo /tmp/layer1_node_cleanup.sh $stripe0_thinmeta_grp0_leg1_l1_mgr_id $stripe0_thinmeta_grp0_leg1_l1_port_num $stripe0_thinmeta_grp0_leg1_l1_pv_path" | ssh ${server_B_ip}
# cleanup l1 stripe0 thindata grp0 leg0
echo "sudo /tmp/layer1_node_cleanup.sh $stripe0_thindata_grp0_leg0_l1_mgr_id $stripe0_thindata_grp0_leg0_l1_port_num $stripe0_thindata_grp0_leg0_l1_pv_path" | ssh ${server_A_ip}
# cleanup l1 stripe0 thindata grp0 leg1
echo "sudo /tmp/layer1_node_cleanup.sh $stripe0_thindata_grp0_leg1_l1_mgr_id $stripe0_thindata_grp0_leg1_l1_port_num $stripe0_thindata_grp0_leg1_l1_pv_path" | ssh ${server_B_ip}
# cleanup l1 stripe1 thinmeta grp0 leg0
echo "sudo /tmp/layer1_node_cleanup.sh $stripe1_thinmeta_grp0_leg0_l1_mgr_id $stripe1_thinmeta_grp0_leg0_l1_port_num $stripe1_thinmeta_grp0_leg0_l1_pv_path" | ssh ${server_A_ip}
# cleanup l1 stripe1 thinmeta grp0 leg1
echo "sudo /tmp/layer1_node_cleanup.sh $stripe1_thinmeta_grp0_leg1_l1_mgr_id $stripe1_thinmeta_grp0_leg1_l1_port_num $stripe1_thinmeta_grp0_leg1_l1_pv_path" | ssh ${server_B_ip}
# cleanup l1 stripe1 thindata grp0 leg0
echo "sudo /tmp/layer1_node_cleanup.sh $stripe1_thindata_grp0_leg0_l1_mgr_id $stripe1_thindata_grp0_leg0_l1_port_num $stripe1_thindata_grp0_leg0_l1_pv_path" | ssh ${server_A_ip}
# cleanup l1 stripe1 thindata grp0 leg1
echo "sudo /tmp/layer1_node_cleanup.sh $stripe1_thindata_grp0_leg1_l1_mgr_id $stripe1_thindata_grp0_leg1_l1_port_num $stripe1_thindata_grp0_leg1_l1_pv_path" | ssh ${server_B_ip}

# create l1 vd stripe0 thinmeta grp0 leg0
echo "sudo /tmp/layer1_vd_leg_create.sh $stripe0_thinmeta_grp0_leg0_l1_mgr_id $stripe0_thinmeta_grp0_leg0_l1_port_num $vd_id $stripe0_thinmeta_grp0_leg0_id $stripe0_l2_prim_mgr_id $host_name_A $thinmeta_leg_mb" | ssh ${server_A_ip}
# create l1 vd stripe0 thinmeta grp0 leg1
echo "sudo /tmp/layer1_vd_leg_create.sh $stripe0_thinmeta_grp0_leg1_l1_mgr_id $stripe0_thinmeta_grp0_leg1_l1_port_num $vd_id $stripe0_thinmeta_grp0_leg1_id $stripe0_l2_prim_mgr_id $host_name_A $thinmeta_leg_mb" | ssh ${server_B_ip}
# create l1 vd stripe0 thindata grp0 leg0
echo "sudo /tmp/layer1_vd_leg_create.sh $stripe0_thindata_grp0_leg0_l1_mgr_id $stripe0_thindata_grp0_leg0_l1_port_num $vd_id $stripe0_thindata_grp0_leg0_id $stripe0_l2_prim_mgr_id $host_name_A $thindata_leg_mb" | ssh ${server_A_ip}
# create l1 vd stripe0 thindata grp0 leg1
echo "sudo /tmp/layer1_vd_leg_create.sh $stripe0_thindata_grp0_leg1_l1_mgr_id $stripe0_thindata_grp0_leg1_l1_port_num $vd_id $stripe0_thindata_grp0_leg1_id $stripe0_l2_prim_mgr_id $host_name_A $thindata_leg_mb" | ssh ${server_B_ip}
# create l1 vd stripe1 thinmeta grp0 leg0
echo "sudo /tmp/layer1_vd_leg_create.sh $stripe1_thinmeta_grp0_leg0_l1_mgr_id $stripe1_thinmeta_grp0_leg0_l1_port_num $vd_id $stripe1_thinmeta_grp0_leg0_id $stripe1_l2_prim_mgr_id $host_name_B $thinmeta_leg_mb" | ssh ${server_A_ip}
# create l1 vd stripe1 thinmeta grp0 leg1
echo "sudo /tmp/layer1_vd_leg_create.sh $stripe1_thinmeta_grp0_leg1_l1_mgr_id $stripe1_thinmeta_grp0_leg1_l1_port_num $vd_id $stripe1_thinmeta_grp0_leg1_id $stripe1_l2_prim_mgr_id $host_name_B $thinmeta_leg_mb" | ssh ${server_B_ip}
# create l1 vd stripe1 thindata grp0 leg0
echo "sudo /tmp/layer1_vd_leg_create.sh $stripe1_thindata_grp0_leg0_l1_mgr_id $stripe1_thindata_grp0_leg0_l1_port_num $vd_id $stripe1_thindata_grp0_leg0_id $stripe1_l2_prim_mgr_id $host_name_B $thindata_leg_mb" | ssh ${server_A_ip}
# create l1 vd stripe1 thindata grp0 leg1
echo "sudo /tmp/layer1_vd_leg_create.sh $stripe1_thindata_grp0_leg1_l1_mgr_id $stripe1_thindata_grp0_leg1_l1_port_num $vd_id $stripe1_thindata_grp0_leg1_id $stripe1_l2_prim_mgr_id $host_name_B $thindata_leg_mb" | ssh ${server_B_ip}

# delete l1 vd stripe0 thinmeta grp0 leg0
echo "sudo /tmp/layer1_vd_leg_delete.sh $stripe0_thinmeta_grp0_leg0_l1_mgr_id $stripe0_thinmeta_grp0_leg0_l1_port_num $vd_id $stripe0_thinmeta_grp0_leg0_id $stripe0_l2_prim_mgr_id $host_name_A" | ssh ${server_A_ip}
# delete l1 vd stripe0 thinmeta grp0 leg1
echo "sudo /tmp/layer1_vd_leg_delete.sh $stripe0_thinmeta_grp0_leg1_l1_mgr_id $stripe0_thinmeta_grp0_leg1_l1_port_num $vd_id $stripe0_thinmeta_grp0_leg1_id $stripe0_l2_prim_mgr_id $host_name_A" | ssh ${server_B_ip}
# delete l1 vd stripe0 thindata grp0 leg0
echo "sudo /tmp/layer1_vd_leg_delete.sh $stripe0_thindata_grp0_leg0_l1_mgr_id $stripe0_thindata_grp0_leg0_l1_port_num $vd_id $stripe0_thindata_grp0_leg0_id $stripe0_l2_prim_mgr_id $host_name_A" | ssh ${server_A_ip}
# delete l1 vd stripe0 thindata grp0 leg1
echo "sudo /tmp/layer1_vd_leg_delete.sh $stripe0_thindata_grp0_leg1_l1_mgr_id $stripe0_thindata_grp0_leg1_l1_port_num $vd_id $stripe0_thindata_grp0_leg1_id $stripe0_l2_prim_mgr_id $host_name_A" | ssh ${server_B_ip}
# delete l1 vd stripe1 thinmeta grp0 leg0
echo "sudo /tmp/layer1_vd_leg_delete.sh $stripe1_thinmeta_grp0_leg0_l1_mgr_id $stripe1_thinmeta_grp0_leg0_l1_port_num $vd_id $stripe1_thinmeta_grp0_leg0_id $stripe1_l2_prim_mgr_id $host_name_B" | ssh ${server_A_ip}
# delete l1 vd stripe1 thinmeta grp0 leg1
echo "sudo /tmp/layer1_vd_leg_delete.sh $stripe1_thinmeta_grp0_leg1_l1_mgr_id $stripe1_thinmeta_grp0_leg1_l1_port_num $vd_id $stripe1_thinmeta_grp0_leg1_id $stripe1_l2_prim_mgr_id $host_name_B" | ssh ${server_B_ip}
# delete l1 vd stripe1 thindata grp0 leg0
echo "sudo /tmp/layer1_vd_leg_delete.sh $stripe1_thindata_grp0_leg0_l1_mgr_id $stripe1_thindata_grp0_leg0_l1_port_num $vd_id $stripe1_thindata_grp0_leg0_id $stripe1_l2_prim_mgr_id $host_name_B" | ssh ${server_A_ip}
# delete l1 vd stripe1 thindata grp0 leg1
echo "sudo /tmp/layer1_vd_leg_delete.sh $stripe1_thindata_grp0_leg1_l1_mgr_id $stripe1_thindata_grp0_leg1_l1_port_num $vd_id $stripe1_thindata_grp0_leg1_id $stripe1_l2_prim_mgr_id $host_name_B" | ssh ${server_B_ip}

# prepare l2 stripe0 prim
echo "sudo /tmp/layer2_node_prepare.sh $stripe0_l2_prim_port_num $stripe0_l2_prim_tr_addr $stripe0_l2_prim_tr_svc_id" | ssh ${server_A_ip}
# prepare l2 stripe0 sec0
echo "sudo /tmp/layer2_node_prepare.sh $stripe0_l2_sec0_port_num $stripe0_l2_sec0_tr_addr $stripe0_l2_sec0_tr_svc_id" | ssh ${server_B_ip}
# prepare l2 stripe1 prim
echo "sudo /tmp/layer2_node_prepare.sh $stripe1_l2_prim_port_num $stripe1_l2_prim_tr_addr $stripe1_l2_prim_tr_svc_id" | ssh ${server_B_ip}
# prepare l2 stripe1 sec0
echo "sudo /tmp/layer2_node_prepare.sh $stripe1_l2_sec0_port_num $stripe1_l2_sec0_tr_addr $stripe1_l2_sec0_tr_svc_id" | ssh ${server_A_ip}

# cleanup l2 stripe0 prim
echo "sudo /tmp/layer2_node_cleanup.sh $stripe0_l2_prim_port_num" | ssh ${server_A_ip}
# cleanup l2 stripe0 sec0
echo "sudo /tmp/layer2_node_cleanup.sh $stripe0_l2_sec0_port_num" | ssh ${server_B_ip}
# cleanup l2 stripe1 prim
echo "sudo /tmp/layer2_node_cleanup.sh $stripe1_l2_prim_port_num" | ssh ${server_B_ip}
# cleanup l2 stripe1 sec0
echo "sudo /tmp/layer2_node_cleanup.sh $stripe1_l2_sec0_port_num" | ssh ${server_A_ip}

# create l2 vd stripe0 prim
echo "sudo /tmp/layer2_vd_primary_create.sh $stripe0_l2_prim_mgr_id $stripe0_l2_prim_port_num $host_name_A $vd_id $stripe0_id $stripe0_thinmeta_grp0_id $stripe0_thinmeta_grp0_leg0_id $stripe0_thinmeta_grp0_leg0_l1_mgr_id $stripe0_thinmeta_grp0_leg0_l1_tr_addr $stripe0_thinmeta_grp0_leg0_l1_tr_svc_id $stripe0_thinmeta_grp0_leg1_id $stripe0_thinmeta_grp0_leg1_l1_mgr_id $stripe0_thinmeta_grp0_leg1_l1_tr_addr $stripe0_thinmeta_grp0_leg1_l1_tr_svc_id $stripe0_thindata_grp0_id $stripe0_thindata_grp0_leg0_id $stripe0_thindata_grp0_leg0_l1_mgr_id $stripe0_thindata_grp0_leg0_l1_tr_addr $stripe0_thindata_grp0_leg0_l1_tr_svc_id $stripe0_thindata_grp0_leg1_id $stripe0_thindata_grp0_leg1_l1_mgr_id $stripe0_thindata_grp0_leg1_l1_tr_addr $stripe0_thindata_grp0_leg1_l1_tr_svc_id $thinmeta_raid1meta_mb $thinmeta_raid1data_mb $thindata_raid1meta_mb $thindata_raid1data_mb $thindev_mb $stripe0_l2_sec0_mgr_id $host_name_B $cntlr_cnt $l3_cntlr0_mgr_id $host_name_A $l3_cntlr1_mgr_id $host_name_B" | ssh ${server_A_ip}
# create l2 vd stripe1 prim
echo "sudo /tmp/layer2_vd_primary_create.sh $stripe1_l2_prim_mgr_id $stripe1_l2_prim_port_num $host_name_B $vd_id $stripe1_id $stripe1_thinmeta_grp0_id $stripe1_thinmeta_grp0_leg0_id $stripe1_thinmeta_grp0_leg0_l1_mgr_id $stripe1_thinmeta_grp0_leg0_l1_tr_addr $stripe1_thinmeta_grp0_leg0_l1_tr_svc_id $stripe1_thinmeta_grp0_leg1_id $stripe1_thinmeta_grp0_leg1_l1_mgr_id $stripe1_thinmeta_grp0_leg1_l1_tr_addr $stripe1_thinmeta_grp0_leg1_l1_tr_svc_id $stripe1_thindata_grp0_id $stripe1_thindata_grp0_leg0_id $stripe1_thindata_grp0_leg0_l1_mgr_id $stripe1_thindata_grp0_leg0_l1_tr_addr $stripe1_thindata_grp0_leg0_l1_tr_svc_id $stripe1_thindata_grp0_leg1_id $stripe1_thindata_grp0_leg1_l1_mgr_id $stripe1_thindata_grp0_leg1_l1_tr_addr $stripe1_thindata_grp0_leg1_l1_tr_svc_id $thinmeta_raid1meta_mb $thinmeta_raid1data_mb $thindata_raid1meta_mb $thindata_raid1data_mb $thindev_mb $stripe1_l2_sec0_mgr_id $host_name_A $cntlr_cnt $l3_cntlr0_mgr_id $host_name_A $l3_cntlr1_mgr_id $host_name_B" | ssh ${server_B_ip}
# create l2 vd stripe0 sec0
echo "sudo /tmp/layer2_vd_secondary_create.sh $stripe0_l2_sec0_mgr_id $stripe0_l2_sec0_port_num $host_name_B $vd_id $stripe0_id $stripe0_l2_prim_mgr_id $stripe0_l2_prim_tr_addr $stripe0_l2_prim_tr_svc_id $thindev_mb $cntlr_cnt $l3_cntlr0_mgr_id $host_name_A $l3_cntlr1_mgr_id $host_name_B" | ssh ${server_B_ip}
# create l2 vd stripe1 sec0
echo "sudo /tmp/layer2_vd_secondary_create.sh $stripe1_l2_sec0_mgr_id $stripe1_l2_sec0_port_num $host_name_A $vd_id $stripe1_id $stripe1_l2_prim_mgr_id $stripe1_l2_prim_tr_addr $stripe1_l2_prim_tr_svc_id $thindev_mb $cntlr_cnt $l3_cntlr0_mgr_id $host_name_A $l3_cntlr1_mgr_id $host_name_B" | ssh ${server_A_ip}

# delete l2 vd stripe0 sec0
echo "sudo /tmp/layer2_vd_secondary_delete.sh $stripe0_l2_sec0_mgr_id $stripe0_l2_sec0_port_num $vd_id $stripe0_id $stripe0_l2_prim_mgr_id $stripe0_l2_prim_tr_addr $stripe0_l2_prim_tr_svc_id $cntlr_cnt $l3_cntlr0_mgr_id $host_name_A $l3_cntlr1_mgr_id $host_name_B" | ssh ${server_B_ip}
# delete l2 vd stripe1 sec0
echo "sudo /tmp/layer2_vd_secondary_delete.sh $stripe1_l2_sec0_mgr_id $stripe1_l2_sec0_port_num $vd_id $stripe1_id $stripe1_l2_prim_mgr_id $stripe1_l2_prim_tr_addr $stripe1_l2_prim_tr_svc_id $cntlr_cnt $l3_cntlr0_mgr_id $host_name_A $l3_cntlr1_mgr_id $host_name_B" | ssh ${server_A_ip}
# delete l2 vd stripe0 prim
echo "sudo /tmp/layer2_vd_primary_delete.sh $stripe0_l2_prim_mgr_id $stripe0_l2_prim_port_num $host_name_A $vd_id $stripe0_id $stripe0_thinmeta_grp0_id $stripe0_thinmeta_grp0_leg0_id $stripe0_thinmeta_grp0_leg0_l1_mgr_id $stripe0_thinmeta_grp0_leg0_l1_tr_addr $stripe0_thinmeta_grp0_leg0_l1_tr_svc_id $stripe0_thinmeta_grp0_leg1_id $stripe0_thinmeta_grp0_leg1_l1_mgr_id $stripe0_thinmeta_grp0_leg1_l1_tr_addr $stripe0_thinmeta_grp0_leg1_l1_tr_svc_id $stripe0_thindata_grp0_id $stripe0_thindata_grp0_leg0_id $stripe0_thindata_grp0_leg0_l1_mgr_id $stripe0_thindata_grp0_leg0_l1_tr_addr $stripe0_thindata_grp0_leg0_l1_tr_svc_id $stripe0_thindata_grp0_leg1_id $stripe0_thindata_grp0_leg1_l1_mgr_id $stripe0_thindata_grp0_leg1_l1_tr_addr $stripe0_thindata_grp0_leg1_l1_tr_svc_id $stripe0_l2_sec0_mgr_id $host_name_B $cntlr_cnt $l3_cntlr0_mgr_id $host_name_A $l3_cntlr1_mgr_id $host_name_B" | ssh ${server_A_ip}
# delete l2 vd stripe1 prim
echo "sudo /tmp/layer2_vd_primary_delete.sh $stripe1_l2_prim_mgr_id $stripe1_l2_prim_port_num $host_name_B $vd_id $stripe1_id $stripe1_thinmeta_grp0_id $stripe1_thinmeta_grp0_leg0_id $stripe1_thinmeta_grp0_leg0_l1_mgr_id $stripe1_thinmeta_grp0_leg0_l1_tr_addr $stripe1_thinmeta_grp0_leg0_l1_tr_svc_id $stripe1_thinmeta_grp0_leg1_id $stripe1_thinmeta_grp0_leg1_l1_mgr_id $stripe1_thinmeta_grp0_leg1_l1_tr_addr $stripe1_thinmeta_grp0_leg1_l1_tr_svc_id $stripe1_thindata_grp0_id $stripe1_thindata_grp0_leg0_id $stripe1_thindata_grp0_leg0_l1_mgr_id $stripe1_thindata_grp0_leg0_l1_tr_addr $stripe1_thindata_grp0_leg0_l1_tr_svc_id $stripe1_thindata_grp0_leg1_id $stripe1_thindata_grp0_leg1_l1_mgr_id $stripe1_thindata_grp0_leg1_l1_tr_addr $stripe1_thindata_grp0_leg1_l1_tr_svc_id $stripe1_l2_sec0_mgr_id $host_name_A $cntlr_cnt $l3_cntlr0_mgr_id $host_name_A $l3_cntlr1_mgr_id $host_name_B" | ssh ${server_B_ip}

# prepare l3 cntlr0
echo "sudo /tmp/layer3_node_prepare.sh $l3_cntlr0_port_num $l3_cntlr0_tr_addr $l3_cntlr0_tr_svc_id" | ssh ${server_A_ip}
# prepare l3 cntlr1
echo "sudo /tmp/layer3_node_prepare.sh $l3_cntlr1_port_num $l3_cntlr1_tr_addr $l3_cntlr1_tr_svc_id" | ssh ${server_B_ip}

# cleanup l3 cntlr0
echo "sudo /tmp/layer3_node_cleanup.sh $l3_cntlr0_port_num" | ssh ${server_A_ip}
# cleanup l3 cntlr1
echo "sudo /tmp/layer3_node_cleanup.sh $l3_cntlr1_port_num" | ssh ${server_B_ip}

# create l3 cntlr0
echo "sudo /tmp/layer3_vd_cntlr_create.sh $l3_cntlr0_mgr_id $l3_cntlr0_port_num $host_name_A $vd_id $external_host_nqn $l3_cntlr0_cntlid_min $l3_cntlr0_cntlid_max $thindev_mb $stripe_cnt $stripe0_id $stripe0_l2_prim_tr_addr $stripe0_l2_prim_tr_svc_id $stripe0_l2_sec0_tr_addr $stripe0_l2_sec0_tr_svc_id $stripe1_id $stripe1_l2_prim_tr_addr $stripe1_l2_prim_tr_svc_id $stripe1_l2_sec0_tr_addr $stripe1_l2_sec0_tr_svc_id" | ssh ${server_A_ip}
# create l3 cntlr1
echo "sudo /tmp/layer3_vd_cntlr_create.sh $l3_cntlr1_mgr_id $l3_cntlr1_port_num $host_name_B $vd_id $external_host_nqn $l3_cntlr1_cntlid_min $l3_cntlr1_cntlid_max $thindev_mb $stripe_cnt $stripe0_id $stripe0_l2_prim_tr_addr $stripe0_l2_prim_tr_svc_id $stripe0_l2_sec0_tr_addr $stripe0_l2_sec0_tr_svc_id $stripe1_id $stripe1_l2_prim_tr_addr $stripe1_l2_prim_tr_svc_id $stripe1_l2_sec0_tr_addr $stripe1_l2_sec0_tr_svc_id" | ssh ${server_B_ip}

# delete l3 cntlr0
echo "sudo /tmp/layer3_vd_cntlr_delete.sh $l3_cntlr0_mgr_id $l3_cntlr0_port_num $vd_id $external_host_nqn $stripe_cnt $stripe0_id $stripe0_l2_prim_tr_addr $stripe0_l2_prim_tr_svc_id $stripe0_l2_sec0_tr_addr $stripe0_l2_sec0_tr_svc_id $stripe1_id $stripe1_l2_prim_tr_addr $stripe1_l2_prim_tr_svc_id $stripe1_l2_sec0_tr_addr $stripe1_l2_sec0_tr_svc_id" | ssh ${server_A_ip}
# delete l3 cntlr1
echo "sudo /tmp/layer3_vd_cntlr_delete.sh $l3_cntlr1_mgr_id $l3_cntlr1_port_num $vd_id $external_host_nqn $stripe_cnt $stripe0_id $stripe0_l2_prim_tr_addr $stripe0_l2_prim_tr_svc_id $stripe0_l2_sec0_tr_addr $stripe0_l2_sec0_tr_svc_id $stripe1_id $stripe1_l2_prim_tr_addr $stripe1_l2_prim_tr_svc_id $stripe1_l2_sec0_tr_addr $stripe1_l2_sec0_tr_svc_id" | ssh ${server_B_ip}

# connect to the virtual disk
sudo nvme connect --nqn "$final_tgt_nqn" --transport "tcp" --traddr "$l3_cntlr0_tr_addr" --trsvcid "$l3_cntlr0_tr_svc_id" --hostnqn "$external_host_nqn"
sudo nvme connect --nqn "$final_tgt_nqn" --transport "tcp" --traddr "$l3_cntlr1_tr_addr" --trsvcid "$l3_cntlr1_tr_svc_id" --hostnqn "$external_host_nqn"

# disconnect from the virtual disk
sudo nvme disconnect --device nvme0
sudo nvme disconnect --device nvme1
