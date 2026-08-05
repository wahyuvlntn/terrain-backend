"""PDF text extraction and heading-aware chunking (DESIGN 6.2).

pypdf extracts the text of every page; running headers, footers and page
numbers are dropped; lines are grouped into chunks of about `TARGET_TOKENS`
tokens (including `OVERLAP_TOKENS` carried over from the previous chunk). A top-level heading
starts a new chunk (when the current one is not tiny), so chunks rarely mix
sections; a section longer than the target is split with overlap. Each chunk
records the page it starts on and the heading path it belongs to
("3 Alarm codes > 3.2 Diagnostics").

Headings are recognised as numbered section lines ("2.3 Alarm limits", as
written by the corpus generator) or short ALL-CAPS lines (common in vendor
manuals). Token counts are estimated as characters / 4, like the embedding
usage accounting.
"""

from __future__ import annotations

import io
import re
from dataclasses import dataclass

from pypdf import PdfReader

TARGET_TOKENS = 700
OVERLAP_TOKENS = 80
MIN_TOKENS = 60
MAX_HEADING_CHARS = 90

_NUMBERED = re.compile(r"^(\d{1,2}(?:\.\d{1,2}){0,3})\.?\s+([A-Z].{1,%d})$" % MAX_HEADING_CHARS)
_CAPS = re.compile(r"^[A-Z0-9][A-Z0-9 &/,()\-]{2,60}$")


@dataclass
class Chunk:
    index: int
    page: int
    heading: str | None
    content: str
    token_count: int


def tokens(text: str) -> int:
    return max(1, len(text) // 4)


def extract_pages(pdf: bytes) -> list[str]:
    """Text of every page (empty strings for pages without extractable text)."""
    reader = PdfReader(io.BytesIO(pdf))
    if reader.is_encrypted:
        try:
            reader.decrypt("")
        except Exception as err:  # noqa: BLE001 - reported as an unreadable document
            raise ValueError("the PDF is encrypted") from err
    return [(p.extract_text() or "") for p in reader.pages]


def heading_level(line: str) -> int | None:
    """1 for a top-level numbered or ALL-CAPS heading, 2+ for sub-sections, None for body text."""
    s = line.strip()
    if not s or len(s) > MAX_HEADING_CHARS or s.endswith((".", ":", ";", ",")):
        return None
    m = _NUMBERED.match(s)
    if m:
        # avoid list items / table rows: a heading has a few words and few digits
        words = m.group(2).split()
        if len(words) > 12 or sum(ch.isdigit() for ch in m.group(2)) > 6:
            return None
        if "." not in m.group(1) and s[len(m.group(1))] == ".":
            # "1. Text" is a numbered step unless it reads like a title ("1. Safety Instructions")
            if len(words) > 6 or any(w[0].islower() for w in words if len(w) >= 4):
                return None
        return m.group(1).count(".") + 1
    if _CAPS.match(s) and sum(ch.isalpha() for ch in s) >= 4 and len(s.split()) <= 8:
        return 1
    return None


def _tail(text: str, n_tokens: int) -> str:
    """The last ~n tokens of a text, cut at a word boundary."""
    keep = n_tokens * 4
    if len(text) <= keep:
        return text
    cut = text[-keep:]
    sp = cut.find(" ")
    return cut[sp + 1 :] if 0 <= sp < 40 else cut


def _split_long(line: str, limit_tokens: int) -> list[str]:
    """Split an over-long line at sentence or word boundaries."""
    limit = limit_tokens * 4
    parts: list[str] = []
    rest = line
    while len(rest) > limit:
        window = rest[:limit]
        cut = max(window.rfind(". "), window.rfind("; "))
        if cut < limit // 2:
            cut = window.rfind(" ")
        if cut <= 0:
            cut = limit
        parts.append(rest[: cut + 1].strip())
        rest = rest[cut + 1 :]
    if rest.strip():
        parts.append(rest.strip())
    return parts


_PAGE_NO = re.compile(r"(?i)\bpage\s+\d+\s*(?:/|of)\s*\d+\b")


def strip_furniture(pages: list[str]) -> list[list[str]]:
    """Page lines without running headers / footers and page numbers.

    A short line that repeats (digits ignored) on more than half of the pages
    of a document of 3 or more pages is page furniture (the generator's
    "SGF1 | title" header, a vendor's running title), as is any short line
    with a "Page n / N" number.
    """
    split = [[re.sub(r"\s+", " ", raw).strip() for raw in page.splitlines()] for page in pages]
    split = [[ln for ln in lines if ln] for lines in split]

    def key(line: str) -> str:
        return re.sub(r"\d+", "#", line.lower())

    counts: dict[str, int] = {}
    for lines in split:
        for k in {key(ln) for ln in lines if len(ln) <= 120}:
            counts[k] = counts.get(k, 0) + 1
    furniture = {k for k, c in counts.items() if len(pages) >= 3 and c > len(pages) / 2}
    return [[ln for ln in lines if key(ln) not in furniture and not (len(ln) <= 120 and _PAGE_NO.search(ln))] for lines in split]


def chunk_pages(pages: list[str], *, target: int = TARGET_TOKENS, overlap: int = OVERLAP_TOKENS, min_tokens: int = MIN_TOKENS) -> list[Chunk]:
    """Chunks of about `target` tokens (overlap included) that follow the section headings."""
    chunks: list[Chunk] = []
    path: list[tuple[int, str]] = []  # (level, heading line)
    buf: list[str] = []
    buf_page = 1
    buf_heading: str | None = None

    def heading_text() -> str | None:
        return " > ".join(h for _, h in path) if path else None

    def size() -> int:
        return tokens("\n".join(buf)) if buf else 0

    def flush(carry: bool) -> None:
        nonlocal buf, buf_heading
        text = "\n".join(buf).strip()
        if text:
            chunks.append(Chunk(len(chunks), buf_page, buf_heading, text, tokens(text)))
        buf = [_tail(text, overlap)] if carry and text else []
        buf_heading = heading_text()

    piece_limit = max(1, target - overlap)  # a piece plus the carried overlap fits in one chunk
    for pno, lines in enumerate(strip_furniture(pages), 1):
        for line in lines:
            level = heading_level(line)
            if level is not None:
                if level == 1 and size() >= min_tokens:
                    flush(carry=False)
                    buf_page = pno
                while path and path[-1][0] >= level:
                    path.pop()
                path.append((level, line))
                if not buf:
                    buf_page = pno
                if not buf or buf_heading is None:
                    buf_heading = heading_text()
                buf.append(line)
                continue
            for piece in _split_long(line, piece_limit) if tokens(line) > piece_limit else [line]:
                if buf and size() >= min_tokens and size() + tokens(piece) > target:
                    flush(carry=True)
                    buf_page = pno
                if not buf:
                    buf_page = pno
                    buf_heading = heading_text()
                buf.append(piece)
    if buf:
        flush(carry=False)
    # merge a tiny trailing chunk into its predecessor
    if len(chunks) >= 2 and chunks[-1].token_count < min_tokens // 2:
        last = chunks.pop()
        prev = chunks[-1]
        prev.content = prev.content + "\n" + last.content
        prev.token_count = tokens(prev.content)
    return chunks


def embed_input(chunk: Chunk) -> str:
    """Text embedded for a chunk: its heading path for context, then the content."""
    if chunk.heading and not chunk.content.startswith(chunk.heading.split(" > ")[-1]):
        return f"{chunk.heading}\n{chunk.content}"
    return chunk.content
