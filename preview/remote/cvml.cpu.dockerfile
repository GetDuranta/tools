# Preview owns this dependency-only CPU runtime intentionally. Application source,
# generated proto code, and LFS models are mounted from the exact checkout at run
# time, while development dependency groups stay out of the image. Keep serving
# OS dependencies here in sync with cvml/inference.dockerfile.
FROM localhost/duranta-preview/base:golden

RUN apt-get update && apt-get install -y --no-install-recommends wget unzip fontconfig \
    && mkdir -p /usr/share/fonts/inter /usr/share/fonts/jetbrains-mono \
    && wget -q -O /tmp/Inter.zip https://github.com/rsms/inter/releases/download/v4.0/Inter-4.0.zip \
    && unzip -q /tmp/Inter.zip -d /tmp/inter \
    && find /tmp/inter -name '*.otf' -exec cp {} /usr/share/fonts/inter/ \; \
    && wget -q -O /tmp/JetBrainsMono.zip https://github.com/JetBrains/JetBrainsMono/releases/download/v2.304/JetBrainsMono-2.304.zip \
    && unzip -q /tmp/JetBrainsMono.zip -d /tmp/jbmono \
    && find /tmp/jbmono -name '*.ttf' -exec cp {} /usr/share/fonts/jetbrains-mono/ \; \
    && fc-cache -f \
    && rm -rf /tmp/Inter.zip /tmp/JetBrainsMono.zip /tmp/inter /tmp/jbmono \
    && apt-get clean && rm -rf /var/lib/apt/lists/*

WORKDIR /app/cvml
COPY pyproject.toml uv.lock ./
ENV UV_LINK_MODE=copy \
    UV_PYTHON=3.12.12 \
    UV_PROJECT_ENVIRONMENT=/app/cvml/.venv
RUN --mount=type=cache,target=/dockercache/uv \
    UV_CACHE_DIR=/dockercache/uv uv sync --frozen --no-dev --no-install-package duranta-proto

ENV MPLCONFIGDIR=/app/cvml/.matplotlib
RUN /app/cvml/.venv/bin/python -c 'import matplotlib.font_manager'
