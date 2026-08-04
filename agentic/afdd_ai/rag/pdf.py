"""A4 PDF writer for the generated RAG corpus (fpdf2 + a Unicode TrueType font).

Font: DejaVu Sans (`fonts-dejavu-core` in the image). The path is configurable
with AI_PDF_FONT / AI_PDF_FONT_BOLD; on a Windows host it falls back to Arial
from C:\\Windows\\Fonts, and as a last resort to the PDF core font Helvetica with
the text transliterated to Latin-1.

Section headings are numbered ("2.3 Alarm limits") so the ingestion chunker
recognises them in the extracted text.
"""

from __future__ import annotations

import logging
import os
import re
from collections.abc import Sequence
from datetime import datetime
from functools import lru_cache
from typing import Any

from fpdf import FPDF, XPos, YPos
from fpdf.fonts import FontFace

log = logging.getLogger(__name__)

FONT_CANDIDATES: tuple[tuple[str, str], ...] = (
    ("/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf", "/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf"),
    ("/usr/share/fonts/dejavu/DejaVuSans.ttf", "/usr/share/fonts/dejavu/DejaVuSans-Bold.ttf"),
    (r"C:\Windows\Fonts\DejaVuSans.ttf", r"C:\Windows\Fonts\DejaVuSans-Bold.ttf"),
    (r"C:\Windows\Fonts\arial.ttf", r"C:\Windows\Fonts\arialbd.ttf"),
)

_LATIN1 = {
    "σ": "sigma", "≥": ">=", "≤": "<=", "→": "->", "←": "<-", "–": "-", "—": "-", "‘": "'", "’": "'", "“": '"', "”": '"',
    "…": "...", "×": "x", "−": "-", "Δ": "delta ", "λ": "lambda", "√": "sqrt", "≈": "~", "•": "-", "€": "EUR",
}  # fmt: skip


@lru_cache(maxsize=1)
def font_paths() -> tuple[str | None, str | None]:
    """(regular, bold) TrueType font paths, or (None, None) for the core-font fallback."""
    reg, bold = os.environ.get("AI_PDF_FONT", "").strip(), os.environ.get("AI_PDF_FONT_BOLD", "").strip()
    if reg and os.path.isfile(reg):
        return reg, bold if bold and os.path.isfile(bold) else reg
    for r, b in FONT_CANDIDATES:
        if os.path.isfile(r):
            return r, b if os.path.isfile(b) else r
    log.warning("no Unicode TrueType font found (set AI_PDF_FONT); PDFs use Helvetica with Latin-1 text")
    return None, None


def _latin1(text: str) -> str:
    out = "".join(_LATIN1.get(ch, ch) for ch in text)
    return out.encode("latin-1", "replace").decode("latin-1")


_MD_BOLD = re.compile(r"\*\*(.+?)\*\*")
_MD_CODE = re.compile(r"`([^`]+)`")


class DocPDF(FPDF):
    """Report-style A4 document with numbered headings, tables and light Markdown."""

    def __init__(self, title: str, subtitle: str = "", *, site: str = "", generated_at: datetime | None = None):
        super().__init__(orientation="P", unit="mm", format="A4")
        self.doc_title = title
        self.doc_subtitle = subtitle
        self.site = site
        self.generated_at = generated_at
        reg, bold = font_paths()
        self.unicode = reg is not None
        if reg:
            self.add_font("Body", "", reg)
            self.add_font("Body", "B", bold or reg)
            self.family = "Body"
        else:
            self.family = "Helvetica"
        self.set_title(self.t(title))
        self.set_author("AFDD AI analytics")
        self.set_creator("afdd_ai.rag.corpus")
        if generated_at:
            self.set_creation_date(generated_at)
        self.set_margins(18, 16, 18)
        self.set_auto_page_break(True, margin=18)
        self.alias_nb_pages()
        self._h = [0, 0, 0]
        self.add_page()
        self._cover()

    # -- text helpers ----------------------------------------------------

    def t(self, text: Any) -> str:
        s = "" if text is None else str(text)
        return s if self.unicode else _latin1(s)

    def _font(self, size: float, bold: bool = False) -> None:
        self.set_font(self.family, "B" if bold else "", size)

    def header(self) -> None:
        if self.page_no() == 1:
            return
        self._font(7.5)
        self.set_text_color(110, 110, 110)
        self.cell(0, 5, self.t(f"{self.site}  |  {self.doc_title}"), new_x=XPos.LMARGIN, new_y=YPos.NEXT)
        self.set_draw_color(200, 200, 200)
        self.line(self.l_margin, self.get_y(), self.w - self.r_margin, self.get_y())
        self.ln(3)
        self.set_text_color(0, 0, 0)

    def footer(self) -> None:
        self.set_y(-12)
        self._font(7.5)
        self.set_text_color(110, 110, 110)
        gen = f"Generated {self.generated_at:%Y-%m-%d} from AFDD plant records" if self.generated_at else "AFDD plant records"
        self.cell(0, 5, self.t(f"{gen}  |  Page {self.page_no()}/{{nb}}"), align="R")
        self.set_text_color(0, 0, 0)

    def _cover(self) -> None:
        self._font(17, True)
        self.multi_cell(0, 8, self.t(self.doc_title), align="L", new_x=XPos.LMARGIN, new_y=YPos.NEXT)
        if self.doc_subtitle:
            self._font(10.5)
            self.set_text_color(70, 70, 70)
            self.multi_cell(0, 5.5, self.t(self.doc_subtitle), align="L", new_x=XPos.LMARGIN, new_y=YPos.NEXT)
            self.set_text_color(0, 0, 0)
        self.ln(4)

    # -- structure -------------------------------------------------------

    def h1(self, text: str) -> None:
        self._h = [self._h[0] + 1, 0, 0]
        if self.get_y() > self.h - 50:
            self.add_page()
        self.ln(2)
        self._font(13, True)
        self.multi_cell(0, 7, self.t(f"{self._h[0]} {text}"), align="L", new_x=XPos.LMARGIN, new_y=YPos.NEXT)
        self.ln(1)

    def h2(self, text: str) -> None:
        self._h = [self._h[0], self._h[1] + 1, 0]
        if self.get_y() > self.h - 40:
            self.add_page()
        self.ln(1)
        self._font(11, True)
        self.multi_cell(0, 6, self.t(f"{self._h[0]}.{self._h[1]} {text}"), align="L", new_x=XPos.LMARGIN, new_y=YPos.NEXT)

    def h3(self, text: str) -> None:
        self._font(9.5, True)
        self.multi_cell(0, 5, self.t(text), align="L", new_x=XPos.LMARGIN, new_y=YPos.NEXT)

    def para(self, text: str, size: float = 9.5) -> None:
        if not text:
            return
        self._font(size)
        self.multi_cell(0, 4.8, self.t(text), align="L", new_x=XPos.LMARGIN, new_y=YPos.NEXT)
        self.ln(1.2)

    def bullets(self, items: Sequence[str], numbered: bool = False) -> None:
        self._font(9.5)
        for i, item in enumerate(items, 1):
            mark = f"{i}." if numbered else "-"
            self.set_x(self.l_margin + 2)
            self.cell(6, 4.8, self.t(mark))
            self.multi_cell(0, 4.8, self.t(item), align="L", new_x=XPos.LMARGIN, new_y=YPos.NEXT)
        self.ln(1.2)

    def kv(self, rows: Sequence[tuple[str, Any]]) -> None:
        rows = [(k, v) for k, v in rows if v not in (None, "")]
        if rows:
            self.data_table(["Item", "Value"], [[k, v] for k, v in rows], widths=(0.32, 0.68), header=False)

    def data_table(self, headers: Sequence[str], rows: Sequence[Sequence[Any]], *, widths: Sequence[float] | None = None, header: bool = True, size: float = 8.0) -> None:
        if not rows:
            return
        self._font(size)
        total = self.w - self.l_margin - self.r_margin
        col_w = tuple(w * total for w in widths) if widths else None
        style = FontFace(emphasis="BOLD", fill_color=(232, 236, 241))
        with self.table(
            col_widths=col_w,
            text_align="LEFT",
            line_height=4.2,
            first_row_as_headings=header,
            headings_style=style,
            padding=1,
            borders_layout="HORIZONTAL_LINES",
        ) as tbl:
            if header:
                r = tbl.row()
                for h in headers:
                    r.cell(self.t(h))
            for row in rows:
                r = tbl.row()
                for c in row:
                    r.cell(self.t(_cell(c)))
        self.ln(2)

    def markdown(self, md: str) -> None:
        """Render the SOP Markdown subset (### headings, numbered / bullet lists, **bold**)."""
        para: list[str] = []
        items: list[str] = []
        numbered = False

        def flush() -> None:
            nonlocal para, items
            if para:
                self.para(_plain(" ".join(para)))
                para = []
            if items:
                self.bullets([_plain(i) for i in items], numbered)
                items = []

        for raw in (md or "").splitlines():
            line = raw.strip()
            if not line:
                flush()
                continue
            if line.startswith("#"):
                flush()
                self.h3(_plain(line.lstrip("#").strip()))
                continue
            m = re.match(r"^(\d+)[.)]\s+(.*)$", line)
            b = re.match(r"^[-*+]\s+(.*)$", line)
            if m or b:
                if para:
                    self.para(_plain(" ".join(para)))
                    para = []
                is_num = bool(m)
                if items and is_num != numbered:
                    self.bullets([_plain(i) for i in items], numbered)
                    items = []
                numbered = is_num
                items.append(m.group(2) if m else b.group(1))  # type: ignore[union-attr]
                continue
            if items:
                items[-1] += " " + line
            else:
                para.append(line)
        flush()

    def render(self) -> bytes:
        return bytes(self.output())


def _plain(md: str) -> str:
    return _MD_CODE.sub(r"\1", _MD_BOLD.sub(r"\1", md))


def _cell(v: Any) -> str:
    if v is None:
        return ""
    if isinstance(v, float):
        return f"{v:g}"
    if isinstance(v, datetime):
        return v.strftime("%Y-%m-%d %H:%M")
    return str(v)
