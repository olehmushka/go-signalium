# Security policy

## Supported versions

go-signalium follows a rolling-release model: only the latest `main` and the most recent tagged release receive security fixes.

| Version | Supported |
|---|---|
| `main` (HEAD) | ✅ |
| Most recent tag | ✅ |
| Older tags | ❌ — upgrade |

## Reporting a vulnerability

**Do not open a public GitHub issue for security problems.**

Email the maintainer with:

- A description of the vulnerability and the impact (confidentiality / integrity / availability).
- The smallest reproducer you can manage — a request, a config, a sequence of calls.
- The version (commit SHA or tag) you observed it on.
- Optionally, a proposed fix.

Use [GitHub Private Vulnerability Reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability) on this repository if you prefer a tracked channel.

### What to expect

- **Acknowledgement** within 5 business days.
- **Triage and a preliminary severity assessment** within 10 business days.
- **A patched release** as soon as a fix is validated; the timeline depends on severity and complexity. Critical issues are prioritised above other work.
- **Credit in the release notes** if you want it (default: anonymous).

Please give us a reasonable window to issue a fix before public disclosure. Ninety days is the customary expectation; faster is appreciated for low-severity issues, longer is acceptable for unusually complex ones — we will agree on a date when we acknowledge the report.

## Out of scope

The following do not warrant a security advisory on their own. Open a regular issue or PR instead:

- Misconfiguration or operator error in someone's own deployment (e.g., leaving `signalCli.tcp.ignoreResults: true` exposed to untrusted callers).
- Vulnerabilities in third-party services we depend on (Postgres, MinIO, `signal-cli`) — report those upstream.
- Theoretical issues with no realistic exploitation path against go-signalium's actual behaviour.

If you're unsure whether something qualifies, err on the side of reporting it privately.

## Hardening notes for operators

- **Sender phone is server-side.** Callers cannot impersonate a different sender — the server stamps `senderPhoneNumber` from `install.yml`. If a request includes a mismatched value the server returns `409 SenderMismatch`.
- **Multipart endpoint validates filenames.** Path-traversal sequences are rejected; only `[A-Za-z0-9._-]` survives. Total request size is bounded by `server.maxRequestBodySize`.
- **Slack webhooks and DB credentials** should be mounted from file references, not inlined in `runtime.yml` / `install.yml`. Witchcraft supports `{file:/path/to/secret}` for any field.
- **`signal-cli`'s `SIGNAL_CLI_TRUST_NEW_IDENTITIES=always`** is set for development convenience in `deploy/entrypoint_signal_cli.sh`. Tighten this for production deployments.
- **Postgres advisory lock + auto-migration** assume a single trusted operator surface. Do not expose the management port (`server.management-port`, default `8084`) to untrusted networks.
