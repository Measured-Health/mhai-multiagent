# OpenBao, the emr service's secret store (deploy/README.md, "OpenBao (the
# EMR secret store)"; EMR design §6.1, H3). Mounted read-only at
# /openbao/config by compose.openbao.yml; the integration test of the emr
# service's OpenBao backend starts the server with this same file
# (services/emr/internal/secrets/openbao_integration_test.go).

# Integrated storage (raft), one node, on the openbao volume
# (/openbao/file). Encrypted at rest by OpenBao's barrier: the volume alone
# reveals nothing without a quorum of the unseal key shares.
storage "raft" {
  path    = "/openbao/file/raft"
  node_id = "openbao-1"
}

# The emr service's listener: TLS only, our private CA, and a client
# certificate from that CA required (emr's own, tls/emr). Reached only on
# the internal openbao network; no port is published.
listener "tcp" {
  address                            = "0.0.0.0:8200"
  tls_cert_file                      = "/openbao/tls/server.pem"
  tls_key_file                       = "/openbao/tls/server-key.pem"
  tls_client_ca_file                 = "/openbao/tls/ca.pem"
  tls_require_and_verify_client_cert = true
  tls_min_version                    = "tls13"
}

# The operator's listener: loopback inside this container's own network
# namespace, reached only through `docker exec` (enable-openbao.sh), which
# already needs root-equivalent rights on the host. No TLS on it: nothing
# else shares this namespace. Only here may a quorum of unseal key shares
# make a short-lived root token (sys/generate-root, enable-openbao.sh
# --rotate-credentials); the emr listener keeps OpenBao's default (off).
listener "tcp" {
  address                                  = "127.0.0.1:8201"
  tls_disable                              = true
  disable_unauthed_generate_root_endpoints = false
}

api_addr     = "https://openbao:8200"
cluster_addr = "https://127.0.0.1:8202"

# Every request and response, HMAC'd (no secret value in clear), to a file
# on the volume. If the audit device can't be written OpenBao refuses the
# request: the store fails closed.
audit "file" "file" {
  options = {
    file_path = "/openbao/file/audit.log"
    mode      = "0600"
  }
}

ui        = false
log_level = "info"
