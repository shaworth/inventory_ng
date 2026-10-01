# inventory_ng

A simple RFID/NFC-enabled inventory system for tracking stowed gear aboard
_SV Frog & Puffin_. It ships as one self-contained Go binary that serves:

- HTTP dashboard + REST API on `:8080`
- Embedded MQTT broker on `:1883` (TCP) and `:1884` (WebSocket)

Items form a parent/child hierarchy (root = the vessel); tapping an NFC tag
publishes to the `vessel/scan` topic, which the dashboard uses to jump to a
container or auto-register a new tag.

## Platform targets

| Target            | Build flag                  | Process supervision        |
|-------------------|-----------------------------|----------------------------|
| macOS (dev)       | `darwin/arm64`              | terminal session           |
| Raspberry Pi      | `linux/arm`, `linux/arm64`  | `systemd` (`--install`)    |
| Android phone     | `android/arm64`             | `tmux` via Termux:Boot     |

## Running on an Android phone (Termux + tmux)

The binary cross-compiles for Android ARM64 and is deployed to a Blackview
phone, where it runs under [Termux](https://termux.dev). Android has no systemd
for user services, so the server is hosted in a **detached `tmux` session**: it
survives SSH/terminal disconnects, can be reattached, and is brought back
automatically on device boot by the **Termux:Boot** app.

### One-time setup

    pkg install tmux        # tmux is the process host
    # Also install the "Termux:Boot" app (from F-Droid) and launch it once so
    # its boot receiver registers; disable battery optimisation for Termux.

### Files installed on the phone

    ~/inventory_ng/inventory_server   # the Go binary (deployed by ./deploy.sh)
    ~/inventory_ng/inventory-ctl      # lifecycle controller (copied from termux/)
    ~/inventory_ng/inventory.log      # stdout/stderr + MQTT traffic log
    ~/.termux/boot/start-services     # Termux:Boot hook (copied from termux/)

Build and deploy the binary as usual, then refresh the helper scripts:

    ./build_all.sh     # -> dist/android/arm64/inventory_server
    ./deploy.sh        # scp's the binary to blackview:./inventory_ng

    scp termux/inventory-ctl  blackview:inventory_ng/inventory-ctl
    scp termux/start-services blackview:.termux/boot/start-services
    ssh blackview 'chmod 700 ~/inventory_ng/inventory-ctl ~/.termux/boot/start-services'

### Managing the server

All lifecycle operations go through `inventory-ctl`:

    ~/inventory_ng/inventory-ctl start     # launch in a detached tmux session
    ~/inventory_ng/inventory-ctl stop      # stop the server and its session
    ~/inventory_ng/inventory-ctl restart   # stop, then start
    ~/inventory_ng/inventory-ctl status    # is it running?
    ~/inventory_ng/inventory-ctl logs      # follow the log (Ctrl-C to exit)
    ~/inventory_ng/inventory-ctl attach    # attach to the session (Ctrl-b d to detach)

`start` is idempotent, so it is safe to call on every boot. `stop`/`restart`
send the server a Ctrl-C before tearing the session down.

> Note: with no systemd unit present, the binary prints an interactive
> "run directly in this terminal session?" prompt. `inventory-ctl` feeds it `y`
> so the detached session never blocks on stdin.

### Boot

`~/.termux/boot/start-services` waits a few seconds for the network, then runs
`inventory-ctl start`. Termux:Boot runs its sibling scripts first: `0_wakelock`
(keeps the CPU awake) and `sshd`. Test the hook without rebooting:

    ~/.termux/boot/start-services   # no-op when the server is already running

### Reach it

    http://<phone-ip>:8080/index.html    # dashboard
    http://<phone-ip>:8080/scanner.html  # NFC scanner

The embedded MQTT broker listens on `:1883` (TCP) and `:1884` (WebSocket) too.

### Name resolution (mDNS)

The server also announces itself over mDNS, so it can be reached by name:

    http://blackview.local:8080/

This is **announcement-only**: Android/Termux cannot *receive* multicast, so the
phone cannot answer mDNS queries — but it *can* broadcast unsolicited
announcements, which clients cache. Change the name with `-hostname <name>`, or
disable it with `-advertise=false`.

Alongside the name it announces these services (so clients/discovery tools see
the phone):

| Service            | Port | Notes                        |
|--------------------|------|------------------------------|
| `_http._tcp`       | 8080 | dashboard / scanner / hosts  |
| `_ssh._tcp`        | 8022 | Termux sshd                  |
| `_sftp-ssh._tcp`   | 8022 | SFTP/SCP over ssh            |
| `_mqtt._tcp`       | 1883 | embedded MQTT broker         |
| `_workstation._tcp`| —    | generic host entry           |

On shutdown it sends an mDNS "goodbye" (TTL=0) so peers drop the records.

## Network host discovery (`/`)

`GET /` serves a discovery page that browses the local network via mDNS /
DNS-SD and lists:

- each host found, with its IPv4/IPv6 address(es),
- the services it advertises (e.g. `_http._tcp`, `_ssh._tcp`), and
- a clickable link where a URL can be inferred (HTTP/HTTPS services, honouring
  the `UrlPath` TXT record — e.g. the Cerbo GX at `http://<ip>/app`).

`/index.html` (dashboard) and `/scanner.html` (NFC scanner) are unchanged.
Results are cached for 30 s; append `?refresh=1` to force a rescan. Links open
in a **named window** (a slug per host + service), so clicking the same service
again reuses that window instead of opening another one.

Implemented in `hosts.go` + `hosts.html`, stdlib only. Because Android/Termux
cannot receive multicast, it issues **legacy unicast** mDNS queries (source
port != 5353) so responders reply unicast — which works on the phone as well as
on the Pi/Mac. (Note: `build_all.sh` compiles the whole package, not just
`main.go`, so the extra file is included.)

## Raspberry Pi (systemd)

The same binary can run as a systemd service on a Pi:

    sudo ./inventory_server --install

This writes `/etc/systemd/system/vessel-inventory.service`, enables it, and
starts it. `./deploy.sh` and `./build_all.sh -d` push the Linux build and
register/restart the service automatically.

## Development

    go build -o inventory_server main.go
    ./inventory_server          # runs in the current directory

