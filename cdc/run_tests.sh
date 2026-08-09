#!/bin/bash
#
# run_tests.sh -- run the four CDC test cases from plan.md §11 (filtering,
#   auto-disconnect x2, auto-add) against the live 6-node testbed and print
#   PASS/FAIL per case. Exits 0 only if all four pass.
#
#   Depends on sibling scripts in the same dir:
#       ./setup_targets.sh  [sync]   provision/reconcile cn targets from $CDC_FILE
#       ./teardown_targets.sh        remove all cn targets
#       ./setup_hosts.sh             provision host0+host1 (stas pointing at cdc0:8009)
#       ./teardown_hosts.sh          teardown hosts
#   and the Go CDC binary `./cdc` (built fresh here from the cdc/ Go sources).
#
#   Conventions follow poc/: `set -uo pipefail` (NO -e); errors handled
#   explicitly with _fail/_warn; assertions emit PASS:/FAIL: lines.
#
set -uo pipefail

SSH_USER="yupeng"
CDC0_IP="192.168.122.78"
CDC0_PORT=8009
HOST0_IP="192.168.122.193"
HOST1_IP="192.168.122.197"
CN0_IP="192.168.122.125"
CN1_IP="192.168.122.229"
CN2_IP="192.168.122.77"
HOST0_NQN="nqn.2026-08.org.dnv:host:host0"
HOST1_NQN="nqn.2026-08.org.dnv:host:host1"
SUBSYS_A_NQN="nqn.2026-08.org.dnv:subsys-A"
SUBSYS_B_NQN="nqn.2026-08.org.dnv:subsys-B"
CDC_FILE="/etc/dnv-cdc/subsystems.json"
BIN_DIR="/home/yupeng/Code/distributed-nvme/cdc"
SETTLE=5
GO_BIN="/usr/local/go/bin/go"
REMOTE_DIR="/home/${SSH_USER}"

PASS_COUNT=0
FAIL_COUNT=0
OVERALL_RC=0

_ssh() {
    ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o LogLevel=ERROR -o ConnectTimeout=10 "${SSH_USER}@$1" "${@:2}"
}

_scp() {
    scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o LogLevel=ERROR "$@"
}

_info() { echo "[INFO] $*"; }
_warn() { echo "[WARN] $*" >&2; }
_fail() { echo "[FAIL] $*" >&2; exit 1; }

pass() { echo "PASS: $1"; PASS_COUNT=$((PASS_COUNT + 1)); }
fail() { echo "FAIL: $1 -- $2"; FAIL_COUNT=$((FAIL_COUNT + 1)); OVERALL_RC=1; }

_settle() { sleep "$SETTLE"; }

_write_file() {
    _ssh "$CDC0_IP" "sudo tee '$CDC_FILE' > /dev/null"
    _settle
}

nvme_list()    { _ssh "$1" "nvme list"; }
nvme_subsys()  {
    local host_ip="$1"; shift
    _ssh "$host_ip" "nvme list-subsys $*"
}

_assert_paths() {
    local host_ip="$1" nqn="$2" expected="$3" json out rc
    json=$(nvme_subsys "$host_ip" -o json 2>/dev/null) || json=""
    out=$(python3 - "$nqn" "$expected" "$json" 2>&1 <<'PY'
import json, sys
trg = sys.argv[1]
exp = set(sys.argv[2].split())
raw = sys.argv[3]
try:
    data = json.loads(raw)
except Exception as e:
    print("parse-error: %s raw=[%s]" % (e, raw[:200]))
    sys.exit(4)

found = set()

def addr_from(obj):
    if not isinstance(obj, dict):
        return ""
    ad = obj.get("AddressDetails")
    if isinstance(ad, dict) and ad.get("traddr"):
        return ad["traddr"]
    a = obj.get("Address")
    if isinstance(a, str) and "traddr=" in a:
        return a.split("traddr=")[1].split(",")[0].strip()
    if obj.get("Traddr"):
        return obj["Traddr"]
    return ""

def iter_subs(node):
    if isinstance(node, dict):
        subs = node.get("Subsystems")
        if isinstance(subs, list):
            for s in subs:
                yield s
        for v in node.values():
            for s in iter_subs(v):
                yield s
    elif isinstance(node, list):
        for x in node:
            for s in iter_subs(x):
                yield s

for s in iter_subs(data):
    if not isinstance(s, dict):
        continue
    snqn = s.get("NQN") or s.get("SubsysNQN") or s.get("SubsystemNQN") or ""
    if snqn != trg:
        continue
    paths = s.get("Paths")
    if isinstance(paths, list):
        for p in paths:
            ta = addr_from(p)
            if ta:
                found.add(ta)

if found != exp:
    missing = sorted(exp - found)
    extra = sorted(found - exp)
    print("nqn=%s expected=%s found=%s missing=%s extra=%s"
          % (trg, sorted(exp), sorted(found), missing, extra))
    sys.exit(1)
sys.exit(0)
PY
)
    rc=$?
    if [ "$rc" -ne 0 ]; then
        if [ "$rc" -eq 4 ]; then
            echo "nvme list-subsys parse error; raw=[${json:0:200}]"
        else
            echo "$out"
        fi
        return 1
    fi
    return 0
}

_assert_no_leak() {
    local mon_ip="$1" victim="$2" vlabel="$3" f="/tmp/cdc_leak_${vlabel}.pcap"
    local tcp_pid cnt
    _ssh "$mon_ip" "sudo rm -f '$f'" >/dev/null 2>&1 || true
    _ssh "$mon_ip" "sudo timeout 11 tcpdump -i any -nn -c 1 -w '$f' host '$victim' 2>/dev/null" >/dev/null 2>&1 &
    tcp_pid=$!
    sleep 1
    _ssh "$victim" "sudo systemctl restart stafd" >/dev/null 2>&1 || true
    wait "$tcp_pid" 2>/dev/null || true
    sleep 1
    cnt=$(_ssh "$mon_ip" "sudo tcpdump -r '$f' 2>/dev/null | wc -l" 2>/dev/null)
    cnt=${cnt//[[:space:]]/}
    [ -z "$cnt" ] && cnt=0
    if [ "$cnt" -ne 0 ]; then
        echo "leak: $cnt packets from $victim seen on $mon_ip during stafd restart"
        return 1
    fi
    return 0
}

_start_cdc() {
    _ssh "$CDC0_IP" "pkill -f './cdc'" >/dev/null 2>&1 || true
    _scp "$BIN_DIR/cdc" "${SSH_USER}@${CDC0_IP}:${REMOTE_DIR}/cdc" >/dev/null 2>&1 \
        || { _warn "scp of cdc binary failed"; return 1; }
    _ssh "$CDC0_IP" "nohup '${REMOTE_DIR}/cdc' > /tmp/cdc.log 2>&1 &" >/dev/null 2>&1
    _info "cdc started on ${CDC0_IP}:${CDC0_PORT}"
}

_stop_cdc() {
    _ssh "$CDC0_IP" "pkill -f './cdc'" >/dev/null 2>&1 || true
}

_file_initial() { cat <<'JSON'
{
  "subsystems": [
    {
      "nqn": "nqn.2026-08.org.dnv:subsys-A",
      "allowed_hosts": ["nqn.2026-08.org.dnv:host:host0"],
      "ports": [
        { "traddr": "192.168.122.125", "trsvcid": "4420" },
        { "traddr": "192.168.122.229", "trsvcid": "4420" }
      ]
    },
    {
      "nqn": "nqn.2026-08.org.dnv:subsys-B",
      "allowed_hosts": ["nqn.2026-08.org.dnv:host:host1"],
      "ports": [
        { "traddr": "192.168.122.229", "trsvcid": "4420" },
        { "traddr": "192.168.122.77",  "trsvcid": "4420" }
      ]
    }
  ]
}
JSON
}

_file_test2() { cat <<'JSON'
{
  "subsystems": [
    {
      "nqn": "nqn.2026-08.org.dnv:subsys-A",
      "allowed_hosts": ["nqn.2026-08.org.dnv:host:host0"],
      "ports": [
        { "traddr": "192.168.122.229", "trsvcid": "4420" }
      ]
    },
    {
      "nqn": "nqn.2026-08.org.dnv:subsys-B",
      "allowed_hosts": ["nqn.2026-08.org.dnv:host:host1"],
      "ports": [
        { "traddr": "192.168.122.229", "trsvcid": "4420" },
        { "traddr": "192.168.122.77",  "trsvcid": "4420" }
      ]
    }
  ]
}
JSON
}

_file_test3() { cat <<'JSON'
{
  "subsystems": [
    {
      "nqn": "nqn.2026-08.org.dnv:subsys-A",
      "allowed_hosts": ["nqn.2026-08.org.dnv:host:host0"],
      "ports": [
        { "traddr": "192.168.122.229", "trsvcid": "4420" }
      ]
    },
    {
      "nqn": "nqn.2026-08.org.dnv:subsys-B",
      "allowed_hosts": ["nqn.2026-08.org.dnv:host:host1"],
      "ports": [
        { "traddr": "192.168.122.77", "trsvcid": "4420" }
      ]
    }
  ]
}
JSON
}

_file_test4() { cat <<'JSON'
{
  "subsystems": [
    {
      "nqn": "nqn.2026-08.org.dnv:subsys-A",
      "allowed_hosts": ["nqn.2026-08.org.dnv:host:host0"],
      "ports": [
        { "traddr": "192.168.122.229", "trsvcid": "4420" },
        { "traddr": "192.168.122.77",  "trsvcid": "4420" }
      ]
    },
    {
      "nqn": "nqn.2026-08.org.dnv:subsys-B",
      "allowed_hosts": ["nqn.2026-08.org.dnv:host:host1"],
      "ports": [
        { "traddr": "192.168.122.77", "trsvcid": "4420" }
      ]
    }
  ]
}
JSON
}

setup_env() {
    _info "=== SETUP: clean slate + provision ==="
    ./teardown_targets.sh   >/dev/null 2>&1 || _warn "teardown_targets.sh (pre) non-zero"
    ./teardown_hosts.sh     >/dev/null 2>&1 || _warn "teardown_hosts.sh (pre) non-zero"
    _stop_cdc
    _ssh "$CDC0_IP" "sudo rm -f '$CDC_FILE'; sudo mkdir -p '$(dirname "$CDC_FILE")'" >/dev/null 2>&1 || true
    _info "writing initial subsystems.json (test1 seed)"
    _file_initial | _write_file
    _info "provisioning cn targets"
    ./setup_targets.sh      >/dev/null 2>&1 || _warn "setup_targets.sh non-zero"
    _info "provisioning hosts"
    ./setup_hosts.sh        >/dev/null 2>&1 || _warn "setup_hosts.sh non-zero"
    _start_cdc || _warn "cdc start reported failure"
    _info "waiting for stafd discovery + stacd connect (10s)"
    sleep 10
}

cleanup_env() {
    _info "=== CLEANUP ==="
    _stop_cdc
    ./teardown_hosts.sh     >/dev/null 2>&1 || _warn "teardown_hosts.sh non-zero"
    ./teardown_targets.sh   >/dev/null 2>&1 || _warn "teardown_targets.sh non-zero"
    _ssh "$CDC0_IP" "sudo rm -f '$CDC_FILE'" >/dev/null 2>&1 || true
}

run_test1() {
    _info "=== Test 1: filtering ==="
    local reason
    if ! reason=$(_assert_paths "$HOST0_IP" "$SUBSYS_A_NQN" "$CN0_IP $CN1_IP"); then
        fail "test1 -- filtering" "host0 subsysA paths: $reason"
        return
    fi
    if ! reason=$(_assert_paths "$HOST1_IP" "$SUBSYS_B_NQN" "$CN1_IP $CN2_IP"); then
        fail "test1 -- filtering" "host1 subsysB paths: $reason"
        return
    fi
    if ! reason=$(_assert_no_leak "$CN0_IP" "$HOST1_IP" "h1_on_cn0"); then
        fail "test1 -- filtering" "leak host1->cn0: $reason"
        return
    fi
    if ! reason=$(_assert_no_leak "$CN2_IP" "$HOST0_IP" "h0_on_cn2"); then
        fail "test1 -- filtering" "leak host0->cn2: $reason"
        return
    fi
    pass "test1 -- filtering"
}

run_test2() {
    _info "=== Test 2: auto-disconnect (subsys-A cn0) ==="
    local reason
    _info "dropping subsys-A cn0 port from file"
    _file_test2 | _write_file
    if ! reason=$(_assert_paths "$HOST0_IP" "$SUBSYS_A_NQN" "$CN1_IP"); then
        fail "test2 -- auto-disconnect subsysA cn0" "host0 subsysA paths: $reason"
        return
    fi
    pass "test2 -- auto-disconnect subsysA cn0"
}

run_test3() {
    _info "=== Test 3: auto-disconnect (subsys-B cn1) ==="
    local reason
    _info "dropping subsys-B cn1 port from file"
    _file_test3 | _write_file
    if ! reason=$(_assert_paths "$HOST1_IP" "$SUBSYS_B_NQN" "$CN2_IP"); then
        fail "test3 -- auto-disconnect subsysB cn1" "host1 subsysB paths: $reason"
        return
    fi
    pass "test3 -- auto-disconnect subsysB cn1"
}

run_test4() {
    _info "=== Test 4: auto-add (subsys-A cn2) ==="
    local reason
    _info "adding subsys-A cn2 entry to file, then sync creates the cn2 target"
    _file_test4 | _write_file
    ./setup_targets.sh sync >/dev/null 2>&1 || _warn "setup_targets.sh sync non-zero"
    _info "waiting for CDC AEN + stacd auto-connect (ana re-eval margin)"
    sleep "$((SETTLE + 5))"
    if ! reason=$(_assert_paths "$HOST0_IP" "$SUBSYS_A_NQN" "$CN1_IP $CN2_IP"); then
        fail "test4 -- auto-add subsysA cn2" "host0 subsysA paths: $reason"
        return
    fi
    pass "test4 -- auto-add subsysA cn2"
}

main() {
    cd "$BIN_DIR" || _fail "cannot cd to $BIN_DIR"
    _info "building CDC (go build)"
    "$GO_BIN" build -o cdc . || _fail "go build failed"
    [ -x ./cdc ] || _fail "cdc binary not produced"

    setup_env
    run_test1
    run_test2
    run_test3
    run_test4
    cleanup_env

    echo "${PASS_COUNT}/4 passed"
    exit "$OVERALL_RC"
}

main "$@"