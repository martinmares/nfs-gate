# nfs-gate

`nfs-gate` je malý NFSv3 server v uživatelském prostoru. Zpřístupní jeden existující POSIX adresář nebo S3 bucket/prefix přes stabilní NFS adresu. V Kubernetes nebo OpenShiftu tak mohou různé aplikace pracovat se stejnými soubory, i když se úložiště pod serverem časem změní.

Krátké vysvětlení s obrázkem a příklady je v [přehledu architektury](docs/overview.cs.md), který je dostupný také jako [PDF](docs/overview.cs.pdf). Anglická dokumentace je v [README.md](README.md).

## Jak to funguje

```text
Pody aplikací → klientské PVC → NFS PV → Service nfs-gate → Pod nfs-gate → backend
                                                                        ↳ emptyDir, připojené NFS/PVC nebo S3
```

Více aplikačních Podů může používat **stejné klientské PVC**. Jeho statický NFS PV ukazuje na Service před `nfs-gate`. NFS mount provádí uzel Kubernetes a aplikace pak vidí běžný adresář. `nfs-gate` převádí NFSv3 operace na operace nad adresářem zadaným pomocí `--root`. Ten může pocházet z `emptyDir` pro dočasný test, z **jiného, backendového PVC** nebo z přímo připojeného NFS. Klientské PVC a případné backendové PVC mají různé účely; `nfs-gate` žádné z nich nevytváří. S `--backend=s3` používá server místo `--root` bucket/prefix, ale klientské PV/PVC se připojuje stejně.

Server používá knihovnu [go-nfs](https://github.com/willscott/go-nfs). Výchozí backend `local` exportuje připojený adresář pomocí Go `os.Root`, aby souborové operace zůstaly uvnitř exportu. Backend `s3` pracuje přímo s objekty přes AWS SDK pro Go v2; nepotřebuje S3 mount, FUSE proces ani backendové PVC.

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

Pokud má každý cluster vlastní `nfs-gate` a aplikace ve všech clusterech mají vidět **stejné soubory**, musí být pod každým serverem připojen **tentýž externí NFS export se stejnou cestou**. Backendové PVC není nutné: externí NFS lze připojit do každého serverového Podu přímo přes inline `nfs` volume. Stávající `volumeMounts` na `/data` a `--root=/data` zůstanou:

```yaml
volumes:
  - name: data
    nfs:
      server: <externi-nfs-server>
      path: /spolecny-export
```

Hodnoty `server` a `path` musí být v obou clusterech stejné. Klientský NFS PV/PVC v každém clusteru stále ukazuje na *místní* Service `nfs-gate`. Oddělená `emptyDir` ani nezávisle vytvořená backendová PVC data mezi clustery nesdílejí. Inline NFS volume neumí určit mount options; pokud upstream vyžaduje vlastní port nebo verzi NFS, použijte backendový NFS PV/PVC s `mountOptions` nebo odpovídající konfiguraci uzlů. Viz [diagram a příklad pro dva clustery](docs/overview.cs.md#5-vice-fyzickych-clusteru).

Externí NFS server musí být dostupný z uzlů všech clusterů a jeho oprávnění musí dovolit Podům `nfs-gate` číst a zapisovat. Cache může viditelnost změn mezi clustery zpozdit; souběžné zápisy musí koordinovat aplikace, protože `nfs-gate` neposkytuje distribuované zámky. Společný upstream NFS sdílí data, ale sám o sobě nezajišťuje HA instancí `nfs-gate` ani nezachovává jejich file handly po restartu.

Kapacita klientského NFS PV je deklarace Kubernetes, nikoli vynucená kvóta. Ověřte mount na konkrétních uzlech a pravidla sítě. Výchozí Service nepublikuje UI. Chcete-li ho zpřístupnit, změňte přepínače serveru, přidejte port 8081 do Service a omezte přístup.

[Dokumentace Kubernetes k NFS volumes](https://kubernetes.io/docs/concepts/storage/volumes/#nfs) potvrzuje, že mount options patří do PV. `nolock` odstraňuje závislost na `rpc.statd`; distribuované zamykání server neposkytuje.

## Backendový adresář

Pod serveru může mít na `/upstream` připojené existující PVC, jiný NFS share, FUSE nebo lokální disk. Pak spusťte `nfs-gate --root /upstream`. Aplikace dál používají stejnou NFS adresu. V backendu zajistěte správná oprávnění UID/GID.

## Varianty úložiště

| Varianta | Nastavení | Životnost dat |
| --- | --- | --- |
| Dočasné úložiště | `--backend=local --root=/data` a Kubernetes `emptyDir` | Do odstranění nebo nahrazení serverového Podu |
| Připojený souborový systém | `--backend=local --root=/data` a NFS volume nebo backendové PVC | Podle skutečného úložiště |
| S3 | `--backend=s3 --s3-bucket=... --s3-prefix=...` | V bucketu, nezávisle na serverovém Podu |

`emptyDir` běžně používá disk uzlu. Zde znamená dočasná data bez persistence; samostatný RAM backend není potřeba.

### Nastavení S3

```bash
./bin/nfs-gate --backend=s3 \
  --s3-bucket=your-bucket --s3-prefix=shared --s3-region=us-east-1
```

Přihlašovací údaje používají standardní řetězec AWS SDK: proměnné prostředí (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, případně `AWS_SESSION_TOKEN`), sdílenou AWS konfiguraci nebo identitu workloadu/role. Hesla nepatří do manifestů ani argumentů procesu. V Kubernetes použijte samostatně vytvořený Secret nebo workload identity. Role potřebuje `s3:ListBucket` pro exportovaný prefix a `s3:GetObject`, `s3:PutObject`, `s3:DeleteObject` pro jeho objekty. Bucket s SSE-KMS může potřebovat i KMS oprávnění. Bucket musí předem existovat.

Pro S3 kompatibilní službu přidejte `--s3-endpoint=https://objects.example.org --s3-path-style` a region přijímaný službou. Volitelné rozšíření checksumů SDK je vypnuté, pokud je operace nevyžaduje; kompatibilitu konkrétního poskytovatele ověřte před nasazením.

Alternativní serverový manifest je [deploy/kubernetes-s3.yaml](deploy/kubernetes-s3.yaml). Změňte image, bucket, prefix a region; zajistěte Secret `nfs-gate-s3` nebo workload identity. Manifest má jednu repliku, `Recreate`, žádný datový svazek a read-only souborový systém kontejneru. Klientské PV/PVC zůstává kvůli současným NFS mount options. Docker image obsahuje CA certifikáty pro HTTPS.

**Nad jedním S3 bucketem/prefixem smí být právě jeden aktivní nfs-gate.** Další zapisující server nesmí běžet ani v jiném clusteru; za provozu také neměňte objekty přímo. Server nemá distribuovaný zámek ani fencing. Jedna replika a `Recreate` brání překryvu při běžném rollout, ale při failoveru musí provozovatel zajistit zastavení původního procesu. Více clusterů může používat jeden společný gateway, pokud jeho NFS adresu dosáhnou jejich uzly.

### Chování a omezení S3

- Soubor odpovídá objektu `<prefix>/<relativni-cesta>`. Prázdné adresáře reprezentují nulové objekty s `/` na konci; existující prefixy objektů se zobrazí jako adresáře. Použijte vyhrazený prefix bez kolizí názvů souborů a adresářů. Klíče s komponentami pro únik z exportu, zpětnými lomítky nebo nekanonickou cestou se nezpřístupňují.
- Čtení používá range GET. Každý NFS zápis, zkrácení souboru nebo změna atributů podle potřeby stáhne objekt a odešle celou novou verzi **ještě před potvrzením úspěchu**. Neexistuje cache neodeslaných změn ani požadavek na lokální persistenci. Chyba S3 se vrací jako NFS chyba. U přerušeného požadavku může být výsledek nejistý; před opakováním ne-idempotentní operace ověřte stav.
- Zápisy jsou v jedné instanci serializované i přes nezávisle otevřené handly. To chrání před ztracenou aktualizací uvnitř procesu; souběžné úpravy aplikací stále potřebují koordinaci. Cache NFS klienta může zpozdit viditelnost změn.
- Výchozí limit zapisovatelného souboru je **64 MiB**. Mění se pomocí `--s3-max-file-size` v bajtech, maximálně na 5 GB. Větší objekty lze číst, ale měnit ani přejmenovat je nelze. Úprava může dočasně spotřebovat několikanásobek velikosti souboru v RAM. I malý NFS zápis může přenášet celý objekt; backend je určený pro menší sdílené soubory, nikoli databázové soubory nebo velké soubory s častým náhodným zápisem.
- Přejmenování souboru uloží cíl a potom smaže zdroj. Je serializované pro klienty tohoto gateway, ale v S3 není transakcí; pád procesu nebo chyba mazání mohou zanechat oba názvy. Přejmenování adresářů, symlinky, hardlinky, distribuované zámky, rozšířená ACL a speciální soubory nejsou podporované.
- Oprávnění, UID/GID a čas změny jsou v metadatech objektu `nfs-*` a přežijí restart. Kořen exportu má pevná oprávnění a vlastníka; čas přístupu se samostatně neukládá. Atributy nejsou autentizací: NFS handler stále používá AUTH_NULL.
- `--s3-timeout` má výchozí hodnotu 30 sekund na API požadavek. Start serveru a `/readyz` ověřují možnost vypsat export, nikoli oprávnění k zápisu. NFS handly zůstávají vázané na proces; po restartu může být potřeba remount, i když objekty zůstanou zachované.

Viz [diagram S3](docs/s3.cs.svg), [S3 kapitola českého přehledu](docs/overview.cs.md#6-s3-backend) a [dokumentace endpointů AWS SDK](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-endpoints.html).

## Health endpointy

`GET /healthz` vrací 200, když proces běží. `GET /readyz` vrací 200, když je aktivní NFS listener a zvolený backend je dostupný (adresář nebo výpis S3 prefixu). Oba endpointy jsou na `--health-listen`, ve výchozím stavu `:8080`.

## Bezpečnost

Výchozí NFS handler používá AUTH_NULL. Klienty neautentizuje a jejich UID/GID nepřenáší jako lokální identitu pro přístup k backendu. Kdo dosáhne na NFS port, může s exportem pracovat v rozsahu oprávnění procesu serveru. Omezte přístup sítí, spusťte server bez root oprávnění a připojte mu jen zamýšlený backend.

Go `os.Root` blokuje `../` a symlinky vedoucí mimo export při souborových operacích. Nebrání vstupu do dalších filesystem mountů umístěných *uvnitř* exportu. U metadatových operací (`Chmod`, `Chown`, `Chtimes`) Go uvádí na Unixu omezení při souběžné výměně cíle za symlink; nespoléhejte na ně jako na přísný sandbox proti nedůvěryhodným lokálním procesům.

UI ukazuje názvy souborů a cesty operací. Ponechte ho na loopbacku nebo ho chraňte jinou přístupovou vrstvou. Health endpointy seznam souborů neposkytují.

## Omezení a restart

`go-nfs` uchovává file handly v omezené cache v paměti. Po restartu serveru nebo vyřazení z cache mohou existující NFS mounty vrátit `stale file handle`; v takovém případě klienta znovu připojte. Stabilní adresa Service handly nezachová. Tvrdý NFS mount může během výpadku blokovat souborové operace. Cache klientů může zpozdit viditelnost změn. Automatické zotavení stávajícího mountu po restartu není garantované.

Projekt neposkytuje vysokou dostupnost, replikaci, distribuované zámky ani stabilní handly po restartu. Hardlinky, speciální zařízení, rozšířená ACL a kvóty nejsou cílem. Go RPC test ověřuje dva klienty a základní operace; S3 testy používají skutečné AWS SDK proti lokálnímu HTTP testovacímu úložišti a dva NFS RPC klienty; ověřují i chyby uploadu, souběh, data po restartu a atributy pro cache adresářů. Neověřují živý AWS účet ani všechny S3 kompatibilní služby. Test kernelového mountu lokálního backendu má tag `integration` a vyžaduje Linux, root oprávnění a `mount.nfs`.

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

Software je dostupný pod [MIT licencí](LICENSE). Závislosti a vložené UI knihovny si zachovávají své vlastní licence; viz [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

## Komerční podpora

Software je poskytován pod MIT licencí bez záruky a bez bezplatné komunitní podpory.

Profesionální podporu poskytuje **[DataLite](https://datalite.cz)**. Může zahrnovat technickou pomoc, ověřené releasy, opravy, aktualizace, pomoc s nasazením, řešení problémů a dlouhodobou údržbu. Možnosti podpory sdělí [DataLite](https://datalite.cz).
