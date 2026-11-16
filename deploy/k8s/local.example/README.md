# Local Kubernetes Environment Maintenance

Copy this directory to `deploy/k8s/local/` (gitignored) to keep cluster-specific Helm values and upgrade scripts out of the public repository while maintaining a single reproducible source of truth.

```bash
cp -r deploy/k8s/local.example deploy/k8s/local
# Edit deploy/k8s/local/values.yaml for your registry, RuntimeClass, StorageClass, and secrets
chmod +x deploy/k8s/local/upgrade.sh
```
