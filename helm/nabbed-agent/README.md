# nabbed-agent Helm chart

Runs the Nabbed agent on Kubernetes as a StatefulSet. Every replica is a
separate agent with its own identity, pairing and data volume.

## Install

From the OCI registry:

```sh
helm install nabbed oci://ghcr.io/getnabbed/charts/nabbed-agent --version 0.1.2 \
  --namespace nabbed --create-namespace \
  --set config.api_url=https://api.nabbed.example
```

Or from the Helm repository:

```sh
helm repo add nabbed https://getnabbed.github.io/agent
helm install nabbed nabbed/nabbed-agent --version 0.1.2 \
  --namespace nabbed --create-namespace \
  --set config.api_url=https://api.nabbed.example
```

From a checkout, use `./helm/nabbed-agent` as the chart. Then follow the
printed notes to pair each agent.

### Verifying the chart

The chart is signed with cosign using the release workflow's GitHub
identity, as the images are. Verify against the exact workflow and tag:

```sh
VERSION=0.1.2
cosign verify ghcr.io/getnabbed/charts/nabbed-agent:$VERSION \
  --certificate-identity "https://github.com/getnabbed/agent/.github/workflows/chart-release.yml@refs/tags/chart/v$VERSION" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-github-workflow-trigger push

# The archive from the Helm repository or the GitHub release:
gh attestation verify nabbed-agent-$VERSION.tgz --repo getnabbed/agent \
  --signer-workflow getnabbed/agent/.github/workflows/chart-release.yml
```

## Versions

The chart has its own version, separate from the agent's. `appVersion` is
the agent release the chart runs by default (`image.tag` defaults to it),
so each chart version pins an exact agent. A new agent release gets a chart
release that bumps `appVersion`. Set `image.tag` or `image.digest` to run
another agent.

Bump `version` in `Chart.yaml` in any pull request that changes the chart.
CI fails if the version is already released. Bump the patch for fixes, the
minor for new values, and the major when existing values change meaning or
an upgrade needs manual steps.

To release, tag the merged commit on `main`:

```sh
git tag -s chart/v0.1.2 -m "Helm chart 0.1.2" && git push origin chart/v0.1.2
```

The `Chart release` workflow checks the chart and checks that the tag
matches `Chart.yaml`. It pushes the chart to ghcr.io and signs it, creates
a GitHub release with the archive, and adds the archive to the Helm
repository's `index.yaml` on GitHub Pages.

## How it runs

- **StatefulSet, not Deployment.** An agent's credentials include a refresh
  token that rotates on every refresh. Two processes holding one identity
  refresh the same token, and the cloud revokes the agent. A StatefulSet
  never runs two pods on one claim. A Deployment's rolling update does.
- **One `data` claim per replica** (`persistence`), mounted at `/data`. The
  agent keeps its identity in `/data/agent`, a folder it creates, because it
  refuses a data dir that it does not own or that holds other files, as a
  fresh volume's root may. Never put credentials in a Secret: the agent
  must rewrite them.
- **Non-interactive.** The pod runs `agent run`, which pairs when the agent
  has no credentials. It logs JSON with the `verification_uri` and
  `user_code`. `NOTES.txt` has a loop that prints them for every replica.
  An expired code makes the agent exit, and Kubernetes restarts it with a
  new code.
- **Revoked agents.** The agent exits with code 3 and the pod crash-loops.
  Delete its `data-<pod>` claim and the pod to pair it again with a new
  identity.
- **Hardened by default.** It runs as uid 65532 with a read-only root
  filesystem, all capabilities dropped and no service account token.

There is no health endpoint yet, so the chart sets no probes. A pod counts
as ready once its process starts.

## Configuration

`config` is the agent's `agent.yaml` (see `contrib/agent.example.yaml`),
rendered into a ConfigMap and mounted at `/config/agent.yaml`. The agent
rejects unknown keys at start, and `helm test <release>` runs `agent config`
against the deployed settings to catch mistakes early.

The chart sets some settings itself through the environment, which takes
precedence over the file, so it refuses them in `config`:

| Setting | Set by | Value |
|---|---|---|
| `name` | pod name | `<release>-nabbed-agent-<n>`, as shown in the web app |
| `data_dir` | chart | `/data/agent` |
| `downloads.dir` | `downloads` | see below |
| `torrent.port` | `torrent.port` | `6881` |
| `tls.ca_file` | `tls.caBundle` or `tls.existingConfigMap` | `/config/ca/ca.pem` |
| `usenet.provider` | `usenet.existingSecret` or `usenet.providers` | file at `/run/secrets/nabbed/nntp` |

`config.usenet` is refused because it would put a password in a ConfigMap.
Use the web app where you can, since it seals providers to each agent.

Use `extraEnv` for anything else, such as `NABBED_LOG_LEVEL=debug`.

## Storage

### Shared folders (NFS)

`volumes` mounts folders into every agent. Each entry has a `mountPath`,
optionally `readOnly`, and `libraryRoots`, the folders under `mountPath`
the web app may import into. They are added to `library.allowed_roots`.
Each entry is backed by exactly one of `claim` or `source`.

**A claim the chart creates** (`claim.create: true`, the default), here
bound to an existing folder on your NAS through a static PersistentVolume:

```yaml
volumes:
  movies:
    mountPath: /media/movies
    libraryRoots: ["."]            # allows /media/movies itself
    claim:
      create: true
      storageClassName: nfs-csi
      size: 300Gi
      persistentVolume:
        csi:
          driver: nfs.csi.k8s.io
          volumeAttributes:
            server: nas.lan
            share: /export/media
            subDir: movies
        mountOptions: [nfsvers=4.1]
```

The claim is named `<fullname>-<entry>` unless you set `claim.name`. The
volume always has reclaim policy `Retain`, so deleting it or the claim
never touches the files. The claim and volume survive `helm uninstall`
(`claim.keep`, default true). Without `persistentVolume`, the storage class
provisions a new, empty volume, which suits a shared downloads area.

**An existing claim** (`claim.create: false`), which must be in the
release's namespace:

```yaml
volumes:
  tv:
    mountPath: /media/tv
    libraryRoots: ["."]
    claim:
      create: false
      name: media-tv
```

The settings that shape a new claim (`size`, `storageClassName`,
`accessModes`, `annotations`, `keep`, `persistentVolume`) are refused
here, so a value that would do nothing fails instead.

**Any other volume source**, used as is under `source`, such as `nfs`,
`hostPath` or `csi`:

```yaml
volumes:
  media:
    mountPath: /media
    source:
      nfs: {server: nas.lan, path: /export/media}
```

### Ownership and fsGroup

The agent writes files as `podSecurityContext.runAsUser`. Run it as the
user your media server expects, so that it can rename and delete what the
agent imports:

```yaml
podSecurityContext:
  runAsNonRoot: true
  runAsUser: 1000
  runAsGroup: 1000
  fsGroup: 1000
  fsGroupChangePolicy: OnRootMismatch
  seccompProfile: {type: RuntimeDefault}
```

Set `fsGroup` explicitly when you override it. Helm merges your values with
the defaults, so leaving the key out keeps `65532`.

`fsGroup` makes the agent's own block-storage claims writable. CSI drivers
with `fsGroupPolicy: File`, including csi-driver-nfs, apply it to NFS
volumes too. Unless the folder's root already has that group, group
read-write and the setgid bit, the kubelet then changes the group and
permissions of every file on the volume when the pod starts. Check your
driver and prepare the folder once on the NAS:

```sh
kubectl get csidriver nfs.csi.k8s.io -o jsonpath='{.spec.fsGroupPolicy}'
chgrp 1000 /export/media/movies && chmod g+rws /export/media/movies
```

The install notes warn about each volume this applies to. The built-in
`nfs` volume source ignores `fsGroup`.

### Downloads

By default each replica gets its own `downloads` claim (`downloads.size`,
100Gi) at `/downloads`. Import then copies finished files into the library.

Set `downloads.volume` to the name of a `volumes` entry to download onto
that shared volume instead, at `<mountPath>/<downloads.subPath>/<pod name>`.
Each agent has its own folder. When the library roots are on the same
volume, import renames files instead of copying them:

```yaml
downloads:
  volume: media        # /media/downloads/<pod name>
```

Keep `persistence` (the data claim) on block storage such as local-path,
Longhorn, Ceph RBD or a cloud disk, not NFS. The state database uses
SQLite's write-ahead log, which needs a local filesystem, and the data-dir
lock is unreliable on many network filesystems.

## Several agents

Set `replicas` to run more agents. They do not connect to each other. Each
one pairs separately, appears in the web app under its pod name, and holds
its own usenet providers. The cloud gives each download to one agent, and
agents that share `volumes` import into the same library.

Scaling down stops the highest-numbered agents but keeps their claims, as
set by `persistence.retentionPolicy`. Scaling back up resumes the same
identities. To retire an agent for good, revoke it in the web app, then
delete its `data-<pod>` claim.

## BitTorrent port

Outgoing connections work without any setup. Peers can also connect in
when the agent is reachable on `torrent.port`:

- `torrent.service.enabled: true` creates one Service per replica,
  `LoadBalancer` by default, so each agent has its own address on the same
  port. With MetalLB, set its address-pool annotation in
  `torrent.service.annotations`. The Service has a TCP port and a UDP port,
  so the load balancer must support mixed protocols.
- `torrent.service.type: NodePort` works with `replicas: 1` only, and needs
  `torrent.port` in the node port range. Peers are told the port the agent
  listens on, so the node port must match it.
- `torrent.hostPort: true` binds the port on the node. That allows one agent
  per node.

## Values

See [`values.yaml`](values.yaml). The files in [`ci/`](ci) are working
examples: the defaults, NFS with three agents, and host ports.

## Development

`make helm` lints the chart against every `ci/` values file, validates the
manifests with kubeconform and loads each generated `agent.yaml` with the
agent itself. It needs `helm`, `kubeconform` and `yq`.
