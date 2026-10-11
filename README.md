# tofu-tf-operator

Operator Kubernetes pour déployer du OpenTofu / Terraform de façon déclarative, sans
dépendre de Flux.

Un `Workspace` décrit une configuration (git, artefact OCI, ConfigMap ou HCL inline),
ses variables et son backend. L'operator exécute `plan` puis `apply` dans des Jobs, attend une
approbation si on le demande, détecte la drift et envoie des notifications.

- **Un Job par étape** : `kubectl logs job/<run>-plan` et `kubectl logs job/<run>-apply`.
- **Historique et audit** : chaque exécution est un objet `Run`, avec son plan
  (`+N ~M -K`) et son résultat.
- **Approbation** : `approvalMode: Manual` applique exactement le plan revu (vérifié
  par digest).
- **Drift detection** : `interval: 30m` refait un plan régulièrement.
- **Déclencheurs** : un nouveau commit ou tag OCI, une modification du spec, ou un
  changement des Secrets/ConfigMaps de variables lance un Run. Un `Receiver` (webhook
  GitHub, GitLab ou générique) fait vérifier la source tout de suite après un push.
- **State** : backend Kubernetes par défaut (Secret + Lease), ou n'importe quel autre
  backend.
- **Plans** : gardés entre le plan et l'apply dans un Secret, dans S3 (ou compatible)
  ou dans PostgreSQL, puis supprimés une fois appliqués.
- **Outputs** : exportés dans un Secret, utilisable par d'autres applications.
- **Destroy à la suppression** : avec `destroyOnDelete: true`.
- **Notifications** : webhook JSON.
- **Métriques** : Prometheus.

Le design détaillé est dans [docs/design.md](docs/design.md), l'avancement dans
[TASKS.md](TASKS.md) et les sujets reportés dans [docs/later.md](docs/later.md).

## Exemple

```yaml
apiVersion: infra.labbs.fr/v1alpha1
kind: Workspace
metadata:
  name: vpc-prod
  namespace: infra
spec:
  source:
    git:
      url: https://github.com/acme/infra.git
      ref: { branch: main }
      path: envs/prod/vpc
      secretRef: { name: git-creds }      # username/password ou identity/known_hosts
  vars:
    - name: region
      value: eu-west-3
    - name: tags
      value: { team: platform }
  varsFrom:
    - { kind: Secret, name: vpc-tfvars }
  envFrom:
    - secretRef: { name: aws-credentials }
  approvalMode: Manual
  interval: 30m
  outputs: { secretName: vpc-prod-outputs }
  notifications:
    - providerRef: { name: ops-webhook }
      events: [ApprovalRequired, ApplyFailed, DriftDetected]
---
apiVersion: infra.labbs.fr/v1alpha1
kind: NotificationProvider
metadata:
  name: ops-webhook
  namespace: infra
spec:
  address: https://hooks.example.com/tofu
```

```console
$ kubectl -n infra get ws,runs
NAME                                ENGINE   APPROVAL   READY   STATUS             RUN
workspace.infra.labbs.fr/vpc-prod   tofu     Manual     False   ApprovalRequired   vpc-prod-3

NAME                               WORKSPACE   OPERATION   TRIGGER        PHASE              ADD   CHANGE   DESTROY
run.infra.labbs.fr/vpc-prod-3      vpc-prod    Apply       SourceChange   AwaitingApproval   2     1        0

$ kubectl -n infra logs job/vpc-prod-3-plan                # le plan complet
$ kubectl tofu approve -n infra vpc-prod                   # affiche le plan, demande confirmation
Run vpc-prod-3 of Workspace vpc-prod (SourceChange, Apply)
Revision: main@sha1:4f2c0d2c5c8a4e0f6c1c2a3b4d5e6f708192a3b4
Plan: 2 to add, 1 to change, 0 to destroy
Full plan: kubectl -n infra logs job/vpc-prod-3-plan
Apply this plan? [y/N] y
run vpc-prod-3 approved (sha256:9c1f...)
```

L'approbation est liée au digest du plan : seul le plan revu peut être appliqué.

`kubectl get tofu` liste toutes les ressources de l'operator. Le plugin `kubectl tofu`
s'installe avec `make build-plugin`, puis en copiant `bin/kubectl-tofu` dans le `PATH`.

## Installation

Chaque release publie sur GitHub les images multi-arch (amd64, arm64)
`ghcr.io/labbs/tofu-tf-operator` et `ghcr.io/labbs/tofu-runner`, le chart Helm et un
`install.yaml`. Le manager lance par défaut le runner de sa propre version.

```sh
# Avec Helm
helm install tofu-tf-operator oci://ghcr.io/labbs/charts/tofu-tf-operator --version <version> \
  --namespace tofu-tf-operator-system --create-namespace

# Ou avec kubectl
kubectl apply -f https://github.com/labbs/tofu-tf-operator/releases/download/v<version>/install.yaml

# Plugin kubectl (linux/darwin/windows, amd64/arm64)
curl -sSL https://github.com/labbs/tofu-tf-operator/releases/download/v<version>/kubectl-tofu_<version>_linux_amd64.tar.gz \
  | tar -xz -C /usr/local/bin kubectl-tofu

# Exemple sans credentials cloud (provider random)
kubectl create namespace demo
kubectl -n demo apply -f https://raw.githubusercontent.com/labbs/tofu-tf-operator/v<version>/config/samples/infra_v1alpha1_workspace.yaml
```

Désinstallation : `helm uninstall` ou `kubectl delete -f install.yaml`. Le state des
Workspaces (Secrets `tfstate-default-<workspace>`) n'est jamais supprimé automatiquement.

### Depuis les sources

```sh
# Images ; le manager est construit avec RUNNER_IMG comme image par défaut du runner
make docker-build docker-push IMG=<registry>/tofu-tf-operator:<tag> RUNNER_IMG=<registry>/tofu-runner:<tag>
make docker-build-runner docker-push-runner RUNNER_IMG=<registry>/tofu-runner:<tag>
make deploy IMG=<registry>/tofu-tf-operator:<tag>
```

Le chart est dans `dist/chart` ; après une modification des CRDs ou du RBAC,
`make helm-chart` le regénère. Une image du runner peut aussi être imposée sans
reconstruire le manager : variable `RUNNER_IMAGE` (`--set manager.envOverrides.RUNNER_IMAGE=...`)
ou `spec.runner.image` d'un Workspace.

### Essai local avec kind et podman

```sh
export KIND_EXPERIMENTAL_PROVIDER=podman CONTAINER_TOOL=podman
make kind && bin/kind create cluster --name tofu-dev
make docker-build IMG=example.com/tofu-tf-operator:dev RUNNER_IMG=example.com/tofu-runner:dev
make docker-build-runner RUNNER_IMG=example.com/tofu-runner:dev
for image in example.com/tofu-tf-operator:dev example.com/tofu-runner:dev; do
  podman save -o /tmp/image.tar "$image" && bin/kind load image-archive /tmp/image.tar --name tofu-dev
done
make deploy IMG=example.com/tofu-tf-operator:dev
```

### Publier une release

```sh
git tag v0.1.0 && git push origin v0.1.0
```

Le workflow [release.yml](.github/workflows/release.yml) construit et pousse les images,
pousse le chart sur `oci://ghcr.io/labbs/charts` et crée la release GitHub, avec
`install.yaml`, les archives du plugin et leurs checksums. `make release-artifacts
VERSION=0.1.0 IMG=...` produit les mêmes fichiers en local, dans `dist/release`.

## Utilisation

| Besoin | Comment |
|---|---|
| Relancer un plan | `kubectl tofu reconcile <workspace>`, ou `kubectl annotate ws <name> infra.labbs.fr/requested-at="$(date +%s)" --overwrite` |
| Approuver un plan | `kubectl tofu approve <workspace>`, ou `kubectl patch run <run> --type merge -p '{"spec":{"approvedPlan":"<status.plan.digest>"}}'` |
| Mettre en pause | `spec.suspend: true` |
| Voir les logs | `kubectl tofu logs <workspace> [--step plan\|apply] [-f]`, ou `kubectl logs job/<run>-plan` |
| Choisir la version | `spec.engine: { type: tofu, version: "1.13.1" }` ; `terraform` exige une version. Les binaires téléchargés sont vérifiés (checksum + signature GPG) ; mirrors via `TFOP_TOFU_API_URL`, `TFOP_TOFU_MIRROR_URL_TEMPLATE`, `TFOP_TERRAFORM_RELEASES_URL` dans `spec.env` |
| Autre backend | `spec.backend: { type: Custom, custom: 'terraform { backend "s3" { ... } }' }` |
| Credentials | `spec.env` / `spec.envFrom`, ou un ServiceAccount (`spec.runner.serviceAccountName`) avec workload identity |
| Réessayer un destroy échoué | l'annotation `requested-at`, ou `destroyOnDelete: false` pour y renoncer |

### Sources

```yaml
source:
  git:                                   # commit d'une branche, d'un tag ou épinglé
    url: git@github.com:acme/infra.git
    ref: { tag: v1.4.0 }
    path: envs/prod
    secretRef: { name: git-ssh }         # identity + known_hosts, ou username + password
  # oci:                                 # artefact poussé avec `oras push` ou `flux push artifact`
  #   url: oci://ghcr.io/acme/modules/vpc
  #   ref: { tag: stable }               # ou { digest: sha256:... }
  #   secretRef: { name: ghcr-pull }     # Secret kubernetes.io/dockerconfigjson
  # configMap: { name: my-module }
  # inline: |
  #   resource "terraform_data" "x" {}
```

Un artefact OCI se publie par exemple avec
`oras push ghcr.io/acme/modules/vpc:stable *.tf` : le tag peut ensuite bouger, le
Workspace suit son digest.

### Stockage des plans

Un plan contient un snapshot du state et les valeurs des variables : son stockage doit
être protégé comme le state. Il est supprimé dès qu'il est appliqué.

```yaml
planStorage:
  type: S3                               # Secret (défaut, 1 Mio max), S3 ou Postgres
  s3:
    bucket: tofu-plans
    region: eu-west-3
    # endpoint: https://minio.example.com   # service compatible S3
    # pathStyle: true
    secretRef: { name: s3-plans }        # AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY ; sinon IRSA ou spec.env
# planStorage:
#   type: Postgres
#   postgres:
#     secretRef: { name: plans-db }      # clé url : postgres://user:pass@host:5432/db?sslmode=require
#     table: tofu_plans                  # créée si besoin
```

Avec S3, une règle de cycle de vie du bucket (expiration après quelques jours) nettoie
les plans jamais appliqués (Runs remplacés ou échoués). Avec Postgres, ils sont
supprimés après 30 jours.

### Webhooks (Receiver)

```yaml
apiVersion: infra.labbs.fr/v1alpha1
kind: Receiver
metadata: { name: infra-repo, namespace: infra }
spec:
  type: github                           # github, gitlab ou generic (Authorization: Bearer)
  secretRef: { name: receiver-token }    # clé token
  workspaceSelector:
    matchLabels: { app.kubernetes.io/part-of: infra }
```

`kubectl get receiver infra-repo -o jsonpath='{.status.webhookPath}'` donne le chemin
à appeler sur le Service `tofu-tf-operator-controller-manager-receiver` (port 80),
à exposer avec un Ingress. Côté GitHub, l'URL du webhook est
`https://<ingress>/hook/...`, avec le content type `application/json` et le token
comme secret. Un appel fait vérifier la source des Workspaces sélectionnés : un Run
démarre seulement si le commit ou le digest a changé. Le type `generic` sert aussi à
une CI, après un `oras push` :
`curl -X POST -H "Authorization: Bearer $TOKEN" https://<ingress>/hook/...`.

### Notifications

Les événements sont `PlanHasChanges`, `PlanFailed`, `ApprovalRequired`,
`ApplySucceeded`, `ApplyFailed`, `DriftDetected`, `DestroySucceeded` et
`DestroyFailed`. Chacun est envoyé une fois par Run, en `POST` JSON, avec
`Authorization: Bearer <token>` si le Secret du provider contient une clé `token`.
Le format est décrit dans le [design](docs/design.md#10-notifications).

### Métriques

Elles sont exposées sur l'endpoint metrics du manager :

| Métrique | Description |
|---|---|
| `tofu_operator_workspace_ready{namespace,workspace,reason}` | 1 si le Workspace est Ready |
| `tofu_operator_runs_total{namespace,workspace,operation,phase}` | Runs terminés |
| `tofu_operator_step_duration_seconds{step,result}` | Durée des Jobs plan/apply |
| `tofu_operator_drift_detected_total{namespace,workspace}` | Drifts détectées |
| `tofu_operator_notifications_total{namespace,provider,result}` | Notifications envoyées |

## Développement

```sh
make test       # tests unitaires, tests du runner avec le vrai tofu, envtest
make lint
# Tests optionnels : stockage Postgres et téléchargement des binaires officiels
TEST_POSTGRES_URL=postgres://postgres:test@localhost:5432/postgres?sslmode=disable go test ./internal/runner/
ENGINE_NETWORK_TESTS=1 go test ./internal/engine/
CONTAINER_TOOL=podman KIND_EXPERIMENTAL_PROVIDER=podman make test-e2e   # cluster kind dédié
```

Après modification des types (`api/v1alpha1`) : `make manifests generate`.

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
