# What Chrome needs to run in a container. Chrome itself is mounted in from this machine, so a test uses the same build as
# everywhere else in the suite. Used only where the browser has to have network interfaces of its own.
FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    socat iproute2 ca-certificates fonts-liberation \
    libnss3 libnspr4 libatk1.0-0t64 libatk-bridge2.0-0t64 libatspi2.0-0t64 libcups2t64 libdbus-1-3 libdrm2 libexpat1 \
    libgbm1 libglib2.0-0t64 libgtk-3-0t64 libpango-1.0-0 libcairo2 libasound2t64 libudev1 libvulkan1 \
    libx11-6 libxcb1 libxcomposite1 libxdamage1 libxext6 libxfixes3 libxkbcommon0 libxrandr2 libxshmfence1 \
    && rm -rf /var/lib/apt/lists/*
