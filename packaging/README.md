# packaging/

Host-side supervision for `wardstoned` (ADR-W003 W5).

## `wardstoned.service`

A systemd unit for running `wardstoned` as the outer watchdog around the
agent's own self-update health gate:

- The agent's self-update flow (ADR-W003 §A6, W5) writes
  `update-state.json` into `--data-dir` right before promoting a new
  binary, giving the promoted process a 2-minute deadline to establish a
  Keystone session. A successful session establishment deletes the file
  ("self-update confirmed"). If a process starts and finds an expired,
  unconfirmed deadline, it swaps the previous binary generation back in and
  re-execs into it ("self-update reverted").
- That gate only works if a crashing promoted binary actually gets
  restarted quickly and repeatedly until either it confirms or its deadline
  passes — that's all `Restart=always` / `RestartSec=2` are for here.
  `WatchdogSec` and `sd_notify` are intentionally not used: there is no
  ongoing liveness signal to send once a session is established, and a
  hung-but-not-crashed process is out of scope for this gate (systemd's
  own process-exit-based restart is the only failure mode the gate is
  designed against).

### Install

```sh
sudo cp wardstoned.service /etc/systemd/system/wardstoned.service
sudo mkdir -p /var/lib/wildstone/wardstone
sudo systemctl daemon-reload
sudo systemctl enable --now wardstoned
```

Edit the `ExecStart=` line (or drop an override in
`/etc/systemd/system/wardstoned.service.d/`) to fill in `--node-id`,
`--keystone`, `--ca-fingerprint`, and — for the very first start only —
`--join-token`. See the comment in the unit file for an example.
