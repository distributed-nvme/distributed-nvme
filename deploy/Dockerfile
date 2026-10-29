# The image of dnv-csi (csi.md, Deployment): the Ubuntu release the lab
# runs, the tools the node plugin runs on the node, and the one binary. Build
# it from the repository root after `make build`:
#
#   docker build -t dnv-csi:0.1.0 -f deploy/Dockerfile bin
#
# The context is bin/, where `make build` puts dnv-csi; nothing else of the
# tree enters the image. One image serves both plugins: the pod's command
# names the subcommand.
FROM ubuntu:26.04

# nvme-cli connects and reads the discovery log; util-linux brings mount,
# umount, blkid and blockdev; e2fsprogs and xfsprogs make the two
# filesystems; coreutils brings stat, mkdir, rmdir and rm.
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        nvme-cli util-linux e2fsprogs xfsprogs coreutils && \
    rm -rf /var/lib/apt/lists/*

COPY dnv-csi /usr/local/bin/dnv-csi

ENTRYPOINT ["/usr/local/bin/dnv-csi"]
