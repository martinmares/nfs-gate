.PHONY: build test test-integration docker clean

build:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -o bin/nfs-gate ./cmd/nfs-gate

test:
	go test ./...

test-integration:
	NFS_GATE_INTEGRATION=1 go test -tags=integration ./...

docker:
	docker build -t nfs-gate:latest .

clean:
	rm -f bin/nfs-gate
