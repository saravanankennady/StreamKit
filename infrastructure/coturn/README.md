# TURN

The production Compose file runs coturn with short-lived credentials from a shared secret. The server mints one for
each client from `STREAMKIT_TURN_SECRET`, so nothing needs adding to a project.

A stock Docker-published port range is slow with thousands of relay ports, so coturn uses the host network.
Relay ports are 49160-49800/udp; see the firewall table in `docs/deployment/self-hosting.md`.

coturn is told to refuse relaying to private networks. Without those lines, anyone with TURN credentials could
reach your internal services through it.
