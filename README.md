# nfs-gate

nfs-gate is a small userspace NFSv3 gateway that exposes an existing POSIX directory through a stable NFS endpoint. It is intended for Kubernetes and OpenShift applications that share one filesystem namespace while the backing storage can change.

```text
NFS clients → nfs-gate:12049 → POSIX directory
                                  emptyDir, PVC, mounted NFS, FUSE, local disk
```

The server does not mount or provision the backend. It exports exactly one directory using [go-nfs](https://github.com/willscott/go-nfs). The filesystem adapter uses Go `os.Root` to contain path traversal and symlinks within the export.

For a short explanation with a diagram and Kubernetes examples, see the [architecture overview (Czech)](docs/overview.cs.md) or its [PDF version](docs/overview.cs.pdf). A full Czech version of this README is in [README.cs.md](README.cs.md).

## How it works

```text
Application Pods → client PVC → NFS PV → nfs-gate Service → nfs-gate Pod → backend directory
                                                                  ↳ emptyDir or backend PVC
```

Several application Pods can mount the **same client PVC**. Its static NFS PV points to the nfs-gate Service. The node mounts that export and presents it as a directory in each application Pod. nfs-gate translates NFSv3 file operations to ordinary operations under its configured `--root` directory. That directory comes from a separate backend volume: `emptyDir` for a disposable test, or a backend PVC or other POSIX mount for data that must outlive the server Pod. The client PVC and backend PVC have different roles; nfs-gate does not create either one.

## Usage

```bash
make build
mkdir -p /tmp/nfs-gate-data /tmp/nfs-gate-client
./bin/nfs-gate --root /tmp/nfs-gate-data
```

On a Linux client with `nfs-utils` or `nfs-common` installed:

```bash
sudo mount -t nfs \
  -o nfsvers=3,proto=tcp,port=12049,mountvers=3,mountport=12049,mountproto=tcp \
  127.0.0.1:/ /tmp/nfs-gate-client
echo hello | sudo tee /tmp/nfs-gate-client/hello.txt
cat /tmp/nfs-gate-data/hello.txt
sudo umount /tmp/nfs-gate-client
```

The NFS listener binds to loopback by default. For remote clients, set `--listen=:12049` and restrict access to trusted clients at the network layer. The port is explicit; `rpcbind` and privileged server ports are unnecessary. `--help` lists all flags and `--version` prints the binary version.

In a minimal Linux client without `rpc.statd`, add `nolock` to the mount options. The server does not provide distributed locking.

## Web UI

The read-only UI is at `http://127.0.0.1:8081/ui/` by default. It shows listener status, operation counts, recent NFS operations, and a paged directory browser. The header offers Light, Dark and Auto themes; Auto follows the system color scheme, and the choice is saved in the browser. Tabler and HTMX are embedded in the binary. The UI has no endpoint for file contents, downloads, uploads, or changes.

Activity is an in-memory history of up to 500 filesystem calls observed through the NFS backend. It is cleared on restart. Direct changes to the backend appear in Files, but not in Activity. NFS clients and their caches can delay when changes appear; this is a diagnostic view, not an audit log.

Without authentication, the UI binds to loopback. To expose it to another host, explicitly use `--ui-listen=:8081 --allow-unauthenticated-ui` and restrict access at the network layer. File names and paths can be sensitive. Health endpoints remain separate on `:8080`.

## Docker

```bash
make docker
docker run --rm -p 12049:12049 -p 8080:8080 -v "$PWD/data:/data" nfs-gate:latest --listen=:12049
```

The image uses `scratch`, a statically linked binary, and UID/GID 65532. Ensure the mounted directory is writable by that identity. For a local UI demonstration, publish port 8081 and pass `--ui-listen=:8081 --allow-unauthenticated-ui` with suitable network restrictions.

## Kubernetes

`deploy/kubernetes.yaml` includes one Deployment with `emptyDir` and a Service for NFS and health. Apply it with `kubectl apply -f deploy/kubernetes.yaml` after choosing an image available to your cluster. The Pod uses UID/GID 65532, `fsGroup`, and drops all capabilities. `emptyDir` survives a container restart in the same Pod, but its data is lost when the Pod is removed or replaced. Use a backend PVC for persistence across Pod replacement.

Use the Service DNS name as the NFS address:

```bash
sudo mount -t nfs \
  -o nfsvers=3,proto=tcp,port=12049,mountvers=3,mountport=12049,mountproto=tcp \
  nfs-gate.<namespace>.svc.cluster.local:/ /mnt/shared
```

An inline Pod `nfs:` volume has `server`, `path`, and `readOnly`, but no field for mount options. With the nonstandard port, use a static NFS PersistentVolume with `mountOptions` and a PVC. The node needs an NFS mount helper and must be able to reach the server address. A ClusterIP works in clusters where nodes can route to Services; node DNS may not resolve `*.svc.cluster.local`, so use an address verified from the nodes. Example:

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: nfs-gate-shared
spec:
  capacity:
    storage: 1Gi
  accessModes: [ReadWriteMany]
  persistentVolumeReclaimPolicy: Retain
  storageClassName: ""
  mountOptions:
    - nfsvers=3
    - proto=tcp
    - port=12049
    - mountvers=3
    - mountport=12049
    - mountproto=tcp
    - nolock
  nfs:
    server: <server-address-reachable-from-nodes>
    path: /
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: nfs-gate-shared
spec:
  accessModes: [ReadWriteMany]
  storageClassName: ""
  resources:
    requests:
      storage: 1Gi
  volumeName: nfs-gate-shared
```

Mount that **client PVC** into each application Pod:

```yaml
spec:
  containers:
    - name: app
      image: your-app:tag
      volumeMounts:
        - name: shared
          mountPath: /shared
  volumes:
    - name: shared
      persistentVolumeClaim:
        claimName: nfs-gate-shared
```

For a persistent server backend, create a **different PVC** from a suitable StorageClass, then replace the `emptyDir` volume in the nfs-gate Deployment with a `persistentVolumeClaim`. Keep the server at one replica and `strategy: Recreate`; choose an access mode and storage class that permit the server Pod to mount the volume on its scheduled node.

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: nfs-gate-backend
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: your-storage-class
  resources:
    requests:
      storage: 10Gi
```

In the server Deployment, replace only its `volumes` entry:

```yaml
volumes:
  - name: data
    persistentVolumeClaim:
      claimName: nfs-gate-backend
```

### Several physical clusters

If each cluster runs its own nfs-gate and applications in all clusters must see the **same files**, each gateway needs a backend mount of the **same external NFS export and path**. Create a backend NFS PV/PVC in each cluster that points to that shared export; mount it at `/data` in that cluster's nfs-gate Pod. The client NFS PV/PVC in each cluster still points to its *local* nfs-gate Service. Separate `emptyDir` volumes or independently provisioned backend PVCs do not share data across clusters. A dynamically provisioned NFS PVC may create a different subdirectory in each cluster, so verify the actual upstream export path. See the [two-cluster diagram and example](docs/overview.cs.md#5-vice-fyzickych-clusteru).

The upstream NFS server must be reachable from every cluster's nodes, and its permissions must allow the nfs-gate Pod to read and write. Cross-cluster changes can be delayed by caches; concurrent writes need application-level coordination because nfs-gate does not provide distributed locking. An upstream NFS backend shares data but does not make nfs-gate highly available or preserve its file handles across restarts.

The client PV's capacity is a Kubernetes declaration, not an enforced quota. Validate the PV mount against your cluster's node image and network policy. UI is not exposed in the default Service. To enable it, make a deliberate change to the Deployment flags, add port 8081 to the Service, and restrict which clients can reach it.

The inline Pod `nfs:` limitation is confirmed by the Kubernetes API fields and the [Kubernetes volume documentation](https://kubernetes.io/docs/concepts/storage/volumes/#nfs). A Linux kernel client has been tested with the same port and mount options. `nolock` avoids requiring `rpc.statd`; the server does not provide distributed locking.

## Gateway mode

Mount an existing POSIX backend into the server Pod, for example at `/upstream`, then run `nfs-gate --root /upstream`. The backend can be a PVC, an externally mounted NFS share, FUSE, or a local directory. nfs-gate does not implement an NFS client.

```yaml
volumeMounts:
  - name: upstream
    mountPath: /upstream
```

Changing from `emptyDir` to an upstream mount does not require changing application NFS endpoints. Preserve appropriate UID/GID and permissions on the backend.

## Health

`GET /healthz` returns 200 while the process is running. `GET /readyz` returns 200 when the NFS listener is active and the export directory still exists and can be opened. Both are on `--health-listen` (default `:8080`).

## Security considerations

NFSv3 AUTH_NULL is used by the default handler. The server does not authenticate NFS clients or enforce their UID/GID as local filesystem identities; access is governed by the server process and the backend directory permissions. Restrict access to the NFS port with cluster networking. Run the server as a dedicated non-root identity with only the intended export mounted.

`os.Root` blocks `../` and symlinks that leave the export in file operations. It does not prevent traversal into filesystem mounts placed *inside* the export. Go documents a Unix race limitation for `os.Root` metadata changes (`Chmod`, `Chown`, `Chtimes`) if a target is replaced by a symlink concurrently; do not allow untrusted local processes to mutate the export while relying on this as a strict sandbox.

The UI exposes file names and activity paths, even though it cannot read file contents. Keep it on loopback or behind an access control layer. Health endpoints expose no file listing.

## Limitations and restart behaviour

go-nfs uses a bounded, in-memory file handle cache. Existing NFS mounts can receive `stale file handle` after the server restarts or cache entries are evicted; remount clients when this occurs. A stable Service address does not preserve file handles. During an outage, hard NFS mounts can block filesystem operations until the server returns. Client attribute caching can delay cross-client visibility. The restart and automatic recovery sequence has not been kernel-tested here; do not assume an existing mount recovers without remounting. This project does not provide HA, persistence, replication, distributed locks, or stable file handles across process restarts. Hard links, special devices, extended ACLs and quotas are not a target.

The direct Go RPC test covers two clients and create, write, read, mkdir, rename, truncate and remove. A Linux kernel mount test is separately tagged `integration` and requires root, `mount.nfs` and mount privileges.

## Development

```bash
make build
make test
make test-integration
make docker
```

The UI embeds Tabler Admin, Tabler Icons and HTMX assets. See [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

## Non-goals

nfs-gate is not a distributed filesystem, persistent storage product, HA storage system, CSI driver, storage provisioner, mount manager, or NFS appliance.

## License

The software is available under the [MIT License](LICENSE). Third-party UI assets retain their own licenses.

## Commercial Support

This software is provided under the MIT License and comes without warranty or free community support.

Commercial support is available from **[DataLite](https://datalite.cz)**, including technical assistance, verified releases, bug fixes, updates, deployment assistance, troubleshooting, and long-term maintenance. Contact [DataLite](https://datalite.cz) for commercial support options.
