# HTTPS bridge for Anthropic-hosted Claude sessions

Status: idea. Priority P3.

Anthropic-hosted sandboxes only egress HTTP(S) through a (TLS-terminating)
proxy; Longarm needs raw TCP 7443 + mTLS (no HTTP carrier, custom dialer
ignores HTTPS_PROXY). A bridge would expose a few authorized actions (submit,
wait, logs, kill) over HTTPS: either a remote MCP connector (traffic goes from
Anthropic's side, not the session allowlist) or a bearer-token API on 443
stored as a network secret. It changes the trust boundary the same way the
web-preview gateway (#159) does; keep it a separate, opt-in component.
