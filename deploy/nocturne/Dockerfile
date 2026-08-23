FROM docker.io/library/python:3.12.11-slim-bookworm@sha256:c00fc7b44d844b6da22861ec24af43968a5200eac4ec607b4725d585165d6b49

ARG SOURCE_DATE_EPOCH=1786297652
ARG SUPPLY_CHAIN_LOCK_SHA256

LABEL org.opencontainers.image.source="https://github.com/Dataojitori/nocturne_memory" \
      org.opencontainers.image.version="2.5.6" \
      org.opencontainers.image.revision="54c48eeaeea3cca61ff6bc065cbe1a4c32a3b254" \
      org.edu-agent.nocturne.compat-revision="edu-agent-maintenance-v1" \
      org.edu-agent.nocturne.supply-chain-lock-sha256="${SUPPLY_CHAIN_LOCK_SHA256}" \
      org.edu-agent.nocturne.source-date-epoch="${SOURCE_DATE_EPOCH}"

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PIP_NO_CACHE_DIR=1 \
    SNAPSHOT_DIR=/app/snapshots \
    EDU_AGENT_BACKUP_ROOT=/app/backups

RUN rm -f /etc/apt/sources.list /etc/apt/sources.list.d/* \
    && printf '%s\n' \
       'deb [check-valid-until=no] https://snapshot.debian.org/archive/debian/20260801T000000Z bookworm main' \
       > /etc/apt/sources.list \
    && apt-get -o Acquire::Check-Valid-Until=false update \
    && apt-get install -y --no-install-recommends \
       libpq5=15.18-0+deb12u1 \
       netbase=6.4 \
       postgresql-client-15=15.18-0+deb12u1 \
       postgresql-client-common=248+deb12u1 \
       sensible-utils=0.0.17+nmu1 \
    && rm -rf /var/lib/apt/lists/* \
              /var/log/apt/* /var/log/dpkg.log /var/log/alternatives.log \
              /var/cache/ldconfig/aux-cache

WORKDIR /app
COPY requirements.lock build-requirements.lock /tmp/
RUN python -m pip install --require-hashes -r /tmp/build-requirements.lock \
    && python -m pip install --require-hashes --no-build-isolation -r /tmp/requirements.lock \
    && rm /tmp/build-requirements.lock /tmp/requirements.lock

COPY app/ /app/
RUN adduser --disabled-password --gecos '' --uid 10001 appuser \
    && install -d -o appuser -g appuser /app/snapshots /app/backups \
    && chown -R appuser:appuser /app

USER appuser
CMD ["python", "main.py"]
