# nfs-gate

`nfs-gate` je malý NFSv3 server v uživatelském prostoru. Zpřístupní jeden existující POSIX adresář přes stabilní NFS adresu. V Kubernetes nebo OpenShiftu tak mohou různé aplikace pracovat se stejnými soubory, i když se úložiště pod serverem časem změní.

Krátké vysvětlení s obrázkem a příklady je v [přehledu architektury](docs/overview.cs.md), který je dostupný také jako [PDF](docs/overview.cs.pdf). Anglická dokumentace je v [README.md](README.md).

## Jak to funguje

```text
Pody aplikací → klientské PVC → NFS PV → Service nfs-gate → Pod nfs-gate → backendový adresář
                                                                        ↳ emptyDir nebo backendové PVC
```

Více aplikačních Podů může používat **stejné klientské PVC**. Jeho statický NFS PV ukazuje na Service před `nfs-gate`. NFS mount provádí uzel Kubernetes a aplikace pak vidí běžný adresář. `nfs-gate` převádí NFSv3 operace na operace nad adresářem zadaným pomocí `--root`. Ten může pocházet z `emptyDir` pro dočasný test nebo z **jiného, backendového PVC** či jiného POSIX mountu. Klientské PVC a backendové PVC mají různé účely; `nfs-gate` žádné z nich nevytváří.

Server sám nemountuje ani nezřizuje backendové úložiště. Exportuje právě jeden adresář pomocí knihovny [go-nfs](https://github.com/willscott/go-nfs). Adaptér souborového systému používá Go `os.Root`, aby souborové operace zůstaly uvnitř exportovaného adresáře.

## Lokální spuštění

```bash
make build
mkdir -p /tmp/nfs-gate-data /tmp/nfs-gate-client
./bin/nfs-gate --root /tmp/nfs-gate-data
```

Na linuxovém klientovi s `nfs-utils` nebo `nfs-common`:

```bash
sudo mount -t nfs \
  -o nfsvers=3,proto=tcp,port=12049,mountvers=3,mountport=12049,mountproto=tcp \
  127.0.0.1:/ /tmp/nfs-gate-client
echo hello | sudo tee /tmp/nfs-gate-client/hello.txt
cat /tmp/nfs-gate-data/hello.txt
sudo umount /tmp/nfs-gate-client
```

NFS listener ve výchozím stavu naslouchá jen na loopbacku. Pro vzdálené klienty nastavte `--listen=:12049` a přístup k portu omezte sítí. Server nepotřebuje `rpcbind` ani privilegovaný port. `--help` vypíše přepínače a `--version` verzi. V minimálním klientovi bez `rpc.statd` přidejte do mount options `nolock`; server neposkytuje distribuované zamykání.

## Webové UI

Read-only UI je ve výchozím stavu na `http://127.0.0.1:8081/ui/`. Ukazuje stav NFS listeneru, počty operací, poslední pozorované operace a stránkovaný seznam adresářů. V záhlaví lze přepínat světlý, tmavý a automatický režim. Tabler a HTMX jsou vložené do binárky. UI neposkytuje obsah souborů, stahování ani změny souborů.

Historie uchovává v paměti nejvýše 500 operací, které prošly přes NFS backend, a po restartu se smaže. Změny provedené přímo v backendu se projeví v seznamu souborů, ale nejsou v Activity. Cache NFS klientů může viditelnost změn zpozdit. UI není úplný auditní záznam.

Bez autentizace se UI váže jen na loopback. Pro vzdálený přístup je nutné výslovně nastavit `--ui-listen=:8081 --allow-unauthenticated-ui` a omezit síťový přístup. Názvy souborů a cesty mohou být citlivé. Health endpointy běží zvlášť na portu 8080.

## Docker

```bash
make docker
docker run --rm -p 12049:12049 -p 8080:8080 -v "$PWD/data:/data" nfs-gate:latest --listen=:12049
```

Image používá `scratch`, staticky linkovanou binárku a UID/GID 65532. Připojený adresář musí být pro tuto identitu zapisovatelný. Chcete-li zpřístupnit UI, publikujte také port 8081 a použijte `--ui-listen=:8081 --allow-unauthenticated-ui` jen v důvěryhodné síti.

## Kubernetes

[deploy/kubernetes.yaml](deploy/kubernetes.yaml) obsahuje Deployment s jednou replikou, `emptyDir` a Service pro NFS a health. Po nastavení image dostupného z clusteru ho lze aplikovat příkazem `kubectl apply -f deploy/kubernetes.yaml`. Pod běží jako UID/GID 65532, má `fsGroup` a bez dalších capabilities. `emptyDir` přežije restart kontejneru ve stejném Podu, ale při odstranění nebo nahrazení Podu data zaniknou.

Service DNS mohou použít klienti, kteří je umějí vyřešit:

```bash
sudo mount -t nfs \
  -o nfsvers=3,proto=tcp,port=12049,mountvers=3,mountport=12049,mountproto=tcp \
  nfs-gate.<namespace>.svc.cluster.local:/ /mnt/shared
```

Inline `nfs:` volume v Podu neumí určit mount options. Pro nestandardní port 12049 proto vytvořte statický NFS PV s `mountOptions` a navázané **klientské PVC**. Na uzlech musí být NFS mount helper a uzel musí dosáhnout na adresu serveru. ClusterIP lze použít tam, kde ji uzly umějí směrovat; DNS `*.svc.cluster.local` nemusí být na samotných uzlech dostupné. Adresu nejprve ověřte z uzlů.

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
    server: <adresa-dostupna-z-uzlu>
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

Každý aplikační Pod připojí **stejné klientské PVC**:

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

Pro data, která mají přežít nahrazení serverového Podu, vytvořte **jiné, backendové PVC** vhodnou StorageClass a nahraďte jím `emptyDir` v Deploymentu `nfs-gate`. Server ponechte s jednou replikou a strategií `Recreate`. Režim přístupu a StorageClass musí dovolit serverovému Podu připojit úložiště na uzlu, kam bude naplánován.

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

V serverovém Deploymentu nahraďte pouze položku `volumes`:

```yaml
volumes:
  - name: data
    persistentVolumeClaim:
      claimName: nfs-gate-backend
```

### Více fyzických clusterů

Pokud má každý cluster vlastní `nfs-gate` a aplikace ve všech clusterech mají vidět **stejné soubory**, musí být pod každým serverem připojen **tentýž externí NFS export se stejnou cestou**. V každém clusteru vytvořte backendový NFS PV/PVC mířící na společný export a připojte jej do tamního Podu `nfs-gate` na `/data`. Klientský NFS PV/PVC v každém clusteru stále ukazuje na *místní* Service `nfs-gate`. Oddělená `emptyDir` ani nezávisle vytvořená backendová PVC data mezi clustery nesdílejí. Dynamický NFS provisioner může v každém clusteru vytvořit jiný podadresář, proto ověřte skutečnou cestu upstream exportu. Viz [diagram a příklad pro dva clustery](docs/overview.cs.md#5-vice-fyzickych-clusteru).

Externí NFS server musí být dostupný z uzlů všech clusterů a jeho oprávnění musí dovolit Podům `nfs-gate` číst a zapisovat. Cache může viditelnost změn mezi clustery zpozdit; souběžné zápisy musí koordinovat aplikace, protože `nfs-gate` neposkytuje distribuované zámky. Společný upstream NFS sdílí data, ale sám o sobě nezajišťuje HA instancí `nfs-gate` ani nezachovává jejich file handly po restartu.

Kapacita klientského NFS PV je deklarace Kubernetes, nikoli vynucená kvóta. Ověřte mount na konkrétních uzlech a pravidla sítě. Výchozí Service nepublikuje UI. Chcete-li ho zpřístupnit, změňte přepínače serveru, přidejte port 8081 do Service a omezte přístup.

[Dokumentace Kubernetes k NFS volumes](https://kubernetes.io/docs/concepts/storage/volumes/#nfs) potvrzuje, že mount options patří do PV. `nolock` odstraňuje závislost na `rpc.statd`; distribuované zamykání server neposkytuje.

## Backendový adresář

Pod serveru může mít na `/upstream` připojené existující PVC, jiný NFS share, FUSE nebo lokální disk. Pak spusťte `nfs-gate --root /upstream`. Aplikace dál používají stejnou NFS adresu. V backendu zajistěte správná oprávnění UID/GID.

## Health endpointy

`GET /healthz` vrací 200, když proces běží. `GET /readyz` vrací 200, když je aktivní NFS listener a exportovaný adresář lze otevřít. Oba endpointy jsou na `--health-listen`, ve výchozím stavu `:8080`.

## Bezpečnost

Výchozí NFS handler používá AUTH_NULL. Klienty neautentizuje a jejich UID/GID nepřenáší jako lokální identitu pro přístup k backendu. Kdo dosáhne na NFS port, může s exportem pracovat v rozsahu oprávnění procesu serveru. Omezte přístup sítí, spusťte server bez root oprávnění a připojte mu jen zamýšlený backend.

Go `os.Root` blokuje `../` a symlinky vedoucí mimo export při souborových operacích. Nebrání vstupu do dalších filesystem mountů umístěných *uvnitř* exportu. U metadatových operací (`Chmod`, `Chown`, `Chtimes`) Go uvádí na Unixu omezení při souběžné výměně cíle za symlink; nespoléhejte na ně jako na přísný sandbox proti nedůvěryhodným lokálním procesům.

UI ukazuje názvy souborů a cesty operací. Ponechte ho na loopbacku nebo ho chraňte jinou přístupovou vrstvou. Health endpointy seznam souborů neposkytují.

## Omezení a restart

`go-nfs` uchovává file handly v omezené cache v paměti. Po restartu serveru nebo vyřazení z cache mohou existující NFS mounty vrátit `stale file handle`; v takovém případě klienta znovu připojte. Stabilní adresa Service handly nezachová. Tvrdý NFS mount může během výpadku blokovat souborové operace. Cache klientů může zpozdit viditelnost změn. Automatické zotavení stávajícího mountu po restartu není garantované.

Projekt neposkytuje vysokou dostupnost, perzistenci, replikaci, distribuované zámky ani stabilní handly po restartu. Hardlinky, speciální zařízení, rozšířená ACL a kvóty nejsou cílem. Go RPC test ověřuje dva klienty a základní operace; test kernelového mountu má tag `integration` a vyžaduje Linux, root oprávnění a `mount.nfs`.

## Vývoj

```bash
make build
make test
make test-integration
make docker
```

UI obsahuje vložené assety Tabler Admin, Tabler Icons a HTMX. Viz [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

## Co projekt nedělá

`nfs-gate` není distribuovaný filesystem, perzistentní úložiště, HA storage systém, CSI driver, provisioner, správce mountů ani plnohodnotné NFS zařízení.

## Licence

Software je dostupný pod [MIT licencí](LICENSE). Vložené UI knihovny si zachovávají své vlastní licence.

## Komerční podpora

Software je poskytován pod MIT licencí bez záruky a bez bezplatné komunitní podpory.

Profesionální podporu poskytuje **[DataLite](https://datalite.cz)**. Může zahrnovat technickou pomoc, ověřené releasy, opravy, aktualizace, pomoc s nasazením, řešení problémů a dlouhodobou údržbu. Možnosti podpory sdělí [DataLite](https://datalite.cz).
