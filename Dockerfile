FROM node:24-alpine AS ui
WORKDIR /src
COPY package.json package-lock.json ./
RUN npm ci
COPY internal/web/static/app.css ./internal/web/static/app.css
COPY internal/web/static/styles.css ./internal/web/static/styles.css
COPY internal/web/templates ./internal/web/templates
RUN npm run build:css

FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=ui /src/internal/web/static/bundle.css ./internal/web/static/bundle.css
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sentinel ./cmd/sentinel

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/sentinel /sentinel
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/sentinel"]
