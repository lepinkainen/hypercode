# Private host deployment

Run Hypercode as the OS user who owns the projects and Codex login. Keep project files, the Hypercode data directory, and the Codex home directory on persistent storage.

Local use binds to `127.0.0.1:8090`. On a VM or VPS, bind directly to that host's Tailscale IP:

```sh
/opt/hypercode/hypercode \
  -addr 100.101.102.103:8090 \
  -data-dir /home/you/.local/share/hypercode \
  -codex /home/you/.local/bin/codex
```

Replace the sample address with the address assigned to your host. Public, wildcard, and ordinary LAN listeners are rejected. Access is intended for your own Tailscale devices. There is no application login or multi-user authorization layer.

## Example systemd service

Install the built binary and adapt the paths and user in this example:

```ini
[Unit]
Description=Hypercode private coding workspace
After=network-online.target tailscaled.service
Wants=network-online.target

[Service]
Type=simple
User=you
WorkingDirectory=/home/you/projects
Environment=HOME=/home/you
Environment=PATH=/home/you/.local/bin:/usr/local/bin:/usr/bin:/bin
ExecStart=/opt/hypercode/hypercode -addr 100.101.102.103:8090 -data-dir /home/you/.local/share/hypercode -codex /home/you/.local/bin/codex
Restart=on-failure
RestartSec=5
KillMode=control-group
TimeoutStopSec=15

[Install]
WantedBy=multi-user.target
```

Authenticate with `codex login` as this user before starting the service. Hypercode does not manage CLI credentials. All browser URLs are relative and work through the same HTTP origin.

A browser disconnect leaves Codex running. A service/VM restart interrupts the turn. Reload the chat, choose Resume chat, then send a message to continue. Approvals from a lost process cannot be answered.

For a simple consistent backup, stop the service and copy the data directory together with project files and the Codex home directory. SQLite uses WAL, so do not copy only the main database while the service is running.

## Host verification

The local browser and protocol checks do not substitute for a test on your VM. After deploying:

1. Start a turn and disconnect the browser from Tailscale. Reconnect and confirm output continues without duplication.
2. Refresh during an approval and answer it.
3. Restart the service during a turn. Confirm interrupted history and explicit cold resume.
4. Reboot the VM and confirm the service starts with the correct paths and CLI login.

A remote Tailscale deployment was not exercised during the initial local implementation.
