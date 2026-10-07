"""Per-agent URL + timeout configuration."""
import os

AGENT_URLS = {
    "DIA": os.environ.get("DIA_URL", "http://dia_agent:8010"),
    "QDA": os.environ.get("QDA_URL", "http://qda_agent:8011"),
    "CCA": os.environ.get("CCA_URL", "http://cca_agent:8012"),
    "HGA": os.environ.get("HGA_URL", "http://hga_agent:8013"),
    "ORCH": os.environ.get("ORCH_URL", "http://orchestrator_agent:8015"),
}

AGENT_TIMEOUT = int(os.environ.get("AGENT_HTTP_TIMEOUT", "300"))
AGENT_RETRIES = int(os.environ.get("AGENT_HTTP_RETRIES", "2"))
