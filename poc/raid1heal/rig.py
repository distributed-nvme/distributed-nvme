"""The rig, hard-coded. Demo code: one RAID1 on cn0 over two NVMe-oF legs.

There is no config file and nothing here is tunable at run time -- if you move
the rig, edit this file.
"""

MD = "md0"
NAME = "dnvr1:0"

# 16 MiB of head room below the data, so the 4 KiB probe slot at
# DATA_OFFSET-4096 can never collide with the superblock or the bitmap.
DATA_OFFSET = 32768                      # sectors
PROBE_BYTES = 4096
PROBE_OFFSET = DATA_OFFSET * 512 - PROBE_BYTES

LEGS = [
    dict(slot=0, node="dn0", addr="192.168.122.48",
         nqn="nqn.2026-08.org.dnv:dn0",
         path="/dev/disk/by-id/nvme-uuid.11111111-1111-1111-1111-000000000000"),
    dict(slot=1, node="dn1", addr="192.168.122.70",
         nqn="nqn.2026-08.org.dnv:dn1",
         path="/dev/disk/by-id/nvme-uuid.22222222-2222-2222-2222-000000000000"),
]

PAUSE_FILE = "/run/dnv-io-pause"          # touch it and io_gen.py lets go of the array
