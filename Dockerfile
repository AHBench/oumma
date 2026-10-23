FROM golang:1.26-alpine AS builder

RUN apk add --no-cache ca-certificates git

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -trimpath -o /bin/oumma ./cmd/main.go

# --- Runtime ---
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S oumma && adduser -S oumma -G oumma

COPY --from=builder /bin/oumma /usr/local/bin/oumma

# The /processes volume is where workflow YAML definitions are mounted.
# Each file must be named <process_id>.yaml and conform to the schema.
RUN mkdir -p /processes && chown oumma:oumma /processes
VOLUME ["/processes"]

USER oumma
EXPOSE 8080

ENV ADDR=":8080" \
    PROCESSES_DIR="/processes" \
    DATABASE_URL="" \
    OPENAI_API_KEY="" \
    OPENAI_BASE_URL="" \
    OPENAI_MODEL="gpt-4o"

HEALTHCHECK --interval=15s --timeout=3s --retries=3 \
    CMD wget -qO- http://localhost:8080/health || exit 1

ENTRYPOINT ["oumma"]
