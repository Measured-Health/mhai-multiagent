"""Cloud model providers (docs/plans/2026-11-06-cloud-model-providers.md).

Off by default: with INTELLIGENCE_CLOUD_PROVIDERS empty nothing here sends
anything anywhere. Keys are read from files only and held as ApiKey, whose
repr and str are redacted. Only this package imports the providers' SDKs.
"""
