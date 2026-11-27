# The backup's policy (deploy/ops/backup.sh): a raft snapshot, nothing
# else. The snapshot is encrypted by OpenBao's barrier; backup.sh encrypts
# it again with age to the backup recipients.
path "sys/storage/raft/snapshot" {
  capabilities = ["read"]
}

path "auth/token/lookup-self" {
  capabilities = ["read"]
}

# Ending its own token early (a one-off login: the readiness check, a
# snapshot).
path "auth/token/revoke-self" {
  capabilities = ["update"]
}
