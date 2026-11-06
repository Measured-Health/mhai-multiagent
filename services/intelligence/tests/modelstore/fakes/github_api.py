"""A fake GitHub REST API behind a fake HTTP session, for ``lock.GitHubClient``.

:class:`FakeGitHubSession` answers the two GET requests the client makes, with GitHub's
response shapes: ``/repos/{repo}/pulls/{n}`` (a pull request: ``number``, ``user.login``,
``head.sha``, ``base.ref``, ``labels``, ``body``) and ``/repos/{repo}/pulls/{n}/reviews``
(a page of reviews: ``id``, ``user.login``, ``state``, ``commit_id``), paged by
``per_page`` and ``page`` with a ``Link: <...>; rel="next"`` header that names the
``/repositories/<id>/...`` form, as GitHub does. ``max_per_page`` serves fewer per page
than asked (as a server setting may). Logins are served in display case, as GitHub does. It carries a bearer token in
its headers, as the real session does, so a test can check that the token never reaches
a log or an error. Nothing here opens a socket. Every login and body is synthetic.
"""

from __future__ import annotations

import json
import secrets
from dataclasses import dataclass, field
from typing import Any

from tests.modelstore.fakes.drive_api import FakeResponse as _DriveResponse

REPO = "synthetic-org/synthetic-repo"
API = "https://api.github.com"
REPO_ID = 424242
NULL = object()  # a review's commit_id served as null


class FakeResponse(_DriveResponse):
    def __init__(self, status_code: int, body: bytes, headers: dict[str, str] | None = None):
        super().__init__(status_code, body)
        self.headers = dict(headers or {})


@dataclass
class FakePR:
    number: int
    author: str
    head_sha: str
    labels: list[str] = field(default_factory=list)
    body: str | None = None
    reviews: list[dict[str, Any]] = field(default_factory=list)
    base_ref: str = "main"

    def resource(self) -> dict[str, Any]:
        return {
            "number": self.number,
            "state": "open",
            "user": {"login": self.author, "type": "User"},
            "head": {"sha": self.head_sha, "ref": "agent/synthetic"},
            "base": {"sha": "0" * 40, "ref": self.base_ref},
            "labels": [{"name": n, "color": "ededed"} for n in self.labels],
            "body": self.body,
        }

    def review(self, login: str | None, state: str, commit_id: Any = None) -> None:
        if commit_id is NULL:
            commit: Any = None
        else:
            commit = self.head_sha if commit_id is None else commit_id
        self.reviews.append(
            {
                "id": 1000 + len(self.reviews),
                "user": None if login is None else {"login": login, "type": "User"},
                "state": state,
                "commit_id": commit,
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
        self.max_per_page = 100
        self.next_base: str | None = None  # serve next links on another base URL

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
        prefixes = (f"{API}/repos/{self.repo}/pulls/", f"{API}/repositories/{REPO_ID}/pulls/")
        prefix = next((p for p in prefixes if url.startswith(p)), None)
        if prefix is None:
            return FakeResponse(404, b'{"message":"Not Found"}')
        rest = url[len(prefix) :].split("/")
        pr = self.prs.get(int(rest[0])) if rest[0].isdigit() else None
        if pr is None:
            return FakeResponse(404, b'{"message":"Not Found"}')
        if len(rest) == 1:
            return FakeResponse(200, json.dumps(pr.resource()).encode())
        if rest[1:] == ["reviews"]:
            per_page = min(int(params.get("per_page", "30")), self.max_per_page)
            page = int(params.get("page", "1"))
            items = pr.reviews[(page - 1) * per_page : page * per_page]
            headers = {}
            if len(pr.reviews) > page * per_page:
                base = self.next_base or f"{API}/repositories/{REPO_ID}"
                nxt = f"{base}/pulls/{pr.number}/reviews?per_page={per_page}&page={page + 1}"
                last = f"{API}/repositories/{REPO_ID}/pulls/{pr.number}/reviews?page=999"
                headers["Link"] = f'<{nxt}>; rel="next", <{last}>; rel="last"'
            return FakeResponse(200, json.dumps(items).encode(), headers)
        return FakeResponse(404, b'{"message":"Not Found"}')
