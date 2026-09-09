# Deployment and security (v1)

Design constraints. Operator instructions live in `docs/deployment.md`.

## Deployment and access (v1)

- Hypercode is a single-user application. It can run on the user's computer or on a persistent VM/VPS. Remote access is exclusively through Tailscale, which provides network authentication and encrypted access. No application accounts, login screen, or multi-user authorization system.
- Default to a loopback listener. For VM/VPS deployment, allow binding to the host's Tailscale address. Do not expose the application on a public interface.
- Run Hypercode as a system service on the VM/VPS, under the same OS user that owns the projects and harness logins. Start the service at boot with an explicit data directory and executable paths.
- Keep project directories, SQLite, and harness home directories on persistent storage. The browser is a client; all agent execution, credentials, and project files remain on the host.
- A browser or network disconnect leaves work running. A service or VM restart preserves files and history but interrupts active work. Restarting the service does not automatically resume agent turns.
- Serve assets, HTTP actions, and SSE from the same origin. Use relative browser URLs so both localhost and the Tailscale address work.

## Security (v1)

- Trust access granted through Tailscale for this single user. Public hosting and sharing access with other users are outside v1.
- Keep the browser's same-origin boundary for mutation endpoints: check `Origin` / `Sec-Fetch-Site` on every POST. This prevents another website from issuing actions through the user's browser; it does not add an application login.
- Login and harness configuration stay in the harness CLIs. Surface installation and auth errors in the app.
