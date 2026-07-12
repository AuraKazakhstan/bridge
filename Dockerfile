FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /bridge ./cmd/bridge

FROM gcr.io/distroless/static-debian12
COPY --from=build /bridge /bridge
EXPOSE 8080
ENTRYPOINT ["/bridge"]
