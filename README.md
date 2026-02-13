# argocd-extension-pod-files
An Argo CD extension to upload or download files from pods in Kubernetes, with support for multi-cluster environments.

https://github.com/user-attachments/assets/ba607c9c-e0f2-4f34-b497-fbb04698519b

## Usage

### Install ArgoCD Extension Pod Files to Kubernetes cluster

[Helm](https://helm.sh) must be installed to use the charts.  Please refer to
Helm's [documentation](https://helm.sh/docs) to get started.

Once Helm has been set up correctly, add the repo as follows:
```sh
helm repo add argocd-extension-pod-files https://rufusmen.github.io/argocd-extension-pod-files
helm repo update
```
If you had already added this repo earlier, run `helm repo update` to retrieve
the latest versions of the packages.  You can then run `helm search repo
<alias>` to see the charts.

To install the chart:
```sh
helm install -n <argocd namespace> argocd-extension-pod-files argocd-extension-pod-files/argocd-extension-pod-files
```

To install with multi-cluster support enabled:
```sh
helm install -n <argocd namespace> argocd-extension-pod-files argocd-extension-pod-files/argocd-extension-pod-files \
  --set multiCluster.enabled=true \
  --set multiCluster.argocdNamespace=argocd
```

To uninstall the chart:
```sh
helm delete -n <argocd namespace> argocd-extension-pod-files
```

### Setup ArgoCD config
Assume you install ArgoCD by this [helm chart](https://github.com/argoproj/argo-helm/tree/main/charts/argo-cd).  
  
**Enable Argo CD extension init container**  
https://github.com/argoproj/argo-helm/blob/main/charts/argo-cd/values.yaml#L1783
```yaml
## Argo CD extensions
## This function in tech preview stage, do expect instability or breaking changes in newer versions.
## Ref: https://github.com/argoproj-labs/argocd-extension-installer
## When you enable extensions, you need to configure RBAC of logged in Argo CD user.
## Ref: https://argo-cd.readthedocs.io/en/stable/operator-manual/rbac/#the-extensions-resource
extensions:
  # -- Enable support for Argo CD extension
  enabled: true

  ...

  # -- Extensions for Argo CD
  # @default -- `[]` (See [values.yaml])
  ## Ref: https://github.com/argoproj-labs/argocd-extension-metrics#install-ui-extension
  extensionList:
    - name: extension-pod-files
        env:
          - name: EXTENSION_URL
            value: http://argocd-extension-pod-files.<argocd namespace>/ui/extension.tar.gz
  
  ...
```

**Enable `server.enable.proxy.extension` and add `extension.config` like this [link](https://argo-cd.readthedocs.io/en/stable/developer-guide/extensions/proxy-extensions/#configuration)**  
https://github.com/argoproj/argo-helm/blob/main/charts/argo-cd/values.yaml#L228
```yaml
# Argo CD configuration parameters
## Ref: https://github.com/argoproj/argo-cd/blob/master/docs/operator-manual/argocd-cmd-params-cm.yaml
params:
  # -- Create the argocd-cmd-params-cm configmap
  # If false, it is expected the configmap will be created by something else.
  create: true

  ...

  # Enable the experimental proxy extension feature
  server.enable.proxy.extension: "true"

  ...
```
https://github.com/argoproj/argo-helm/blob/main/charts/argo-cd/values.yaml#L161
```yaml
## Argo Configs
configs:
  # General Argo CD configuration
  ## Ref: https://github.com/argoproj/argo-cd/blob/master/docs/operator-manual/argocd-cm.yaml
  cm:
    # -- Create the argocd-cm configmap for [declarative setup]
    create: true

    ...

    extension.config: |
      extensions:
        - name: pod-files
          backend:
            services:
              - url: http://argocd-extension-pod-files.<argocd namespace>

    ...
```

**Enable multi-cluster support (Optional)**
If you manage applications across multiple Kubernetes clusters with ArgoCD, enable multi-cluster support:
```yaml
# When installing the extension
helm install -n argocd argocd-extension-pod-files argocd-extension-pod-files/argocd-extension-pod-files \
  --set multiCluster.enabled=true \
  --set multiCluster.argocdNamespace=argocd
```

This grants the extension permission to read cluster secrets from the ArgoCD namespace, allowing it to access pods in remote clusters. The extension will automatically:
- Read cluster credentials from ArgoCD cluster secrets
- Generate temporary kubeconfig files per request
- Execute kubectl commands against the correct target cluster

**Grant permission for admin in `policy.csv`**
https://github.com/argoproj/argo-helm/blob/main/charts/argo-cd/values.yaml#L313
```yaml
# Argo CD RBAC policy configuration
## Ref: https://github.com/argoproj/argo-cd/blob/master/docs/operator-manual/rbac.md
rbac:
  # -- Create the argocd-rbac-cm configmap with ([Argo CD RBAC policy]) definitions.
  # If false, it is expected the configmap will be created by something else.
  # Argo CD will not work if there is no configmap created with the name above.
  create: true

  ...

  policy.csv: |
    p, role:admin, extensions, invoke, pod-files, allow

  ...
```

## Features

### Multi-Cluster Support
The extension supports accessing pods across multiple Kubernetes clusters managed by ArgoCD:

- **In-Cluster Access**: Works out-of-the-box for pods in the same cluster as ArgoCD
- **Remote Cluster Access**: Automatically detects and connects to remote clusters when multi-cluster is enabled
- **Secure Credential Management**: Uses ArgoCD's existing cluster secrets (no additional credentials needed)
- **Backward Compatible**: Existing installations continue to work without changes

### How It Works
1. The UI extracts cluster information from the ArgoCD application's destination
2. The backend retrieves cluster credentials from ArgoCD cluster secrets
3. A temporary kubeconfig is generated for the target cluster
4. `kubectl cp` executes against the correct cluster
5. Temporary files are automatically cleaned up

### Configuration

#### Multi-Cluster Configuration (values.yaml)
```yaml
# Enable multi-cluster support
multiCluster:
  enabled: true              # Default: true
  argocdNamespace: "argocd"  # Namespace where ArgoCD stores cluster secrets

# Environment variables
env:
  - name: ARGOCD_NAMESPACE
    value: "argocd"
```

#### RBAC Requirements
When multi-cluster is enabled, the extension requires:
- **Read access to secrets** in the ArgoCD namespace (automatically granted via Role/RoleBinding)
- **RBAC on target clusters**: The ArgoCD service account must have exec permissions on pods in remote clusters

The Helm chart automatically creates:
- A `Role` in the ArgoCD namespace to read cluster secrets (labeled `argocd.argoproj.io/secret-type=cluster`)
- A `RoleBinding` to grant the extension's service account access

### Usage with Remote Clusters

1. **Register a remote cluster with ArgoCD:**
   ```bash
   argocd cluster add my-remote-cluster
   ```

2. **Deploy an application to the remote cluster:**
   ```yaml
   apiVersion: argoproj.io/v1alpha1
   kind: Application
   metadata:
     name: my-app
   spec:
     destination:
       server: https://my-remote-cluster.example.com
       namespace: default
   ```

3. **Access pods in the ArgoCD UI:**
   - Navigate to the application
   - Click on a pod resource
   - Use the "Pod Files" extension to upload/download files

The extension automatically detects the cluster destination and handles authentication.

### Security Considerations
- Temporary kubeconfig files use `0600` permissions (owner read/write only)
- Credentials are never logged or cached
- Automatic cleanup with defer statements
- Least-privilege RBAC (only read access to cluster secrets)
- No credential storage (fetched fresh per request)

### Troubleshooting

#### Multi-Cluster Not Working
1. Verify multi-cluster is enabled:
   ```bash
   helm get values -n argocd argocd-extension-pod-files
   ```

2. Check RBAC permissions:
   ```bash
   kubectl get role -n argocd argocd-extension-pod-files-cluster-secrets
   kubectl get rolebinding -n argocd argocd-extension-pod-files-cluster-secrets
   ```

3. Verify cluster secrets exist:
   ```bash
   kubectl get secrets -n argocd -l argocd.argoproj.io/secret-type=cluster
   ```

4. Check extension logs:
   ```bash
   kubectl logs -n argocd -l app.kubernetes.io/name=argocd-extension-pod-files
   ```

#### Common Errors
- **"Cluster not found"**: The cluster URL doesn't match any ArgoCD cluster secret. Verify the cluster is registered with `argocd cluster list`.
- **"Authentication failure"**: The bearer token in the cluster secret is invalid or expired. Re-register the cluster.
- **"Permission denied"**: The ArgoCD service account lacks RBAC permissions on the target cluster. Grant exec permissions on pods.

   