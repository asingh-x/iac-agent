Drop an extra CA certificate here (as a `.crt` file) if `make sandbox-build`
fails with a TLS verification error — this usually means you're behind a
corporate TLS-inspecting proxy whose root CA isn't in the base image's trust
store. Files here are picked up by the Dockerfile's `update-ca-certificates`
step and are gitignored (never committed).
