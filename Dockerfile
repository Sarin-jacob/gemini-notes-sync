FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gemini-notes-sync ./cmd/gemini-notes-sync

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/gemini-notes-sync /usr/local/bin/gemini-notes-sync
# config.yaml, key.json and .env are mounted here; state lives in /app/data.
VOLUME /app/data
EXPOSE 8080
HEALTHCHECK --interval=1m --timeout=10s --start-period=2m CMD ["gemini-notes-sync", "healthcheck"]
ENTRYPOINT ["gemini-notes-sync", "-config", "/app/config.yaml"]
CMD ["run"]
