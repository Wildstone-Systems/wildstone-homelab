# Build context: runtimes/ddgs-worker. Deployment/activation belongs to #493.
ARG PYTHON_BASE=python:3.12.14-slim-bookworm
FROM ${PYTHON_BASE}
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
WORKDIR /app
COPY requirements.txt ./
RUN pip install --no-cache-dir --require-hashes -r requirements.txt
COPY contract.py engine.py service.py ./
USER 65532:65532
# Loopback is the safe standalone default. The managed package must explicitly
# bind its isolated Keystone-only network; do not publish this port publicly.
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD ["python", "-c", "import json,urllib.request; r=json.load(urllib.request.urlopen('http://127.0.0.1:8080/health',timeout=2)); assert r == {'version':1,'ready':True}"]
ENTRYPOINT ["python", "-B", "service.py"]
