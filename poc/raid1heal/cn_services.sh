#!/bin/bash
# cn_services.sh -- run the healer every 60 s and keep verified IO on the array.
set -eu
sudo tee /etc/systemd/system/dnv-healer.service >/dev/null <<'U'
[Unit]
Description=RAID1 leg health checker / healer
[Service]
ExecStart=/usr/bin/python3 /opt/dnv/raid1_healer.py
Restart=always
RestartSec=5
StandardOutput=append:/var/log/dnv-healer.log
StandardError=append:/var/log/dnv-healer.log
[Install]
WantedBy=multi-user.target
U
sudo tee /etc/systemd/system/dnv-iogen.service >/dev/null <<'U'
[Unit]
Description=RAID1 verified IO load
[Service]
ExecStart=/usr/bin/python3 /opt/dnv/io_gen.py
Restart=always
RestartSec=5
StandardOutput=append:/var/log/dnv-iogen.log
StandardError=append:/var/log/dnv-iogen.log
[Install]
WantedBy=multi-user.target
U
sudo systemctl daemon-reload
sudo systemctl restart dnv-healer dnv-iogen
sleep 3
systemctl is-active dnv-healer dnv-iogen
