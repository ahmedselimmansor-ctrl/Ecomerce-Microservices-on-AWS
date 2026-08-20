
## Deployment

The Kubernetes manifests live at
[`infra/k8s/base/services/order-service.yaml`](../../infra/k8s/base/services/order-service.yaml),
not in this directory. Kustomize refuses to reference files outside its root, and that
restriction is worth respecting rather than working around with `--load-restrictor
LoadRestrictionsNone`: a base that can reach anywhere on disk is not reproducible.

```bash
kubectl kustomize infra/k8s/overlays/prod   # render
make k8s-diff ENV=prod                      # diff against the cluster
make k8s-apply ENV=prod
```
