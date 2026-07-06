FROM golang:1.22-bookworm AS build

WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/ledger-bot ./cmd/ledger-bot

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata font-noto-cjk ttf-dejavu
WORKDIR /app
COPY --from=build /out/ledger-bot /app/ledger-bot
ENV DATABASE_URL=postgres://ledger:ledger@postgres:5432/ledger_bot?sslmode=disable
ENTRYPOINT ["/app/ledger-bot"]
