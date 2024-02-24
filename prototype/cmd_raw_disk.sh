#!/bin/bash

sudo fio --filename=/dev/nvme1n1 --name mytest --rw=randwrite --ioengine=libaio --iodepth=4 --bs=4k --numjobs=36 --direct=1 --time_based --runtime=60 --group_reporting --norandommap
# mytest: (g=0): rw=randwrite, bs=(R) 4096B-4096B, (W) 4096B-4096B, (T) 4096B-4096B, ioengine=libaio, iodepth=4
# ...
# fio-3.32
# Starting 36 processes
# Jobs: 36 (f=36): [w(36)][100.0%][w=586MiB/s][w=150k IOPS][eta 00m:00s]
# mytest: (groupid=0, jobs=36): err= 0: pid=20214: Sat Feb 24 00:05:45 2024
#   write: IOPS=146k, BW=569MiB/s (596MB/s)(33.3GiB/60002msec); 0 zone resets
#     slat (nsec): min=1442, max=586956, avg=5023.90, stdev=3719.58
#     clat (usec): min=169, max=9063, avg=982.89, stdev=145.00
#      lat (usec): min=171, max=9068, avg=987.91, stdev=145.29
#     clat percentiles (usec):
#      |  1.00th=[  709],  5.00th=[  799], 10.00th=[  816], 20.00th=[  848],
#      | 30.00th=[  889], 40.00th=[  930], 50.00th=[  979], 60.00th=[ 1020],
#      | 70.00th=[ 1045], 80.00th=[ 1090], 90.00th=[ 1172], 95.00th=[ 1237],
#      | 99.00th=[ 1401], 99.50th=[ 1483], 99.90th=[ 1614], 99.95th=[ 1680],
#      | 99.99th=[ 1811]
#    bw (  KiB/s): min=532288, max=624832, per=100.00%, avg=582756.71, stdev=635.91, samples=4284
#    iops        : min=133072, max=156208, avg=145689.16, stdev=158.98, samples=4284
#   lat (usec)   : 250=0.01%, 500=0.01%, 750=2.12%, 1000=52.75%
#   lat (msec)   : 2=45.13%, 4=0.01%, 10=0.01%
#   cpu          : usr=1.82%, sys=2.84%, ctx=3091773, majf=0, minf=394
#   IO depths    : 1=0.1%, 2=0.1%, 4=100.0%, 8=0.0%, 16=0.0%, 32=0.0%, >=64=0.0%
#      submit    : 0=0.0%, 4=100.0%, 8=0.0%, 16=0.0%, 32=0.0%, 64=0.0%, >=64=0.0%
#      complete  : 0=0.0%, 4=100.0%, 8=0.0%, 16=0.0%, 32=0.0%, 64=0.0%, >=64=0.0%
#      issued rwts: total=0,8735218,0,0 short=0,0,0,0 dropped=0,0,0,0
#      latency   : target=0, window=0, percentile=100.00%, depth=4

# Run status group 0 (all jobs):
#   WRITE: bw=569MiB/s (596MB/s), 569MiB/s-569MiB/s (596MB/s-596MB/s), io=33.3GiB (35.8GB), run=60002-60002msec

# Disk stats (read/write):
#   nvme1n1: ios=23/8714491, merge=0/0, ticks=6/8302196, in_queue=8302203, util=99.93%

sudo fio --filename=/dev/nvme1n1 --name mytest --rw=randread --ioengine=libaio --iodepth=4 --bs=4k --numjobs=36 --direct=1 --time_based --runtime=60 --group_reporting --norandommap
# mytest: (g=0): rw=randread, bs=(R) 4096B-4096B, (W) 4096B-4096B, (T) 4096B-4096B, ioengine=libaio, iodepth=4
# ...
# fio-3.32
# Starting 36 processes
# Jobs: 36 (f=36): [r(36)][100.0%][r=731MiB/s][r=187k IOPS][eta 00m:00s]
# mytest: (groupid=0, jobs=36): err= 0: pid=19842: Sat Feb 24 00:04:07 2024
#   read: IOPS=189k, BW=740MiB/s (776MB/s)(43.3GiB/60003msec)
#     slat (nsec): min=1405, max=108707, avg=4312.47, stdev=2921.55
#     clat (usec): min=149, max=8987, avg=755.08, stdev=144.27
#      lat (usec): min=151, max=8990, avg=759.39, stdev=144.53
#     clat percentiles (usec):
#      |  1.00th=[  424],  5.00th=[  494], 10.00th=[  578], 20.00th=[  660],
#      | 30.00th=[  693], 40.00th=[  709], 50.00th=[  734], 60.00th=[  775],
#      | 70.00th=[  824], 80.00th=[  881], 90.00th=[  938], 95.00th=[  996],
#      | 99.00th=[ 1106], 99.50th=[ 1156], 99.90th=[ 1254], 99.95th=[ 1303],
#      | 99.99th=[ 1401]
#    bw (  KiB/s): min=697400, max=818072, per=100.00%, avg=758222.05, stdev=678.57, samples=4284
#    iops        : min=174350, max=204518, avg=189555.55, stdev=169.64, samples=4284
#   lat (usec)   : 250=0.01%, 500=5.38%, 750=48.98%, 1000=40.90%
#   lat (msec)   : 2=4.73%, 4=0.01%, 10=0.01%
#   cpu          : usr=2.05%, sys=3.07%, ctx=3348812, majf=0, minf=478
#   IO depths    : 1=0.1%, 2=0.1%, 4=100.0%, 8=0.0%, 16=0.0%, 32=0.0%, >=64=0.0%
#      submit    : 0=0.0%, 4=100.0%, 8=0.0%, 16=0.0%, 32=0.0%, 64=0.0%, >=64=0.0%
#      complete  : 0=0.0%, 4=100.0%, 8=0.0%, 16=0.0%, 32=0.0%, 64=0.0%, >=64=0.0%
#      issued rwts: total=11363159,0,0,0 short=0,0,0,0 dropped=0,0,0,0
#      latency   : target=0, window=0, percentile=100.00%, depth=4

# Run status group 0 (all jobs):
#    READ: bw=740MiB/s (776MB/s), 740MiB/s-740MiB/s (776MB/s-776MB/s), io=43.3GiB (46.5GB), run=60003-60003msec

# Disk stats (read/write):
#   nvme1n1: ios=11336930/0, merge=0/0, ticks=8270569/0, in_queue=8270568, util=99.90%
