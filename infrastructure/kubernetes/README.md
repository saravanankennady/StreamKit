# Kubernetes

The Helm chart in `infrastructure/helm/elnova-streamkit` is the supported way to deploy to Kubernetes. To get plain
manifests, render it: `helm template streamkit infrastructure/helm/elnova-streamkit > streamkit.yaml`.

See `docs/deployment/self-hosting.md` for what the chart assumes: host networking on media nodes, PostgreSQL and
Redis provided separately, and a Secret holding credentials.
