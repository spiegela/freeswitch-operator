# Deploying with Flux

This directory is an **example fleet layout**, not something Flux reads from this repository. Copy the shape into
your fleet repository (the one Flux bootstraps from), one `clusters/<name>/` per cluster.

```
clusters/<name>/
  freeswitch-operator.yaml   # where the operator comes from (a tag of this repository) and how it's applied
  telephony.yaml             # the edge itself, applied after the operator
apps/telephony/
  kustomization.yaml
  namespace.yaml
  freeswitchcluster.yaml     # the FreeswitchCluster for this cluster
  # FreeSWITCH config ConfigMaps, and Kamailio with its dispatcher ConfigMap mount, go here too
```

- **Promotion is a pull request that moves a tag.** QA follows a release candidate tag, production the release tag.
- **The operator goes first.** `telephony.yaml` uses `dependsOn` so the CRDs exist before any `FreeswitchCluster`.
- **Pin the operator image.** `freeswitch-operator.yaml` sets the manager image with Flux's `images` override, so the
  tag of the manifests and the tag of the image move together.
- **Flux image automation** can open the tag-bump pull requests once the image is published by CI.
