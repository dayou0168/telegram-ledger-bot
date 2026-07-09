FROM golang:1.22-bookworm AS build

WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
ARG APP=ledger-bot
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${APP}

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata font-noto-cjk ttf-dejavu
WORKDIR /app
COPY --from=build /out/app /app/app
ENV DATABASE_URL=postgres://ledger:ledger@postgres:5432/ledger_bot?sslmode=disable
ENTRYPOINT ["/app/app"]
