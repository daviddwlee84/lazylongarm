# Ephemeral cloud workers

Status: idea. Priority P3.

Longarm supports the pieces: `install.sh --no-service`/foreground `longarmd run`,
`LONGARM_DATA_DIR`, non-interactive `longarmd join`, `longarmd leave`. Missing:
provisioning, lifecycle and cleanup. Join-code TTL is enrollment validity, not
worker lifetime.

Sketch: `lazylongarm worker add --cloud <provider> --ttl 4h` → provision
(az/RunPod API) → cloud-init with a 15-minute owner code passed via the
provider's secret channel → `--service user` → label `pool=cloud` → on expiry
`longarm worker remove` + destroy. Prefer a separate hub (or at least labels)
for disposable machines so they never share a trust root with private data.
