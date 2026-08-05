"""PDF text extraction and heading-aware chunking (rag/chunking.py, rag/pdf.py)."""

from __future__ import annotations

from afdd_ai.rag import chunking
from afdd_ai.rag.pdf import DocPDF


def words(n: int, seed: str = "valve") -> str:
    return " ".join(f"{seed}{i % 37} seat leakage check" for i in range(n // 4 + 1))


def test_heading_levels():
    assert chunking.heading_level("3 Alarm codes") == 1
    assert chunking.heading_level("3.2 Diagnostics") == 2
    assert chunking.heading_level("2.1.4 Seal replacement") == 3
    assert chunking.heading_level("MAINTENANCE PROCEDURES") == 1
    assert chunking.heading_level("1. Isolate the pump.") is None  # a numbered step, not a heading
    assert chunking.heading_level("1. Suction Superheat is abnormal AND Evaporator Approach is abnormal") is None
    assert chunking.heading_level("2. Safety Instructions") == 1  # a vendor's numbered title
    assert chunking.heading_level("12 34 56 78 90 12 34") is None
    assert chunking.heading_level("The chiller trips on low oil pressure when the heater fails") is None


def test_chunks_follow_sections_with_overlap():
    pages = [
        "1 Overview\n" + words(300) + "\n2 Alarm codes\n" + words(1500, "code"),
        "2.1 Diagnostics\n" + words(900, "diag") + "\n3 Procedures\n" + words(200, "proc"),
    ]
    chunks = chunking.chunk_pages(pages)
    assert [c.index for c in chunks] == list(range(len(chunks)))
    assert all(c.token_count <= chunking.TARGET_TOKENS + 40 for c in chunks)
    assert chunks[0].heading == "1 Overview" and chunks[0].page == 1
    # a new top-level section starts a new chunk
    starts = [c for c in chunks if c.content.startswith("2 Alarm codes")]
    assert starts and starts[0].heading == "2 Alarm codes"
    # long sections are split with overlap: consecutive chunks share text
    a, b = chunks[1], chunks[2]
    assert a.heading == b.heading == "2 Alarm codes"
    assert a.content[-60:].split()[-1] in b.content[:600]
    diag = [c for c in chunks if c.heading and c.heading.endswith("2.1 Diagnostics")]
    assert diag and diag[0].heading == "2 Alarm codes > 2.1 Diagnostics" and diag[0].page == 2
    assert chunks[-1].heading == "3 Procedures"


def test_embed_input_carries_the_heading_path():
    c = chunking.Chunk(0, 1, "2 Alarm codes > 2.1 Diagnostics", "Check the oil heater.", 5)
    assert chunking.embed_input(c) == "2 Alarm codes > 2.1 Diagnostics\nCheck the oil heater."
    c2 = chunking.Chunk(0, 1, "1 Overview", "1 Overview\nThe plant ...", 5)
    assert chunking.embed_input(c2) == c2.content


def test_generated_pdf_round_trip():
    pdf = DocPDF("Test manual - CUB plant", "Unit tests", site="SGF1")
    pdf.h1("Overview")
    pdf.para("The chilled water plant has three 1,000 RT centrifugal chillers (R-134a, 6.5 °C supply, ΔT 5.5 K). " * 4)
    pdf.h2("Design data")
    pdf.kv([("Capacity", "3,517 kW"), ("Approach", "≤ 3.5 K")])
    pdf.h1("Maintenance procedures")
    pdf.markdown("### Safety\n1. Lock out the **fan motor**.\n2. Verify zero energy.\n\nCheck `CT-01-GB` oil level.")
    pdf.data_table(["Code", "Name"], [["CHS-006", "Condenser 3-Way Valve Leakage"], ["CT-014", "Cooling Tower Heat Exchanger Fouling"]])
    blob = pdf.render()
    assert blob[:5] == b"%PDF-"
    pages = chunking.extract_pages(blob)
    text = "\n".join(pages)
    assert "1 Overview" in text and "2 Maintenance procedures" in text and "1.1 Design data" in text
    assert "CHS-006" in text and "Lock out the fan motor" in text
    chunks = chunking.chunk_pages(pages)
    assert chunks and chunks[0].heading.startswith("1 Overview")
    assert any(c.heading and c.heading.startswith("2 Maintenance procedures") for c in chunks)
