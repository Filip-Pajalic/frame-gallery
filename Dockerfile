# Frame Gallery — one static Go binary in a scratch image.
# Pure-Go SQLite (modernc.org/sqlite) keeps CGO off, which is what allows scratch.

FROM golang:1.27-alpine AS build
WORKDIR /src
# Browse fetches reframed.gallery over HTTPS; scratch needs a CA bundle.
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# A broken test fails the build instead of shipping.
RUN go vet ./... && go test ./...
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/frame ./cmd/frame
RUN mkdir -p /out/data

FROM scratch
# Templates, CSS, JS and fonts are embedded in the binary.
COPY --from=build /out/frame /frame
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chown=99:100 /out/data /data
ENV FRAME_DATA=/data FRAME_HTTP=:18000
USER 99:100
EXPOSE 18000
# Exec form: scratch has no shell for a CMD-SHELL check.
HEALTHCHECK --interval=60s --timeout=5s --retries=3 --start-period=10s CMD ["/frame", "health"]
ENTRYPOINT ["/frame"]
CMD ["serve"]
