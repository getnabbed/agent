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
{{- $src := omit $v "mountPath" "readOnly" "libraryRoots" }}
{{- if ne (len $src) 1 }}
{{- fail (printf "volumes.%s needs exactly one volume source such as nfs or persistentVolumeClaim, got %v" $name (keys $src | sortAlpha)) }}
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
