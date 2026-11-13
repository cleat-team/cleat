{{- /*
  The decision behind the 0.5.0 to 0.6.0 stop-first guard (cleat#3758). TEMPORARY: it goes away after
  0.6.0 (cleat#3759). See upgrade-guard.yaml for why it exists and what it does not cover.

  This is a pure function of its inputs so that a test can pin it without a cluster
  (tests/manifests/a_chart_refuses_a_live_050_upgrade_test.go); the lookup and the fail are in
  upgrade-guard.yaml. It returns the refusal message, or nothing.

  Inputs (a dict):
    liveImage     the image of the worker container in the Deployment that is running now
    liveReplicas  that Deployment's spec.replicas, as an integer
    newImage      repository:tag of the image this upgrade would deploy
    allow         the operator's override (upgrade.allowLiveWorkers). It fails CLOSED: only the boolean true
                  or the string "true" skips the guard. `not .allow` is NOT used, because a non-empty string
                  is truthy and a quoted "false" would switch the guard off (cleat#3763, cleat-review).
    fullname      the Deployment's name         namespace  the release's namespace
    name          the chart's name label        release    the release name

  It refuses only when ALL of these hold:
    - workers are running (replicas > 0) and not overridden;
    - the live image is IDENTIFIABLY a 0.5.x release: a tag 0.5, 0.5.N or v0.5.N (optionally with a
      -pre-release, +build or _suffix such as _amd64, and optionally followed by @sha256:...);
    - the image being deployed is not 0.5.x.
  The tag "latest", a digest with no tag, or any other tag says nothing about the version, so the
  guard stays silent for them rather than guess.
*/ -}}
{{- define "cleat.upgradeRefusal" -}}
{{- $v05 := `:v?0\.5(\.[0-9]+)?([-+_][^@/:]*)?(@sha256:[0-9a-f]+)?$` -}}
{{- if and (not (eq (toString .allow) "true")) (gt .liveReplicas 0) (regexMatch $v05 .liveImage) (not (regexMatch $v05 .newImage)) -}}
refusing to upgrade {{ .release }}: {{ .liveReplicas }} worker(s) of {{ .liveImage }} are running in namespace {{ .namespace }}.

An upgrade from 0.5.x is stop-first (owner decision D79). The migration runs as a pre-upgrade hook
BEFORE any pod is replaced, so the 0.5.x workers would be running while the schema migrates, and
then beside the new workers while the rollout proceeds. Stop them, wait until they are gone, and
run the same helm upgrade again:

  kubectl scale deployment/{{ .fullname }} --replicas=0 -n {{ .namespace }}
  kubectl wait --for=delete pod -l app.kubernetes.io/name={{ .name }},app.kubernetes.io/instance={{ .release }} -n {{ .namespace }} --timeout=180s

What the replica count is afterwards depends on autoscaling (cleat#3761). With autoscaling.enabled=false,
Helm restores replicaCount when it applies the new chart. With it on (the default) the chart sets no replica
count: the HorizontalPodAutoscaler stays off while the Deployment is at 0, and a Helm 4 server-side apply leaves
it at 0, so check it after the upgrade and, if it reads 0, bring the workers back yourself:

  kubectl scale deployment/{{ .fullname }} --replicas=<autoscaling.minReplicas> -n {{ .namespace }}

The autoscaler takes over from there.

To skip this check, set upgrade.allowLiveWorkers=true. That is the unsupported case.

This check reads the live Deployment's image tag only. It does not fire for the tag "latest" or for
a digest with no tag, and it does not see pods that are still terminating, which is why the wait
step above is there. See docs/operations/upgrading.md ("Moving from 0.5.0 to 0.6.0").
{{- end -}}
{{- end -}}
