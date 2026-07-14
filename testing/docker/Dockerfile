# Test image: builds wifi-cursor from source and runs it against a virtual
# X server (Xvfb), so two containers on the same Docker bridge network can
# stand in for "two machines on the same Wi-Fi" for testing discovery/join
# without needing real hardware. Not used by end users or CI releases.
FROM golang:latest

RUN apt-get update && apt-get install -y --no-install-recommends \
    gcc pkg-config libx11-dev xorg-dev libxtst-dev libxkbcommon-dev libxkbcommon-x11-dev \
    libpng-dev libx11-xcb-dev libxcb1-dev libxcb-xkb-dev libxfixes-dev \
    xvfb iproute2 iputils-ping procps \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY . .
RUN go mod tidy && go build -o /usr/local/bin/wifi-cursor .

ENV DISPLAY=:99
COPY testing/docker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
