# Security

## Reporting a vulnerability

Contact the project's owner privately. Please do not open a public issue. Include how to reproduce it and what an attacker
could do with it. (Owner: add a dedicated security contact here before publishing this repository.)

## What is protected, and how

| Asset | Protection |
| --- | --- |
| Passwords | Argon2id with a per-user salt |
| API secrets | AES-256-GCM at rest. The key comes from the environment and never reaches the database. Shown once when created |
| Session cookies, email tokens | Random 256-bit values, stored as hashes. Cookies are `HttpOnly` and `SameSite=Lax` |
| Access tokens | HS256 only, with a required expiry. `alg: none` and other algorithms are rejected |
| Media | DTLS-SRTP on every connection. There is no unencrypted mode. The server can read media, because an SFU has to route it: this is not end-to-end encryption |
| Tenants | Rooms, keys and data are scoped to a project, and asking for another project's data answers 404 |
| Webhooks | Signed. Private addresses are refused at save time and again when connecting, which also stops DNS tricks |
| TURN | Short-lived credentials, and no relaying to private networks |

Login and password reset responses do not reveal which emails have accounts. State-changing requests need a JSON content
type, which a cross-site form cannot send. Login attempts are rate limited per address. Security-relevant actions go to an audit log.

## Before you deploy

- Serve everything over TLS. Browsers require it for cameras anyway.
- Keep ports 7880, 5432 and 6379 off the public internet. Only the ports in
  [the firewall table](docs/deployment/self-hosting.md#ports-and-firewall) should be open.
- Set `secure_cookies: true`. Set `trust_proxy_headers: true` only behind a proxy that overwrites `X-Forwarded-For`,
  or clients can dodge rate limits and forge audit entries.
- Register straight after installing. The first account becomes the platform administrator.
- Back up `STREAMKIT_MASTER_KEY`, separately from database dumps.

## Known limits

- A removed participant can rejoin until their token expires, so keep join tokens short-lived for rooms where this matters.
  Revocation on removal is on the roadmap.
- A revoked API key can keep working on other nodes for up to 30 seconds because of caching. It stops at once on the node that handled the
  request.
- There is no end-to-end encryption yet.
- No external security audit has been done.
