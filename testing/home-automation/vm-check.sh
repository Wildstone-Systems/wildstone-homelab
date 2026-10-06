#!/usr/bin/env bash
# Read-only checks for the home-automation hardware runbook
# (testing/home-automation/README.md, #453).
#
#   sudo --preserve-env=WILD_TOKEN,NODE ./vm-check.sh <section> [section ...]
#
# (--preserve-env passes your WILD_TOKEN and NODE through sudo, which
# otherwise drops them; only the api and canary sections use them.)
#
# Sections:
#   host        machine, Docker, Wildstone services, IPv6/Thread sysctls
#   radios      USB devices and their /dev/serial/by-id names
#   api         Keystone's radio inventory, readiness probe and
#               home-automation status (needs WILD_TOKEN and NODE)
#   containers  how each home-automation container is running, and
#               which ports are listening
#   identity    fingerprints of each controller's identity (hashes
#               only, never a key) to compare before and after a drill
#   restored    the same fingerprints, read from the <plan>-restore
#               volumes `wild backup restore` made, for the files that
#               carry them (Zigbee, Z-Wave, Home Assistant)
#   canary      the drill canary in each live volume and in each
#               <plan>-restore volume
#   all         everything above
#
# This script never changes anything: it reads files, asks Docker and
# systemd what is running, and makes GET requests. Every step of the
# runbook that changes something is a command the runbook prints for
# YOU to run. Run it with sudo, because Docker volumes and some sockets
# are root-only.
#
# Output has USB serial numbers and MAC addresses replaced with
# REPLACED, so it can be pasted back. It still contains private LAN
# addresses: fine in a private chat, but replace them before posting
# anywhere public. Set VM_CHECK_RAW=1 to see the unredacted values.
#
# Environment:
#   WILD_TOKEN   Keystone API token (api section)
#   NODE         the node name `wild node list` shows (api section)
#   KEYSTONE_API Keystone's API base (default http://127.0.0.1:9441)
set -uo pipefail

KEYSTONE_API="${KEYSTONE_API:-http://127.0.0.1:9441}"
CANARY=".wildstone-canary"

# plan:volume:container, for every controller with an identity volume.
CONTROLLERS="zigbee:zigbee2mqtt-data:zigbee2mqtt
zwave:zwave-js-ui-store:zwave-js-ui
thread:otbr-data:otbr
matter:matter-data:matter-server
home-assistant:home-assistant-config:home-assistant"

say()  { printf '\n=== %s\n' "$*"; }
item() { printf -- '--- %s\n' "$*"; }
have() { command -v "$1" >/dev/null 2>&1; }

# redact replaces the serial-number part of USB by-id names and MAC
# addresses. A by-id name is usb-<vendor>_<model>_<serial>-if<NN>[-port<N>];
# the vendor and model stay so the reader can tell the sticks apart.
redact() {
  if [ "${VM_CHECK_RAW:-0}" = 1 ]; then
    cat
    return
  fi
  sed -E \
    -e 's#(usb-[A-Za-z0-9.+-]+(_[A-Za-z0-9.+-]+)*)_[A-Za-z0-9.+:-]+(-if[0-9]+)#\1_REPLACED\3#g' \
    -e 's#("serial": *")[^"]*"#\1REPLACED"#g' \
    -e 's#([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}#REPLACED-MAC#g'
}

volume_dir() { # volume -> its directory on the host, or nothing
  docker volume inspect -f '{{.Mountpoint}}' "$1" 2>/dev/null
}

# FROM_RESTORE=1 (the "restored" section) reads each controller's files
# from <plan>-restore/backup/<volume> instead of the live volume.
FROM_RESTORE=0
ident_dir() { # plan volume -> where to read that volume's files
  local r
  if [ "$FROM_RESTORE" = 1 ]; then
    r="$(volume_dir "$1-restore")"
    [ -n "$r" ] && [ -d "$r/backup/$2" ] && printf '%s' "$r/backup/$2"
  else
    volume_dir "$2"
  fi
}

running() { # container -> 0 if running
  [ "$(docker inspect -f '{{.State.Running}}' "$1" 2>/dev/null)" = true ]
}

sha() { sha256sum | cut -c1-16; }

sysctl_show() {
  local key="$1" path
  path="/proc/sys/${key//.//}"
  if [ -r "$path" ]; then
    printf '%s = %s\n' "$key" "$(cat "$path")"
  else
    printf '%s = (absent)\n' "$key"
  fi
}

default_nic() { ip route show default 2>/dev/null | awk '{for (i=1;i<NF;i++) if ($i=="dev") {print $(i+1); exit}}'; }

section_host() {
  say "host"
  item "machine"
  printf 'arch: %s\n' "$(uname -m)"
  printf 'kernel: %s\n' "$(uname -r)"
  if [ -r /etc/os-release ]; then
    # shellcheck disable=SC1091
    ( . /etc/os-release && printf 'os: %s\n' "${PRETTY_NAME:-unknown}" )
  fi
  item "services"
  for u in docker keystone wardstoned bluetooth; do
    printf '%-11s %s\n' "$u" "$(systemctl is-active "$u" 2>/dev/null || true)"
  done
  if have docker; then
    printf 'docker: %s\n' "$(docker version --format '{{.Server.Version}}' 2>/dev/null || echo unreachable)"
  fi
  if have wild; then
    printf 'wild: %s\n' "$(wild version 2>/dev/null | head -1)"
  fi
  if have keystone; then
    printf 'keystone: %s\n' "$(keystone -version 2>/dev/null | head -1)"
  fi
  local nic
  nic="$(default_nic)"
  item "network (default-route interface: ${nic:-none})"
  sysctl_show net.ipv6.conf.all.disable_ipv6
  sysctl_show net.ipv6.conf.all.forwarding
  sysctl_show net.ipv4.ip_forward
  if [ -n "$nic" ]; then
    sysctl_show "net.ipv6.conf.$nic.disable_ipv6"
    sysctl_show "net.ipv6.conf.$nic.accept_ra"
    sysctl_show "net.ipv6.conf.$nic.accept_ra_rt_info_max_plen"
    ip -6 addr show dev "$nic" 2>/dev/null | awk '/inet6/ {print "ipv6:", $2, $4}'
    ip link show dev "$nic" 2>/dev/null | head -1 | grep -o '<[^>]*>' | sed 's/^/flags: /'
    ip -6 route show default 2>/dev/null | sed 's/^/ipv6 default: /'
  fi
  item "Thread prerequisites"
  if [ -c /dev/net/tun ]; then echo "tun: /dev/net/tun present"; else echo "tun: MISSING"; fi
  if grep -q '^ip6table_filter ' /proc/modules 2>/dev/null; then
    echo "ip6table_filter: loaded"
  else
    echo "ip6table_filter: not loaded"
  fi
  if [ -f /etc/sysctl.d/60-wildstone-otbr.conf ]; then
    sed 's/^/60-wildstone-otbr.conf: /' /etc/sysctl.d/60-wildstone-otbr.conf
  fi
  if [ -f /etc/modules-load.d/wildstone-otbr.conf ]; then
    echo "modules-load.d/wildstone-otbr.conf: present"
  fi
  item "Bluetooth"
  if [ -d /sys/class/bluetooth ] && [ -n "$(ls -A /sys/class/bluetooth)" ]; then
    find /sys/class/bluetooth -mindepth 1 -maxdepth 1 -printf 'adapter: %f\n'
  else
    echo "adapter: none"
  fi
  if have rfkill; then rfkill list bluetooth 2>/dev/null; fi
  sysctl_show net.ipv4.ping_group_range
}

section_radios() {
  say "radios"
  item "/dev/serial/by-id"
  if [ -d /dev/serial/by-id ]; then
    find /dev/serial/by-id -mindepth 1 -printf '%p -> %l\n' | sort
  else
    echo "(no /dev/serial/by-id: no USB serial device is visible)"
  fi
  item "tty devices"
  find /dev -maxdepth 1 \( -name 'ttyUSB*' -o -name 'ttyACM*' \) -printf '%p\n' | sort
  item "lsusb"
  if have lsusb; then lsusb; else echo "lsusb not installed (sudo apt install usbutils)"; fi
}

api_get() { # path
  curl -sS --max-time 30 -H "Authorization: Bearer ${WILD_TOKEN}" "${KEYSTONE_API}$1"
}

pretty() {
  if have jq; then jq .; else python3 -m json.tool; fi
}

section_api() {
  say "api"
  if [ -z "${WILD_TOKEN:-}" ] || [ -z "${NODE:-}" ]; then
    echo "set WILD_TOKEN and NODE first (see the runbook, step 0.6)"
    return
  fi
  item "nodes"
  api_get /api/v1/nodes | pretty
  item "radio inventory"
  api_get "/api/v1/nodes/${NODE}/probes/radios" | pretty
  item "home-automation readiness"
  api_get "/api/v1/nodes/${NODE}/probes/home-automation" | pretty
  item "home-automation status (managed, everything selected)"
  api_get "/api/v1/home-automation?topology=managed&select=zigbee,zwave,matter,thread" | pretty
}

section_containers() {
  say "containers"
  local c
  for c in mosquitto zigbee2mqtt zwave-js-ui otbr matter-server home-assistant; do
    item "$c"
    if ! docker inspect "$c" >/dev/null 2>&1; then
      echo "not present"
      continue
    fi
    docker inspect -f 'state={{.State.Status}} health={{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}} restarts={{.RestartCount}} started={{.State.StartedAt}}
network={{.HostConfig.NetworkMode}} privileged={{.HostConfig.Privileged}} capadd={{.HostConfig.CapAdd}} readonly={{.HostConfig.ReadonlyRootfs}}
devices={{json .HostConfig.Devices}}
mounts={{range .Mounts}}{{.Name}}{{.Source}}:{{.Destination}}{{if not .RW}}:ro{{end}} {{end}}
ports={{json .HostConfig.PortBindings}}
image={{.Config.Image}}
spec-hash={{index .Config.Labels "dev.wildstone.spec-hash"}}' "$c" 2>/dev/null
  done
  item "TCP listeners (1883 mqtt, 3000 zwave ws, 5580 matter, 8080/8091 UIs, 8081 otbr rest, 8123 home assistant, 80/443)"
  ss -ltnpH 2>/dev/null | awk '$4 ~ /:(1883|3000|5580|8080|8081|8091|8123|80|443)$/ {print $4, $6}'
  item "UDP listeners (5353 mdns, 5540 matter)"
  ss -lunpH 2>/dev/null | awk '$4 ~ /:(5353|5540)$/ {print $4, $6}'
  item "wpan0"
  ip -6 addr show dev wpan0 2>/dev/null || echo "no wpan0"
}

# The identity of each controller, as hashes and public ids only. Equal
# fingerprints before and after a drill mean the same network.
section_identity() {
  say "identity"
  local d
  item "zigbee (zigbee2mqtt-data)"
  d="$(ident_dir zigbee zigbee2mqtt-data)"
  if [ -n "$d" ] && [ -f "$d/configuration.yaml" ]; then
    python3 - "$d/configuration.yaml" <<'PY'
import hashlib, re, sys
text = open(sys.argv[1]).read()
def block(key):
    # the value on the key's line, plus any indented "- n" lines under it
    m = re.search(r'^(\s*)' + key + r':(.*)$', text, re.M)
    if not m:
        return None
    out = [m.group(2).strip()]
    for line in text[m.end():].splitlines()[1:]:
        if re.match(r'^\s*-\s', line):
            out.append(line.strip())
        else:
            break
    return " ".join(out)
key = block("network_key")
print("network_key sha256:", hashlib.sha256(key.encode()).hexdigest()[:16] if key else "(not set)")
for k in ("pan_id", "ext_pan_id", "channel"):
    print(k + ":", block(k) or "(not set)")
PY
    if [ -f "$d/coordinator_backup.json" ]; then
      echo "coordinator_backup.json: present"
    else
      echo "coordinator_backup.json: absent"
    fi
    if [ -f "$d/database.db" ]; then
      printf 'paired devices in database.db: %s\n' "$(grep -cE '"type":"(Router|EndDevice)"' "$d/database.db")"
    fi
  else
    echo "no zigbee2mqtt-data/configuration.yaml"
  fi

  item "zwave (zwave-js-ui-store)"
  d="$(ident_dir zwave zwave-js-ui-store)"
  if [ -n "$d" ] && [ -f "$d/settings.json" ]; then
    python3 - "$d/settings.json" <<'PY'
import hashlib, json, sys
z = json.load(open(sys.argv[1])).get("zwave", {})
for k in ("securityKeys", "securityKeysLongRange"):
    v = z.get(k) or {}
    blob = json.dumps(v, sort_keys=True)
    print(k, "sha256:", hashlib.sha256(blob.encode()).hexdigest()[:16], "names:", ",".join(sorted(v)))
PY
    if [ -d "$d/backups/nvm" ]; then
      find "$d/backups/nvm" -maxdepth 1 -type f -printf 'nvm backup: %f\n' | sort | tail -3
    fi
  else
    echo "no zwave-js-ui-store/settings.json"
  fi

  item "thread (otbr)"
  if [ "$FROM_RESTORE" = 1 ]; then
    echo "(the dataset is read from the running border router only; compare file names)"
    d="$(ident_dir thread otbr-data)"
    [ -n "$d" ] && find "$d" -maxdepth 1 -type f -printf 'restored otbr-data: %s bytes %f\n'
  elif running otbr; then
    printf 'active dataset sha256: %s\n' "$(curl -s --max-time 5 -H 'Accept: text/plain' http://127.0.0.1:8081/node/dataset/active | sha)"
    printf 'state: %s\n' "$(docker exec otbr ot-ctl state 2>/dev/null | head -1)"
    printf 'channel: %s\n' "$(docker exec otbr ot-ctl channel 2>/dev/null | head -1)"
    printf 'rcp: %s\n' "$(docker exec otbr ot-ctl rcp version 2>/dev/null | head -1)"
  else
    echo "otbr not running"
  fi
  d="$(volume_dir otbr-data)"
  if [ -n "$d" ]; then
    find "$d" -maxdepth 1 -type f -printf 'otbr-data: %s bytes %f\n' 2>/dev/null
  fi

  item "matter (matter-server)"
  if [ "$FROM_RESTORE" = 1 ]; then
    d="$(ident_dir matter matter-data)"
    [ -n "$d" ] && printf 'restored matter-data: %s files\n' "$(find "$d" -type f | wc -l)"
    echo "(the fabric id is read from the running server only)"
  elif running matter-server; then
    python3 - <<'PY'
# A minimal WebSocket read: the server's first message is server_info.
import base64, json, os, socket, struct
try:
    s = socket.create_connection(("127.0.0.1", 5580), timeout=10)
    key = base64.b64encode(os.urandom(16)).decode()
    s.sendall(("GET /ws HTTP/1.1\r\nHost: 127.0.0.1:5580\r\nUpgrade: websocket\r\n"
               "Connection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n" % key).encode())
    buf = b""
    while b"\r\n\r\n" not in buf:
        buf += s.recv(4096)
    head, buf = buf.split(b"\r\n\r\n", 1)
    if b" 101 " not in head.split(b"\r\n")[0]:
        raise SystemExit("handshake refused: " + head.split(b"\r\n")[0].decode())
    def need(n):
        global buf
        while len(buf) < n:
            buf += s.recv(65536)
        out, buf = buf[:n], buf[n:]
        return out
    b1, b2 = need(2)
    n = b2 & 0x7F
    if n == 126:
        n = struct.unpack(">H", need(2))[0]
    elif n == 127:
        n = struct.unpack(">Q", need(8))[0]
    info = json.loads(need(n))
    for k in ("fabric_id", "compressed_fabric_id", "sdk_version", "schema_version"):
        print(k + ":", info.get(k))
except SystemExit as e:
    print(e)
except Exception as e:
    print("could not read server_info:", e)
PY
  else
    echo "matter-server not running"
  fi

  item "home assistant (home-assistant-config)"
  d="$(ident_dir home-assistant home-assistant-config)"
  if [ -n "$d" ] && [ -d "$d/.storage" ]; then
    [ -f "$d/.storage/core.uuid" ] && printf 'instance uuid sha256: %s\n' "$(sha < "$d/.storage/core.uuid")"
    python3 - "$d" <<'PY'
import json, os, sys
d = sys.argv[1]
def load(p):
    try:
        return json.load(open(os.path.join(d, p)))
    except Exception:
        return None
auth = load(".storage/auth")
if auth:
    users = [u for u in auth["data"].get("users", []) if not u.get("system_generated")]
    print("users:", len(users))
entries = load(".storage/core.config_entries")
if entries:
    domains = sorted({e["domain"] for e in entries["data"].get("entries", [])})
    print("integrations:", ", ".join(domains))
ds = load(".storage/thread.datasets")
if ds:
    print("thread datasets known to Home Assistant:", len(ds["data"].get("datasets", [])))
try:
    import re
    autos = open(os.path.join(d, "automations.yaml")).read()
    print("automations:", len(re.findall(r"^- ", autos, re.M)))
except Exception:
    pass
http = load(".storage/http")
if http:
    print("trusted_proxies:", http["data"].get("stable", {}).get("trusted_proxies"))
PY
  else
    echo "no home-assistant-config/.storage"
  fi
}

section_canary() {
  say "canary"
  local plan vol d r
  while IFS=: read -r plan vol _; do
    item "$plan"
    d="$(volume_dir "$vol")"
    if [ -n "$d" ] && [ -f "$d/$CANARY" ]; then
      printf 'live %s: %s\n' "$vol" "$(cat "$d/$CANARY")"
    else
      printf 'live %s: no canary\n' "$vol"
    fi
    r="$(volume_dir "$plan-restore")"
    if [ -z "$r" ]; then
      printf 'restore volume %s-restore: absent\n' "$plan"
    elif [ -f "$r/backup/$vol/$CANARY" ]; then
      printf 'restored %s-restore: %s\n' "$plan" "$(cat "$r/backup/$vol/$CANARY")"
      printf 'restored files: %s\n' "$(find "$r/backup/$vol" -type f | wc -l)"
    else
      printf 'restored %s-restore: no canary under backup/%s\n' "$plan" "$vol"
    fi
  done <<< "$CONTROLLERS"
  item "backup runs (last 15)"
  if [ -n "${WILD_TOKEN:-}" ]; then
    api_get /api/v1/backups | python3 -c '
import json, sys
d = json.load(sys.stdin)
for r in d.get("runs", [])[:15]:
    print(r.get("planName"), r.get("runKind"), r.get("status"), r.get("startedAt"), r.get("detail", ""))
'
  else
    echo "set WILD_TOKEN to list backup runs"
  fi
}

main() {
  if [ "$#" -eq 0 ]; then
    sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'
    exit 2
  fi
  if [ "$(id -u)" -ne 0 ]; then
    echo "run with sudo: Docker volumes are readable by root only" >&2
    exit 2
  fi
  local s
  for s in "$@"; do
    case "$s" in
      host) section_host ;;
      radios) section_radios ;;
      api) section_api ;;
      containers) section_containers ;;
      identity) section_identity ;;
      restored) FROM_RESTORE=1; say "restored copies"; section_identity; FROM_RESTORE=0 ;;
      canary) section_canary ;;
      all)
        section_host; section_radios; section_api; section_containers; section_identity
        FROM_RESTORE=1; say "restored copies"; section_identity; FROM_RESTORE=0
        section_canary ;;
      *) echo "unknown section: $s" >&2; exit 2 ;;
    esac
  done
}

main "$@" 2>&1 | redact
