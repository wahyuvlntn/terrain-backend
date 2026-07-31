# AFDD AI services: one image, three services (docker-compose: ai-agent, ai-mcp, ai-analytics).
FROM python:3.12-slim

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    OMP_NUM_THREADS=2 \
    GOOGLE_GENAI_USE_VERTEXAI=FALSE

# libgomp1: LightGBM (OpenMP); fonts-dejavu-core: Unicode PDFs (fpdf2)
RUN apt-get update \
    && apt-get install -y --no-install-recommends libgomp1 fonts-dejavu-core \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY requirements.txt .
RUN pip install -r requirements.txt

COPY afdd_ai ./afdd_ai
RUN useradd --system --uid 10001 --home /app afddai && chown -R afddai /app
USER afddai

EXPOSE 8000 8101 8102
CMD ["python", "-m", "afdd_ai.analytics.worker"]
