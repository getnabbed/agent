{{/*
Names and labels.
*/}}
{{- define "nabbed-agent.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
The fullname is the pod name prefix, so it leaves room for "-<ordinal>"
and the controller-revision-hash label StatefulSets add.
*/}}
{{- define "nabbed-agent.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 52 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 52 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 52 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "nabbed-agent.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "nabbed-agent.selectorLabels" -}}
app.kubernetes.io/name: {{ include "nabbed-agent.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "nabbed-agent.labels" -}}
helm.sh/chart: {{ include "nabbed-agent.chart" . }}
{{ include "nabbed-agent.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "nabbed-agent.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "nabbed-agent.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "nabbed-agent.image" -}}
{{- if .Values.image.digest }}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else }}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag | toString) }}
{{- end }}
{{- end }}

{{/*
Fixed paths inside the container. The data dir is a folder inside the
claim, not the claim's root: the agent insists its data dir is owned by
its own uid and holds only its files, and a fresh volume's root is owned
by root and may hold lost+found.
*/}}
{{- define "nabbed-agent.dataMount" -}}/data{{- end }}
{{- define "nabbed-agent.dataDir" -}}/data/agent{{- end }}
{{- define "nabbed-agent.configDir" -}}/config{{- end }}
{{- define "nabbed-agent.secretsDir" -}}/run/secrets/nabbed{{- end }}

{{- define "nabbed-agent.usenetSecretName" -}}
{{- default (printf "%s-usenet" (include "nabbed-agent.fullname" .)) .Values.usenet.existingSecret }}
{{- end }}

{{- define "nabbed-agent.hasUsenet" -}}
{{- if or .Values.usenet.existingSecret .Values.usenet.providers }}true{{ end }}
{{- end }}

{{- define "nabbed-agent.hasCA" -}}
{{- if or .Values.tls.caBundle .Values.tls.existingConfigMap }}true{{ end }}
{{- end }}

{{/*
Where downloads go. With a shared volume, each pod gets its own folder,
so agents never see each other's work directories. $(POD_NAME) is
expanded by the kubelet from the env var of that name.
*/}}
{{- define "nabbed-agent.downloadDir" -}}
{{- if .Values.downloads.volume }}
{{- $v := index .Values.volumes .Values.downloads.volume }}
{{- if not $v }}
{{- fail (printf "downloads.volume %q is not an entry in volumes" .Values.downloads.volume) }}
{{- end }}
{{- $sub := trimAll "/" (default "" .Values.downloads.subPath) }}
{{- if $sub }}
{{- printf "%s/%s/$(POD_NAME)" (trimSuffix "/" $v.mountPath) $sub }}
{{- else }}
{{- printf "%s/$(POD_NAME)" (trimSuffix "/" $v.mountPath) }}
{{- end }}
{{- else }}
{{- print "/downloads" }}
{{- end }}
{{- end }}

{{/*
Checks the values once, so every template can rely on them.
*/}}
{{- define "nabbed-agent.validate" -}}
{{- $reserved := list "/data" "/config" "/downloads" "/tmp" "/run/secrets/nabbed" }}
{{- range $name, $v := .Values.volumes }}
{{- if not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" $name) }}
{{- fail (printf "volumes.%s: the name must be a DNS label (lowercase letters, digits and -)" $name) }}
{{- end }}
{{- if not $v.mountPath }}
{{- fail (printf "volumes.%s.mountPath is required" $name) }}
{{- end }}
{{- if not (isAbs $v.mountPath) }}
{{- fail (printf "volumes.%s.mountPath must be absolute, got %q" $name $v.mountPath) }}
{{- end }}
{{- range $reserved }}
{{- if or (eq (clean $v.mountPath) .) (hasPrefix (printf "%s/" .) (clean $v.mountPath)) }}
{{- fail (printf "volumes.%s.mountPath %s is inside %s, which the chart uses" $name $v.mountPath .) }}
{{- end }}
{{- end }}
{{- range $k, $_ := $v }}
{{- if not (has $k (list "mountPath" "readOnly" "libraryRoots" "claim" "source")) }}
{{- fail (printf "volumes.%s.%s is not a setting; put a volume source such as nfs under volumes.%s.source" $name $k $name) }}
{{- end }}
{{- end }}
{{- if ne (len (pick $v "claim" "source")) 1 }}
{{- fail (printf "volumes.%s needs exactly one of claim or source" $name) }}
{{- end }}
{{- with $v.claim }}
{{- range $k, $_ := . }}
{{- if not (has $k (list "create" "name" "size" "storageClassName" "accessModes" "annotations" "keep" "persistentVolume")) }}
{{- fail (printf "volumes.%s.claim.%s is not a setting" $name $k) }}
{{- end }}
{{- end }}
{{- if and (hasKey . "create") (not (kindIs "bool" .create)) }}
{{- fail (printf "volumes.%s.claim.create must be true or false" $name) }}
{{- end }}
{{- if include "nabbed-agent.claimCreate" . }}
{{- if not .size }}
{{- fail (printf "volumes.%s.claim.size is required with create: true" $name) }}
{{- end }}
{{- $modes := default (list "ReadWriteMany") .accessModes }}
{{- if and (gt (int $.Values.replicas) 1) (not (or (has "ReadWriteMany" $modes) (has "ReadOnlyMany" $modes))) }}
{{- fail (printf "volumes.%s.claim: replicas share this claim, so accessModes needs ReadWriteMany or ReadOnlyMany" $name) }}
{{- end }}
{{- with .persistentVolume }}
{{- $pvSrc := omit . "mountOptions" }}
{{- if ne (len $pvSrc) 1 }}
{{- fail (printf "volumes.%s.claim.persistentVolume needs exactly one volume source such as csi or nfs, got %v" $name (keys $pvSrc | sortAlpha)) }}
{{- end }}
{{- end }}
{{- else }}
{{- if not .name }}
{{- fail (printf "volumes.%s.claim.name is required with create: false: the existing claim in the release's namespace" $name) }}
{{- end }}
{{- $createOnly := keys (pick . "size" "storageClassName" "accessModes" "annotations" "keep" "persistentVolume") | sortAlpha }}
{{- if $createOnly }}
{{- fail (printf "volumes.%s.claim: %v apply only with create: true" $name $createOnly) }}
{{- end }}
{{- end }}
{{- end }}
{{- with $v.source }}
{{- if or (not (kindIs "map" .)) (ne (len .) 1) }}
{{- fail (printf "volumes.%s.source needs exactly one Kubernetes volume source, such as nfs or hostPath" $name) }}
{{- end }}
{{- end }}
{{- range $v.libraryRoots }}
{{- if or (isAbs .) (has ".." (splitList "/" .)) }}
{{- fail (printf "volumes.%s.libraryRoots: %q must be a folder under mountPath, without .." $name .) }}
{{- end }}
{{- end }}
{{- end }}
{{- if and .Values.downloads.volume (index .Values.volumes .Values.downloads.volume) }}
{{- if (index .Values.volumes .Values.downloads.volume).readOnly }}
{{- fail (printf "downloads.volume %q is mounted readOnly" .Values.downloads.volume) }}
{{- end }}
{{- end }}
{{- if and .Values.usenet.existingSecret .Values.usenet.providers }}
{{- fail "set usenet.existingSecret or usenet.providers, not both" }}
{{- end }}
{{- if and .Values.tls.caBundle .Values.tls.existingConfigMap }}
{{- fail "set tls.caBundle or tls.existingConfigMap, not both" }}
{{- end }}
{{- if and .Values.torrent.service.enabled (eq .Values.torrent.service.type "NodePort") }}
{{- if gt (int .Values.replicas) 1 }}
{{- fail "torrent.service.type NodePort works only with replicas: 1, since every agent listens on the same torrent.port; use LoadBalancer or hostPort" }}
{{- end }}
{{- if or (lt (int .Values.torrent.port) 30000) (gt (int .Values.torrent.port) 32767) }}
{{- fail "torrent.service.type NodePort needs torrent.port in the node port range (30000-32767): peers are told the port the agent listens on" }}
{{- end }}
{{- end }}
{{- if or (lt (int .Values.torrent.port) 1) (gt (int .Values.torrent.port) 65535) }}
{{- fail "torrent.port must be 1-65535" }}
{{- end }}
{{- end }}

{{/*
agent.yaml: values.config plus what the chart derives. Keys the chart
sets through the environment are refused, because the environment would
silently win over them.
*/}}
{{- define "nabbed-agent.config" -}}
{{- $cfg := deepCopy (default dict .Values.config) }}
{{- range list "name" "data_dir" }}
{{- if hasKey $cfg . }}
{{- fail (printf "config.%s is set by the chart; remove it from config" .) }}
{{- end }}
{{- end }}
{{- if hasKey (default dict $cfg.downloads) "dir" }}
{{- fail "config.downloads.dir is set by the chart; use downloads.volume and downloads.subPath" }}
{{- end }}
{{- if hasKey (default dict $cfg.torrent) "port" }}
{{- fail "config.torrent.port is set by the chart; use torrent.port" }}
{{- end }}
{{- if hasKey $cfg "usenet" }}
{{- fail "config.usenet would put a password in a ConfigMap; use usenet.existingSecret or usenet.providers" }}
{{- end }}
{{- if hasKey (default dict $cfg.tls) "ca_file" }}
{{- fail "config.tls.ca_file is set by the chart; use tls.caBundle or tls.existingConfigMap" }}
{{- end }}
{{- $_ := required "config.api_url is required: the agent pairs with it on first start" $cfg.api_url }}
{{- $lib := default dict $cfg.library }}
{{- $roots := default list $lib.allowed_roots }}
{{- range $name, $v := .Values.volumes }}
{{- range $v.libraryRoots }}
{{- $roots = append $roots (clean (printf "%s/%s" $v.mountPath .)) }}
{{- end }}
{{- end }}
{{- $_ = set $lib "allowed_roots" (uniq $roots) }}
{{- $_ = set $cfg "library" $lib }}
{{- if not $cfg.tls }}
{{- $_ = unset $cfg "tls" }}
{{- end }}
{{- toYaml $cfg }}
{{- end }}

{{/*
Environment shared by the agent and the config test. The chart-owned
settings go here, overriding the image's defaults.
*/}}
{{- define "nabbed-agent.env" -}}
- name: POD_NAME
  valueFrom:
    fieldRef:
      fieldPath: metadata.name
- name: NABBED_CONFIG
  value: {{ include "nabbed-agent.configDir" . }}/agent.yaml
- name: NABBED_NAME
  value: $(POD_NAME)
- name: NABBED_DATA_DIR
  value: {{ include "nabbed-agent.dataDir" . }}
- name: NABBED_DOWNLOAD_DIR
  value: {{ include "nabbed-agent.downloadDir" . }}
- name: NABBED_TORRENT_PORT
  value: {{ .Values.torrent.port | quote }}
{{- if include "nabbed-agent.hasCA" . }}
- name: NABBED_CA_FILE
  value: {{ include "nabbed-agent.configDir" . }}/ca/ca.pem
{{- end }}
{{- if include "nabbed-agent.hasUsenet" . }}
- name: NABBED_NNTP_FILE
  value: {{ include "nabbed-agent.secretsDir" . }}/nntp
{{- end }}
{{- with .Values.extraEnv }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{/*
Config, CA and secret mounts shared by the agent and the config test.
*/}}
{{- define "nabbed-agent.configVolumeMounts" -}}
- name: config
  mountPath: {{ include "nabbed-agent.configDir" . }}/agent.yaml
  subPath: agent.yaml
  readOnly: true
{{- if include "nabbed-agent.hasCA" . }}
- name: ca
  mountPath: {{ include "nabbed-agent.configDir" . }}/ca
  readOnly: true
{{- end }}
{{- if include "nabbed-agent.hasUsenet" . }}
- name: usenet
  mountPath: {{ include "nabbed-agent.secretsDir" . }}
  readOnly: true
{{- end }}
{{- end }}

{{- define "nabbed-agent.configVolumes" -}}
- name: config
  configMap:
    name: {{ include "nabbed-agent.fullname" . }}
    items:
      - key: agent.yaml
        path: agent.yaml
{{- if .Values.tls.caBundle }}
- name: ca
  configMap:
    name: {{ include "nabbed-agent.fullname" . }}
    items:
      - key: ca.pem
        path: ca.pem
{{- else if .Values.tls.existingConfigMap }}
- name: ca
  configMap:
    name: {{ .Values.tls.existingConfigMap }}
    items:
      - key: {{ .Values.tls.configMapKey }}
        path: ca.pem
{{- end }}
{{- if include "nabbed-agent.hasUsenet" . }}
- name: usenet
  secret:
    secretName: {{ include "nabbed-agent.usenetSecretName" . }}
    # Readable whatever uid the agent runs as; it is the only process in
    # the container.
    defaultMode: 0444
    items:
      - key: {{ .Values.usenet.secretKey }}
        path: nntp
{{- end }}
{{- end }}

{{/*
Whether a volumes entry's claim is created by the chart (create, default
true) rather than an existing one.
*/}}
{{- define "nabbed-agent.claimCreate" -}}
{{- if ne (toString .create) "false" }}true{{ end }}
{{- end }}

{{/*
The claim a volumes entry mounts: claim.name, or <fullname>-<entry> for
one the chart creates. The static PersistentVolume's name carries the
namespace, since PersistentVolumes are cluster-wide.
*/}}
{{- define "nabbed-agent.claimName" -}}
{{- if .v.claim.name }}
{{- .v.claim.name }}
{{- else }}
{{- printf "%s-%s" (include "nabbed-agent.fullname" .root) .name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{- define "nabbed-agent.pvName" -}}
{{- printf "%s-%s-%s" .root.Release.Namespace (include "nabbed-agent.fullname" .root) .name | trunc 253 | trimSuffix "-" }}
{{- end }}

{{/*
The pod's volume source for a volumes entry: its claim, or source as is.
*/}}
{{- define "nabbed-agent.volumeSource" -}}
{{- if .v.claim -}}
persistentVolumeClaim:
  claimName: {{ include "nabbed-agent.claimName" . }}
  {{- if .v.readOnly }}
  readOnly: true
  {{- end }}
{{- else }}
{{- toYaml .v.source }}
{{- end }}
{{- end }}

{{/*
The CSI driver behind a claim in the cluster, found through the volume
bound to it.
*/}}
{{- define "nabbed-agent.claimDriver" -}}
{{- with lookup "v1" "PersistentVolumeClaim" .root.Release.Namespace .claim }}
{{- with lookup "v1" "PersistentVolume" "" (dig "spec" "volumeName" "" .) }}
{{- dig "spec" "csi" "driver" "" . }}
{{- end }}
{{- end }}
{{- end }}

{{/*
The CSI driver behind a volumes entry that holds existing files: the
chart's static PersistentVolume, an existing claim, or a CSI source. A
claim the storage class provisions starts empty, so it has nothing to
re-permission. Empty when unknown, including under helm template, where
lookup finds nothing.
*/}}
{{- define "nabbed-agent.csiDriver" -}}
{{- $v := .v }}
{{- if $v.claim }}
{{- if include "nabbed-agent.claimCreate" $v.claim }}
{{- dig "persistentVolume" "csi" "driver" "" $v.claim }}
{{- else }}
{{- include "nabbed-agent.claimDriver" (dict "root" .root "claim" $v.claim.name) }}
{{- end }}
{{- else if $v.source.persistentVolumeClaim }}
{{- include "nabbed-agent.claimDriver" (dict "root" .root "claim" $v.source.persistentVolumeClaim.claimName) }}
{{- else }}
{{- dig "csi" "driver" "" $v.source }}
{{- end }}
{{- end }}
