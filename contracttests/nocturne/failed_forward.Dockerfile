ARG BASE_IMAGE=invalid.invalid/edu-agent/nocturne:required
FROM ${BASE_IMAGE}
ARG SOURCE_DATE_EPOCH
LABEL org.opencontainers.image.title="edu-agent Nocturne failed-forward A84 fixture" \
      org.opencontainers.image.created="${SOURCE_DATE_EPOCH}" \
      edu-agent.nocturne.fixture="failed-forward-v1"
COPY failed_forward.py /usr/local/bin/edu-agent-nocturne-failed-forward.py
USER 10001:10001
ENTRYPOINT ["python", "/usr/local/bin/edu-agent-nocturne-failed-forward.py"]
