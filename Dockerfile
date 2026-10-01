FROM golang:1.27-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /nfs-gate ./cmd/nfs-gate
RUN mkdir -p /data && chown 65532:65532 /data

FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /nfs-gate /nfs-gate
COPY --from=builder --chown=65532:65532 /data /data
COPY LICENSE THIRD_PARTY_NOTICES.md /licenses/
COPY third_party/licenses /licenses/third_party/
USER 65532:65532
EXPOSE 12049/tcp 8080/tcp
ENTRYPOINT ["/nfs-gate"]
