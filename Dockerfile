# builder
FROM golang:1.24-alpine AS builder
RUN apk add --no-cache git ca-certificates

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o quotient

# runner
FROM alpine:3.21
RUN apk add --no-cache fortune ca-certificates \
    wireguard-tools iproute2 iputils curl git openssh \
    py3-uv python3 proxychains-ng jq file sshpass bind-tools grep

# Install custom python check dependencies
COPY custom-checks/requirements.txt /tmp/requirements.txt
RUN ln -sf python3 /usr/bin/python && \
    uv pip install --system --break-system-packages setuptools && \
    uv pip install --system --break-system-packages -r /tmp/requirements.txt && \
    rm /tmp/requirements.txt

COPY config/certs/. /usr/local/share/ca-certificates/
RUN update-ca-certificates
COPY --from=builder /src/quotient /usr/local/bin/quotient
WORKDIR /app

CMD ["quotient"]
