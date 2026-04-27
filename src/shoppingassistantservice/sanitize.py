#!/usr/bin/python
#
# Copyright 2024 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Input sanitization for the shopping assistant service.

Defends against prompt injection (R-020) and SSRF via the image URL by
validating sizes and shapes before any LLM call is constructed. Sanitization
runs at the HTTP boundary; downstream code can assume inputs are bounded
and well-formed.
"""

from __future__ import annotations

import ipaddress
import re
import socket
from urllib.parse import urlparse

# Cap the user message at a length that fits comfortably in the Gemini
# context window alongside system instructions, the room description, and
# retrieved docs. Anything longer is almost certainly an attempt to flood
# the context or exfiltrate via prompt-injection payloads.
MAX_MESSAGE_CHARS = 2000

# Browser address bars typically tolerate URLs up to ~2KB; anything beyond
# that is suspicious for an image reference.
MAX_IMAGE_URL_CHARS = 2048

# Strip ASCII control characters (U+0000..U+001F) except for whitespace
# (\t, \n, \r). Keeps the message readable while removing escape sequences
# that some LLM prompt-injection chains rely on.
_CONTROL_CHARS = re.compile(r"[\x00-\x08\x0B\x0C\x0E-\x1F\x7F]")


class SanitizationError(ValueError):
    """Raised when input fails sanitization. Callers should map to HTTP 400."""


def sanitize_message(raw: object) -> str:
    """Validate and clean a user-supplied chat message.

    Returns the cleaned string. Raises SanitizationError if the input is
    missing, the wrong type, empty after stripping, exceeds the length cap,
    or contains content that cannot be made safe (length still over cap
    after control-char stripping).
    """
    if raw is None:
        raise SanitizationError("message is required")
    if not isinstance(raw, str):
        raise SanitizationError("message must be a string")

    cleaned = _CONTROL_CHARS.sub("", raw).strip()
    if not cleaned:
        raise SanitizationError("message must not be empty")
    if len(cleaned) > MAX_MESSAGE_CHARS:
        raise SanitizationError(
            f"message exceeds maximum length of {MAX_MESSAGE_CHARS} characters"
        )
    return cleaned


def sanitize_image_url(raw: object) -> str:
    """Validate that the image URL is a public https:// resource.

    Returns the URL unchanged if valid. Raises SanitizationError on:
      - missing or non-string input
      - length exceeding MAX_IMAGE_URL_CHARS
      - non-https scheme
      - missing or unparseable hostname
      - hostname resolving to a loopback, private, link-local, or
        otherwise non-public IP (SSRF defense)

    Hostnames that don't resolve are accepted (the LLM will fail the fetch
    and degrade gracefully); only confirmed-private targets are rejected.
    """
    if raw is None:
        raise SanitizationError("image is required")
    if not isinstance(raw, str):
        raise SanitizationError("image must be a string URL")
    if len(raw) > MAX_IMAGE_URL_CHARS:
        raise SanitizationError(
            f"image URL exceeds maximum length of {MAX_IMAGE_URL_CHARS} characters"
        )

    parsed = urlparse(raw)
    if parsed.scheme != "https":
        raise SanitizationError("image URL must use https://")
    if not parsed.hostname:
        raise SanitizationError("image URL must include a hostname")

    if _hostname_is_private(parsed.hostname):
        raise SanitizationError("image URL must point to a public host")
    return raw


def _hostname_is_private(hostname: str) -> bool:
    """Return True if hostname is, or resolves only to, a non-public address.

    Catches IP literals (most direct SSRF attempts) and DNS names that
    resolve to loopback / RFC1918 / link-local space. Resolution failures
    are treated as not-private; the LLM will simply fail to fetch.
    """
    try:
        ip = ipaddress.ip_address(hostname)
    except ValueError:
        ip = None

    if ip is not None:
        return _ip_is_private(ip)

    try:
        infos = socket.getaddrinfo(hostname, None)
    except socket.gaierror:
        return False  # let the downstream fetch fail naturally

    for info in infos:
        sockaddr = info[4]
        addr = sockaddr[0]
        try:
            ip = ipaddress.ip_address(addr)
        except ValueError:
            continue
        if _ip_is_private(ip):
            return True
    return False


def _ip_is_private(ip: ipaddress._BaseAddress) -> bool:
    return (
        ip.is_private
        or ip.is_loopback
        or ip.is_link_local
        or ip.is_reserved
        or ip.is_multicast
        or ip.is_unspecified
    )
