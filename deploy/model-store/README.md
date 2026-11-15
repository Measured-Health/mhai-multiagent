# The model store: multipart uploads

The model store is any S3-compatible bucket with two users, a writer and a reader
(`services/intelligence/intelligence/modelstore/store.py`; the MinIO policies are in
`deploy/minio/policies/`). This note covers files over 5 GiB. Task 27 adds the provisioning
for the production store here.

## Files over 5 GiB

One PUT is at most 5 GiB. Gemma 4's per-layer embedding alone is a 5.6 GB tensor, so its
merged safetensors can't go up in one PUT. A larger file, up to 50 GiB, goes up as a
**conditional multipart upload**:

1. The key must still be free before any part is sent.
2. Each part carries its SHA-256 (or `Content-MD5` where the store won't take SHA-256), so
   the store refuses a part that changed on the way.
3. The whole file's SHA-256, over exactly the bytes sent, must be the one the run's files
   hash recorded. Otherwise the upload is not completed.
4. The upload is completed with `If-None-Match: *`, so an object that appeared in the
   meantime is refused.
5. The size and checksum of what the store kept are read back (or the whole object, if the
   store keeps no checksum of the parts). The key must have exactly that one version.
   Promote then reads the object back in full.

Any failure before completion aborts the upload. Only the writer's credential uploads, as
before. The load-time verifier hashes every byte of every file against the signed manifest,
multipart or not.

### Turning it on

Multipart is **off** unless the writer runs with `MODEL_STORE_CONDITIONAL_MULTIPART=1`.
Without it, a file over 5 GiB is refused (`too_large`, `file_too_large_for_store`). It is
never written unconditionally.

A store that ignored `If-None-Match` on completion would let an upload overwrite a version,
so check the store first, as the writer:

```bash
MODEL_STORE_ENDPOINT=... MODEL_STORE_BUCKET=... \
MODEL_STORE_WRITER_ACCESS_KEY=... MODEL_STORE_WRITER_SECRET_KEY=... \
  uv run mhai-models store-probe-multipart
# {"conditional_multipart": true}    exit 0: the store refused the overwrite
```

Set the repository variable `MODEL_STORE_CONDITIONAL_MULTIPART` to `1` only after it prints
`true` (`model-promote.yml` passes it to the promote step). Run the probe again whenever the
store or its provider changes. The probe writes one small object under
`models/_store-probe/<random>/` (two versions of it on a store that fails). MinIO and AWS S3
pass; the CI rehearsal store (MinIO) runs with the setting on.

### The writer's permissions

On top of `s3:PutObject` (which covers create, upload part and complete), the writer needs
`s3:AbortMultipartUpload` and `s3:ListMultipartUploadParts` on `models/*` and `eval/*`, and
`s3:ListBucketMultipartUploads` on the bucket (for the cleanup below). The reader is denied
`s3:AbortMultipartUpload` and has no list-uploads permission. Neither can delete an object.

## Incomplete uploads

An upload whose job was killed between its first part and its completion leaves parts that
are stored, and billed, but are not an object. No reader can see them, and no manifest can
name them. Clean them up in two ways:

- **A lifecycle rule** on the production bucket (AWS S3 and most S3-compatible stores),
  set once by the bucket's administrator:

  ```json
  {
    "Rules": [
      {
        "ID": "abort-incomplete-model-uploads",
        "Status": "Enabled",
        "Filter": { "Prefix": "models/" },
        "AbortIncompleteMultipartUpload": { "DaysAfterInitiation": 1 }
      },
      {
        "ID": "abort-incomplete-eval-uploads",
        "Status": "Enabled",
        "Filter": { "Prefix": "eval/" },
        "AbortIncompleteMultipartUpload": { "DaysAfterInitiation": 1 }
      }
    ]
  }
  ```

  `aws s3api put-bucket-lifecycle-configuration --bucket <bucket> --lifecycle-configuration file://rule.json`.
  The rule has no expiration action, so it never removes an object. Neither store user may
  change the lifecycle configuration (`NeverChangeTheBucket`).

- **The CLI**, as the writer, for a store without lifecycle rules or to clean up now:

  ```bash
  uv run mhai-models store-abort-stale-uploads --older-than-hours 24
  # {"stale_uploads_aborted": 2}
  ```

  It aborts uploads under `models/` and `eval/` that started more than N hours ago (at
  least 6, so a running promotion is never cut off). It never touches an object.

MinIO lists uploads only for an exact key, so on MinIO the CLI finds nothing. MinIO
removes stale uploads itself (`api stale_uploads_expiry`, 24 hours by default).
