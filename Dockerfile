FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/webservice .

FROM alpine:3.21
RUN adduser -D -H appuser
WORKDIR /app
COPY --from=build /out/webservice /app/webservice
USER appuser
EXPOSE 8080
ENTRYPOINT ["/app/webservice"]
