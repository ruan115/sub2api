# Explicit worker egress

`New` requires a canonical, credential-free
`http://host-agent.execution.internal:<port>` origin. Every request, including a
loopback target, uses this proxy. Ambient HTTP/HTTPS/ALL_PROXY and NO_PROXY do
not select the route, and a failed proxy connection never requests a direct dial.
Each call creates a separate connection pool and verifying TLS configuration;
there is no injected client private key, key logger or shared TLS session cache.

The worker process requires `EXECUTION_EGRESS_PROXY_URL` before starting its RPC
listener. Docker bootstrap supplies it from the existing SlotSpec network
policy. A real process requires HTTPS execution and onboarding endpoints; only
the explicit synthetic activation mode permits HTTP. Injectable library tests
can still use their own transports. Execution and onboarding retain their
existing no-redirect behavior, and process exit closes idle connections.

Tests combine the actual transport with a loopback CONNECT proxy and TLS origin,
using ephemeral test certificates and synthetic authentication. They cover SNI,
TLS 1.3 negotiation, unknown CA, wrong SAN, expiry, obsolete TLS rejection and no
direct dial during proxy failure. Proxy/CA test adapters do not enter process
configuration. Provider and worker URL acceptance also have a shared-contract
regression.

This is an application egress boundary, not a kernel firewall, a VM provider or
per-runtime certificate provisioning. It cannot stop arbitrary code from using
a different socket API; DNS/IPv6/metadata/host-port/other-slot isolation needs
the separate Linux network gate. The target TLS handshake is independent of
TLS to an HTTPS upstream proxy. Public HTTP/SOCKS proxy transport protection,
worker RPC mTLS, credential/lease revocation and production assembly remain
unverified. No production account or external model endpoint is used in tests.
