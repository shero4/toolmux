FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/toolmux ./cmd/toolmux

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/toolmux /toolmux
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/toolmux"]
