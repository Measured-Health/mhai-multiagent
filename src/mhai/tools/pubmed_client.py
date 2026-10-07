"""PubMed API client for guideline retrieval."""
import logging
import requests
from typing import List, Dict

log = logging.getLogger(__name__)

class PubMedClient:
    """NCBI E-utilities client (free, up to 10 req/sec with API key)."""
    BASE_URL = "https://eutils.ncbi.nlm.nih.gov/entrez/eutils"

    def search(self, query: str, max_results: int = 10) -> List[Dict]:
        """Search PubMed and return article metadata."""
        try:
            resp = requests.get(f"{self.BASE_URL}/esearch.fcgi", params={
                "db": "pubmed", "term": query, "retmax": max_results, "retmode": "json"
            }, timeout=30)
            data = resp.json()
            ids = data.get("esearchresult", {}).get("idlist", [])
            log.info(f"PubMed search '{query}': {len(ids)} results")
            return [{"pmid": pid} for pid in ids]
        except Exception as e:
            log.warning(f"PubMed search failed: {e}")
            return []
