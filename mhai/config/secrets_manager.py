"""
Secrets Manager — Local .env / AWS Secrets Manager / Azure Key Vault switch.
"""
import logging
import os

log = logging.getLogger(__name__)

def get_secret(name: str, default: str = "") -> str:
    """Retrieve a secret from the appropriate backend."""
    env = os.environ.get("APP_ENV", "local")
    if env == "local":
        return os.environ.get(name, default)
    elif env in ("dev", "staging", "prod"):
        # AWS Secrets Manager or Azure Key Vault
        try:
            import boto3
            client = boto3.client("secretsmanager")
            resp = client.get_secret_value(SecretId=name)
            return resp["SecretString"]
        except Exception:
            log.warning(f"Secrets Manager unavailable for {name}; falling back to env")
            return os.environ.get(name, default)
    return os.environ.get(name, default)
