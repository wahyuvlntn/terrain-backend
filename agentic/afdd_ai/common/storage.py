"""MinIO access for RAG documents (private bucket `afdd-docs`, never `/public`)."""

from __future__ import annotations

import asyncio
import io
import logging
from functools import lru_cache

from minio import Minio
from minio.error import S3Error

from .config import get_settings

log = logging.getLogger(__name__)


@lru_cache(maxsize=1)
def _client() -> Minio:
    s = get_settings()
    return Minio(s.minio_endpoint, access_key=s.minio_access_key, secret_key=s.minio_secret_key, secure=s.minio_secure)


def _ensure_bucket_sync() -> None:
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
        _client().put_object(s.docs_bucket, key, io.BytesIO(data), length=len(data), content_type=content_type)

    await asyncio.to_thread(_put)


async def get_bytes(key: str) -> bytes:
    s = get_settings()

    def _get() -> bytes:
        resp = _client().get_object(s.docs_bucket, key)
        try:
            return resp.read()
        finally:
            resp.close()
            resp.release_conn()

    return await asyncio.to_thread(_get)


async def delete(key: str) -> None:
    s = get_settings()

    def _del() -> None:
        try:
            _client().remove_object(s.docs_bucket, key)
        except S3Error as err:
            if err.code != "NoSuchKey":
                raise

    await asyncio.to_thread(_del)
