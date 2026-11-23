{{- /*
  A resource REQUEST above its LIMIT is rejected by the Kubernetes API at apply time, and helm lint and helm template both pass it
  (cleat#4393). It is easy to do by accident: Helm deep-merges an install's values over the chart's, so
      --set resources.limits.memory=512Mi
  alone leaves the chart's default requests.memory (2Gi) above the new 512Mi limit, and an install whose values were valid against the
  old defaults (a 128Mi request) is refused by `helm upgrade`. This turns that late API error into an early one that says what to set.

  It is a pure function of the resources map so a test can pin it. It returns the refusal message, or nothing.

  Quantities understood: a plain decimal, optionally with m (milli), a binary suffix (Ki Mi Gi Ti Pi Ei) or a decimal one (k M G T P E).
  Anything else (an exponent, a template-built string) is NOT compared and never refuses: the guard must not turn a valid chart
  value it cannot parse into an error.
*/ -}}
{{- define "cleat.quantityValue" -}}
{{- $q := toString . -}}
{{- if regexMatch "^[0-9]+(\\.[0-9]+)?(Ki|Mi|Gi|Ti|Pi|Ei|k|M|G|T|P|E|m)?$" $q -}}
{{- $num := float64 (regexFind "^[0-9]+(\\.[0-9]+)?" $q) -}}
{{- $suf := regexFind "[A-Za-z]+$" $q -}}
{{- $mult := dict "" 1.0 "m" 0.001 "k" 1000.0 "M" 1000000.0 "G" 1000000000.0 "T" 1000000000000.0 "P" 1000000000000000.0 "E" 1000000000000000000.0 "Ki" 1024.0 "Mi" 1048576.0 "Gi" 1073741824.0 "Ti" 1099511627776.0 "Pi" 1125899906842624.0 "Ei" 1152921504606846976.0 -}}
{{- mulf $num (get $mult $suf) -}}
{{- else -}}
-1
{{- end -}}
{{- end -}}

{{- define "cleat.resourcesRefusal" -}}
{{- $res := default (dict) . -}}
{{- $requests := default (dict) $res.requests -}}
{{- $limits := default (dict) $res.limits -}}
{{- range $name := list "cpu" "memory" -}}
{{- $req := get $requests $name -}}
{{- $lim := get $limits $name -}}
{{- if and $req $lim -}}
{{- $r := include "cleat.quantityValue" $req | float64 -}}
{{- $l := include "cleat.quantityValue" $lim | float64 -}}
{{- if and (ge $r 0.0) (ge $l 0.0) (gt $r $l) -}}
resources.requests.{{ $name }} ({{ $req }}) is above resources.limits.{{ $name }} ({{ $lim }}), and Kubernetes rejects a pod whose request exceeds its limit. Helm merges your values over the chart's defaults, so setting only one of resources.requests.{{ $name }} and resources.limits.{{ $name }} leaves the other at the chart's default (requests: cpu 500m, memory 2Gi; limits: cpu 2, memory 2Gi): set BOTH, with the request at or below the limit. The defaults are in values.yaml, with what a worker needs.
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
