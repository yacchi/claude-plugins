FROM ubuntu:24.04@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        python3 python3-yaml python3-pytest git bubblewrap ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*

COPY --from=ghcr.io/astral-sh/uv:latest /uv /usr/local/bin/uv
COPY fake_pi.py /usr/local/bin/pi
RUN chmod 0755 /usr/local/bin/pi \
    && useradd --create-home --shell /bin/bash worker

USER worker
WORKDIR /home/worker
ENV PATH=/home/worker/.local/bin:$PATH \
    UV_PYTHON_PREFERENCE=only-system
# Warm uv's cache for agent-exec's PEP 723 dependency (pyyaml).
RUN printf '# /// script\n# dependencies = ["pyyaml"]\n# ///\nimport yaml\n' > /tmp/warm.py \
    && uv run --quiet /tmp/warm.py && rm /tmp/warm.py \
    && git config --global user.email worker@example.invalid \
    && git config --global user.name worker \
    && git config --global init.defaultBranch main
