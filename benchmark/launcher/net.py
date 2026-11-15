"""URL and proxy rewriting between the host and containers."""

from __future__ import annotations

import ipaddress
import urllib.parse
from typing import Mapping

PROXY_VARIABLES = ("HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy")
NO_PROXY_HOSTS = ("localhost", "127.0.0.1", "host.docker.internal")


def is_loopback_host(hostname: str | None) -> bool:
    """True for localhost and any loopback IP (127.0.0.0/8, ::1)."""
    name = (hostname or "").lower()
    if name == "localhost":
        return True
    try:
        return ipaddress.ip_address(name).is_loopback
    except ValueError:
        return False


def _with_host(parsed: urllib.parse.SplitResult, host: str, *, userinfo: bool) -> str:
    """netloc of ``parsed`` with its host replaced, keeping the port."""
    netloc = f"[{host}]" if ":" in host else host
    if parsed.port:
        netloc += f":{parsed.port}"
    if userinfo:
        info, _, _ = parsed.netloc.rpartition("@")
        if info:
            netloc = f"{info}@{netloc}"
    return netloc


def host_side_url(endpoint: str, host: str = "127.0.0.1") -> str:
    """Rewrite a URL for a probe made from the host.

    host.docker.internal and loopback hosts are replaced by ``host``: pass the
    address containers actually reach this computer at (the docker0 gateway on
    Linux), so a host-side probe fails exactly when a container's would.
    """
    parsed = urllib.parse.urlsplit(endpoint)
    hostname = (parsed.hostname or "").lower()
    if hostname != "host.docker.internal" and not is_loopback_host(hostname):
        return endpoint
    netloc = _with_host(parsed, host, userinfo=False)
    return urllib.parse.urlunsplit((parsed.scheme, netloc, parsed.path, "", ""))


def container_side_url(endpoint: str) -> str:
    """Rewrite loopback hosts to host.docker.internal for use inside a container."""
    parsed = urllib.parse.urlsplit(endpoint)
    if not is_loopback_host(parsed.hostname):
        return endpoint
    netloc = _with_host(parsed, "host.docker.internal", userinfo=True)
    return urllib.parse.urlunsplit((parsed.scheme, netloc, parsed.path, parsed.query, parsed.fragment))


def container_proxy(value: str) -> str:
    """Rewrite a loopback proxy URL (with or without scheme) for use in a container."""
    text = str(value).strip()
    has_scheme = "://" in text
    rewritten = container_side_url(text if has_scheme else f"http://{text}")
    return rewritten if has_scheme else rewritten.split("://", 1)[1]


def host_proxy_args(environ: Mapping[str, str]) -> dict[str, str]:
    """Host proxy variables for builds and containers.

    A proxy on the host's loopback is not reachable from a container, so it is
    rewritten to host.docker.internal; NO_PROXY always lists the local hosts.
    """
    args = {}
    for name in PROXY_VARIABLES:
        value = environ.get(name)
        if value:
            args[name] = value if name.lower() == "no_proxy" else container_proxy(value)
    if args:
        no_proxy = no_proxy_value(args.get("NO_PROXY") or args.get("no_proxy") or "")
        args["NO_PROXY"] = args["no_proxy"] = no_proxy
    return args


def no_proxy_value(existing: str) -> str:
    items = list(NO_PROXY_HOSTS)
    for item in str(existing or "").split(","):
        item = item.strip()
        if item and item not in items:
            items.append(item)
    return ",".join(items)
