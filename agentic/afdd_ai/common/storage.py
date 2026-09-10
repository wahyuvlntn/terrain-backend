"""Private RAG document store: MinIO (local) or Google Cloud Storage (cloud).

The bucket is never served under `/public`; the backend streams originals to
users after its permission checks. `STORAGE_PROVIDER=gcs` uses the attached
service account (metadata server) and `GCS_DOCS_BUCKET`; a GCS emulator
(`STORAGE_EMULATOR_HOST`) is used without credentials and its bucket is created.
"""

from __future__ import annotations

import asyncio
import io
import logging
from functools import lru_cache
from urllib.parse import quote

import httpx

from . import gcp
from .config import get_settings

log = logging.getLogger(__name__)


class ObjectNotFound(Exception):
    """The object does not exist."""

    def __init__(self, key: str, code: str = "NoSuchKey") -> None:
        super().__init__(f"{key}: {code}")
        self.key, self.code = key, code


def _gcs() -> bool:
    return get_settings().storage_provider == "gcs"


# ---------------------------------------------------------------------------
# MinIO


@lru_cache(maxsize=1)
def _client():  # noqa: ANN202 - minio is only imported when used
    from minio import Minio

    s = get_settings()
    return Minio(s.minio_endpoint, access_key=s.minio_access_key, secret_key=s.minio_secret_key, secure=s.minio_secure)


# ---------------------------------------------------------------------------
# Google Cloud Storage (JSON API)


def _gcs_base() -> tuple[str, bool]:
    endpoint = get_settings().gcs_endpoint.strip().rstrip("/")
    if endpoint:
        return (endpoint if "://" in endpoint else f"http://{endpoint}"), True
    return "https://storage.googleapis.com", False


def _gcs_headers(emulator: bool) -> dict[str, str]:
    return {} if emulator else {"Authorization": f"Bearer {gcp.access_token()}"}


def _gcs_object(key: str) -> str:
    base, _ = _gcs_base()
    return f"{base}/storage/v1/b/{quote(get_settings().docs_bucket, safe='')}/o/{quote(key, safe='')}"


def _gcs_ensure() -> None:
    base, emulator = _gcs_base()
    bucket = get_settings().docs_bucket
    resp = httpx.get(f"{base}/storage/v1/b/{quote(bucket, safe='')}/o", params={"maxResults": 1, "fields": "kind"},
                     headers=_gcs_headers(emulator), timeout=30.0)
    if resp.status_code == 200:
        return
    if emulator and resp.status_code == 404:
        httpx.post(f"{base}/storage/v1/b", params={"project": "local"}, json={"name": bucket}, timeout=30.0).raise_for_status()
        log.info("created emulator bucket %s", bucket)
        return
    resp.raise_for_status()


def _gcs_put(key: str, data: bytes, content_type: str) -> None:
    base, emulator = _gcs_base()
    bucket = get_settings().docs_bucket
    resp = httpx.post(f"{base}/upload/storage/v1/b/{quote(bucket, safe='')}/o", params={"uploadType": "media", "name": key},
                      content=data, headers={**_gcs_headers(emulator), "Content-Type": content_type}, timeout=120.0)
    resp.raise_for_status()


def _gcs_get(key: str) -> bytes:
    _, emulator = _gcs_base()
    resp = httpx.get(_gcs_object(key), params={"alt": "media"}, headers=_gcs_headers(emulator), timeout=120.0)
    if resp.status_code == 404:
        raise ObjectNotFound(key)
    resp.raise_for_status()
    return resp.content


def _gcs_delete(key: str) -> None:
    _, emulator = _gcs_base()
    resp = httpx.delete(_gcs_object(key), headers=_gcs_headers(emulator), timeout=30.0)
    if resp.status_code not in (200, 204, 404):
        resp.raise_for_status()


# ---------------------------------------------------------------------------
# API


def _ensure_bucket_sync() -> None:
    if _gcs():
        _gcs_ensure()
        return
    s = get_settings()
    c = _client()
    if not c.bucket_exists(s.docs_bucket):
        c.make_bucket(s.docs_bucket)
        log.info("created bucket %s", s.docs_bucket)


async def ensure_bucket() -> None:
    await asyncio.to_thread(_ensure_bucket_sync)


async def put_bytes(key: str, data: bytes, content_type: str = "application/pdf") -> None:
    s = get_settings()

    def _put() -> None:
        if _gcs():
            _gcs_put(key, data, content_type)
            return
        _client().put_object(s.docs_bucket, key, io.BytesIO(data), length=len(data), content_type=content_type)

    await asyncio.to_thread(_put)


async def get_bytes(key: str) -> bytes:
    """The object's content; ObjectNotFound when it does not exist."""
    s = get_settings()

    def _get() -> bytes:
        if _gcs():
            return _gcs_get(key)
        from minio.error import S3Error

        try:
            resp = _client().get_object(s.docs_bucket, key)
        except S3Error as err:
            if err.code in ("NoSuchKey", "NoSuchBucket"):
                raise ObjectNotFound(key, err.code) from err
            raise
        try:
            return resp.read()
        finally:
            resp.close()
            resp.release_conn()

    return await asyncio.to_thread(_get)


async def delete(key: str) -> None:
    s = get_settings()

    def _del() -> None:
        if _gcs():
            _gcs_delete(key)
            return
        from minio.error import S3Error

        try:
            _client().remove_object(s.docs_bucket, key)
        except S3Error as err:
            if err.code != "NoSuchKey":
                raise

    await asyncio.to_thread(_del)
