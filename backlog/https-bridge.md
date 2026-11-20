# HTTPS bridge for Anthropic-hosted Claude sessions

Status: idea. Priority P3.

Anthropic-hosted sandboxes only egress HTTP(S) through a (TLS-terminating)
proxy; Longarm needs raw TCP 7443 + mTLS (no HTTP carrier, custom dialer
ignores HTTPS_PROXY). A bridge would expose a few authorized actions (submit,
wait, logs, kill) over HTTPS: either a remote MCP connector (traffic goes from
Anthropic's side, not the session allowlist) or a bearer-token API on 443
stored as a network secret. It changes the trust boundary the same way the
web-preview gateway (#159) does; keep it a separate, opt-in component.

## LAN variant

A smaller step: wrap `longarm mcp` (stdio only today) with a stdio-to-HTTP
bridge on one machine of a trusted LAN, behind TLS and authentication, for
local agents (Claude Code, Copilot CLI) on machines that should not hold a
client certificate. Its real value over enrolling each machine is narrowing:
a client certificate grants everything its user may do, while a gateway can
expose only `fleet_status`, `fleet_job_submit` (pinned to a label pool),
`wait`, `logs` and `kill`.

- claude.ai, mobile and Desktop custom connectors call from Anthropic's side
  and cannot reach a LAN-only server; this serves local agents only.
- Every caller acts as the gateway's identity: enroll it as its own non-admin
  user, log callers in the gateway, and limit it with the workers' `[access]`.
- `fleet_exec` / `fleet_job_submit` are remote code execution: a leaked token
  is code execution on the fleet for anyone on the LAN. TLS, a bound
  interface, short-lived tokens, `Origin`/`Host` checks (DNS rebinding).
- Run it with `LONGARM_MCP_SECRETS` empty and `LONGARM_MCP_AGENT_AUTH=none`,
  or its environment's tokens become everyone's.
