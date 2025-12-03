FROM golang:1.23.3-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY *.go ./

RUN CGO_ENABLED=0 GOOS=linux go build -o /main main.go

FROM gcr.io/distroless/base-debian11

WORKDIR /

COPY --from=builder /main /main

EXPOSE 8080

USER nonroot:nonroot

ENTRYPOINT ["/main"]