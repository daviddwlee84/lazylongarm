# Ephemeral cloud workers

Status: idea. Priority P3. Colab shipped as `worker colab add|ls|rm`
(internal/flows/colabworker.go): session named after the worker, verified
binary upload, owner code as a file, background `longarmd run`,
`provider=colab` label as the guard for `rm`. Other providers can follow
that shape; the gaps below (no worker TTL, no way to keep auto-placed jobs
off a disposable worker) still apply.

Longarm supports the pieces: `install.sh --no-service`/foreground `longarmd run`,
`LONGARM_DATA_DIR`, non-interactive `longarmd join`, `longarmd leave`. Missing:
provisioning, lifecycle and cleanup. Join-code TTL is enrollment validity, not
worker lifetime.

Sketch: `lazylongarm worker add --cloud <provider> --ttl 4h` → provision
(az/RunPod API) → cloud-init with a 15-minute owner code passed via the
provider's secret channel → `--service user` → label `pool=cloud` → on expiry
`longarm worker remove` + destroy. Prefer a separate hub (or at least labels)
for disposable machines so they never share a trust root with private data.
