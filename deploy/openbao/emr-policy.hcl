# The emr service's policy (EMR design §6.1): its own environment's EMR
# secrets and nothing else. @ENV@ is replaced by configure.sh with the
# environment (production on the live host). `+` is one path segment:
# hospital, connection, field. No list beyond one connection's field
# names (wipe_credentials needs them); no sys/, no other mount, no other
# environment.

# Read, write and the version reads of one field.
path "mhai/data/@ENV@/emr/+/+/+" {
  capabilities = ["create", "read", "update"]
}

# A field's versions (their times, for the 24 h keep) and its removal.
path "mhai/metadata/@ENV@/emr/+/+/+" {
  capabilities = ["read", "delete"]
}

# One connection's field names: wipe_credentials removes each.
path "mhai/metadata/@ENV@/emr/+/+/" {
  capabilities = ["list"]
}

# Dropping one version for good (a rolled-back put, or older than 24 h).
path "mhai/destroy/@ENV@/emr/+/+/+" {
  capabilities = ["update"]
}

# Its own token's lookup (the service's readiness check). The role sets
# token_no_default_policy: nothing of the default policy is granted.
path "auth/token/lookup-self" {
  capabilities = ["read"]
}

# Ending its own token early (a one-off login: the readiness check, a
# snapshot).
path "auth/token/revoke-self" {
  capabilities = ["update"]
}
