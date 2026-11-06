"""A fake GitHub REST API behind a fake HTTP session, for ``lock.GitHubClient``.

:class:`FakeGitHubSession` answers the two GET requests the client makes, with GitHub's
response shapes: ``/repos/{repo}/pulls/{n}`` (a pull request: ``number``, ``user.login``,
``head.sha``, ``labels``, ``body``) and ``/repos/{repo}/pulls/{n}/reviews`` (a page of
reviews: ``id``, ``user.login``, ``state``, ``commit_id``), paged by ``per_page`` and
``page``. Logins are served in display case, as GitHub does. It carries a bearer token in
its headers, as the real session does, so a test can check that the token never reaches
a log or an error. Nothing here opens a socket. Every login and body is synthetic.
"""

from __future__ import annotations

import json
import secrets
from dataclasses import dataclass, field
from typing import Any

from tests.modelstore.fakes.drive_api import FakeResponse

REPO = "synthetic-org/synthetic-repo"
API = "https://api.github.com"


@dataclass
class FakePR:
    number: int
    author: str
    head_sha: str
    labels: list[str] = field(default_factory=list)
    body: str | None = None
    reviews: list[dict[str, Any]] = field(default_factory=list)

    def resource(self) -> dict[str, Any]:
        return {
            "number": self.number,
            "state": "open",
            "user": {"login": self.author, "type": "User"},
            "head": {"sha": self.head_sha, "ref": "agent/synthetic"},
            "base": {"sha": "0" * 40, "ref": "main"},
            "labels": [{"name": n, "color": "ededed"} for n in self.labels],
            "body": self.body,
        }

    def review(self, login: str | None, state: str, commit_id: str | None = None) -> None:
        self.reviews.append(
            {
                "id": 1000 + len(self.reviews),
                "user": None if login is None else {"login": login, "type": "User"},
                "state": state,
                "commit_id": self.head_sha if commit_id is None else commit_id,
                "body": "",
            }
        )


@dataclass
class Request:
    url: str
    params: dict[str, str]
    allow_redirects: Any


class FakeGitHubSession:
    def __init__(self, repo: str = REPO) -> None:
        self.token = "ghs_fake" + secrets.token_hex(16)
        self.headers = {"Authorization": f"Bearer {self.token}"}
        self.repo = repo
        self.prs: dict[int, FakePR] = {}
        self.requests: list[Request] = []
        self.status: int | None = None  # every response's status, when set
        self.raise_error = False  # every request raises, its message holding the token

    def add(self, pr: FakePR) -> FakePR:
        self.prs[pr.number] = pr
        return pr

    def get(self, url: str, **kw: Any) -> FakeResponse:
        params = {k: str(v) for k, v in (kw.get("params") or {}).items()}
        self.requests.append(Request(url, params, kw.get("allow_redirects")))
        if self.raise_error:
            raise ConnectionError(f"failed {url} with {self.headers['Authorization']}")
        if self.status is not None:
            return FakeResponse(self.status, b'{"message":"synthetic"}')
        prefix = f"{API}/repos/{self.repo}/pulls/"
        if not url.startswith(prefix):
            return FakeResponse(404, b'{"message":"Not Found"}')
        rest = url[len(prefix) :].split("/")
        pr = self.prs.get(int(rest[0])) if rest[0].isdigit() else None
        if pr is None:
            return FakeResponse(404, b'{"message":"Not Found"}')
        if len(rest) == 1:
            return FakeResponse(200, json.dumps(pr.resource()).encode())
        if rest[1:] == ["reviews"]:
            per_page = int(params.get("per_page", "30"))
            page = int(params.get("page", "1"))
            items = pr.reviews[(page - 1) * per_page : page * per_page]
            return FakeResponse(200, json.dumps(items).encode())
        return FakeResponse(404, b'{"message":"Not Found"}')
