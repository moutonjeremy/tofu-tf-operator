# tofu-tf-operator — Document de design

> Operator Kubernetes indépendant (sans Flux) pour planifier et appliquer du
> OpenTofu / Terraform de façon déclarative, avec approbation et notifications.
>
> Groupe API : `infra.labbs.fr` · Module Go : `github.com/labbs/tofu-tf-operator`

---

## Table des matières

1. [Objectifs](#1-objectifs)
2. [Vue d'ensemble](#2-vue-densemble)
3. [Ressources (CRDs)](#3-ressources-crds)
4. [Cycle de vie d'un Run](#4-cycle-de-vie-dun-run)
5. [Workspace controller](#5-workspace-controller)
6. [Run controller](#6-run-controller)
7. [Runner](#7-runner)
8. [State et backend](#8-state-et-backend)
9. [Variables et credentials](#9-variables-et-credentials)
10. [Notifications](#10-notifications)
11. [Sécurité et RBAC](#11-sécurité-et-rbac)
12. [Observabilité](#12-observabilité)
13. [Limites connues](#13-limites-connues)
14. [Développement local](#14-développement-local)

---

## 1. Objectifs

- Décrire un déploiement OpenTofu/Terraform dans un manifest (`Workspace`) et laisser
  l'operator faire `init` → `plan` → `apply`.
- Deux modes : application automatique, ou approbation manuelle du plan.
- Détection de drift périodique.
- Notifications des événements importants (plan avec changements, approbation requise,
  succès/échec, drift).
- Aucune dépendance à Flux : l'operator récupère lui-même les sources et envoie
  lui-même les notifications.
- Chaque étape s'exécute dans son propre Job, avec ses propres logs.

Hors périmètre initial : sources OCI, dépendances entre Workspaces, commentaires de PR,
policy-as-code. Voir `TASKS.md`.

## 2. Vue d'ensemble

```
                  ┌──────────────────────────── operator ────────────────────────────┐
 Workspace ──────▶│ Workspace controller                                             │
                  │   - résout la révision de la source (git ls-remote, hash)        │
                  │   - décide quand créer un Run (spec, révision, interval, manuel) │
                  │   - garbage collect l'historique, finalizer (destroy)            │
                  │        │ crée                                                    │
                  │        ▼                                                         │
                  │ Run controller                                                   │
                  │   - Job plan → (approbation) → Job apply                         │
                  │   - lit le résultat des Jobs, met à jour le status               │
                  │   - envoie les notifications                                     │
                  └───────────────┬──────────────────────────────────────────────────┘
                                  │ crée
                                  ▼
                 Job <run>-plan / <run>-apply  (image runner : binaire Go + tofu)
                   fetch source → init → plan|apply → Secrets (plan, outputs)
                   → termination message (résumé JSON)
```

Arborescence :

```
api/v1alpha1/          types Workspace, Run, NotificationProvider
cmd/main.go            manager (controllers)
cmd/runner/            binaire exécuté dans les Jobs
internal/controller/   reconcilers
internal/source/       résolution de révision + fetch (git, configmap, inline)
internal/engine/       installation des binaires tofu/terraform + vérification checksum
internal/runner/       exécution init/plan/apply, parsing du plan JSON
internal/notify/       envoi des événements (webhook)
config/                kustomize (CRDs, RBAC, manager)
test/e2e/              tests sur kind, providers random/null/local uniquement
```

## 3. Ressources (CRDs)

### Workspace

Le « fichier » qui décrit un déploiement. Exemple complet :

```yaml
apiVersion: infra.labbs.fr/v1alpha1
kind: Workspace
metadata:
  name: vpc-prod
  namespace: infra
spec:
  engine:
    type: tofu              # tofu (défaut) | terraform
    version: "1.10.0"       # vide = version embarquée dans l'image runner (tofu uniquement)
  source:                   # exactement une source
    git:
      url: https://github.com/acme/infra.git
      ref: { branch: main } # ou tag / commit ; vide = HEAD distant
      path: envs/prod/vpc
      secretRef: { name: git-creds }
      interval: 1m          # polling des nouveaux commits
    # configMap: { name: my-module }   # chaque clé = un fichier
    # inline: |                        # un seul main.tf
  vars:
    - name: region
      value: eu-west-3
    - name: tags
      value: { team: platform }        # types complexes acceptés
    - name: db_password
      valueFrom: { secretKeyRef: { name: db, key: password } }
  varsFrom:
    - { kind: Secret, name: tfvars }
  envFrom:
    - secretRef: { name: aws-credentials }
  backend:
    type: Kubernetes        # Kubernetes (défaut) | Source | Custom
  approvalMode: Manual      # Auto (défaut) | Manual
  interval: 30m             # drift detection ; désactivée si absent
  destroyOnDelete: false
  outputs: { secretName: vpc-prod-outputs }
  runner:
    serviceAccountName: ""  # vide = SA géré par l'operator
    timeout: 1h
  notifications:
    - providerRef: { name: ops-webhook }
      events: [ApprovalRequired, ApplyFailed, DriftDetected]   # vide = tous
  runHistoryLimit: 10
```

Validation dans la CRD (OpenAPI et CEL, testée avec envtest) : une seule source, `version`
obligatoire pour terraform, `custom` présent si et seulement si `backend.type: Custom`,
`git.path` relatif sans `..`, au plus une ref git. La règle « `value` xor `valueFrom` »
d'une variable est vérifiée par le controller. CEL ne voit pas `value`, qui est un JSON
non typé.

Status : conditions `Ready` et `SourceReady`, `sourceRevision`, `lastAppliedRevision`,
`lastAppliedAt`, `currentRun`, `lastRun`.

```
$ kubectl get ws
NAME       ENGINE   APPROVAL   READY   STATUS             RUN           AGE
vpc-prod   tofu     Manual     False   ApprovalRequired   vpc-prod-12   3d
```

### Run

Une exécution : un plan, puis un apply ou un destroy. Les Runs sont créés **par le
controller**, pas par l'utilisateur. Ils servent d'historique et d'audit. Le spec est
immuable (CEL `self == oldSelf`), sauf `approvedPlan`.

Les Runs s'appellent `<workspace>-<n>` (label `infra.labbs.fr/run-number`). Le numéro
ordonne l'historique. Comme le nom est déterministe, un Run recréé à partir d'un cache
périmé échoue en `AlreadyExists` au lieu de produire un doublon. Le nom d'un Workspace
est limité à 63 caractères (règle CEL), car il sert de valeur de label.

```
$ kubectl get runs
NAME          WORKSPACE   OPERATION   TRIGGER        PHASE              ADD   CHANGE   DESTROY   AGE
vpc-prod-12   vpc-prod    Apply       SourceChange   AwaitingApproval   2     1        0         5m
```

Approbation : `spec.approvedPlan` doit contenir le digest du plan revu
(`status.plan.digest`). Seul ce plan peut alors être appliqué : une approbation ne
peut ni précéder le plan, ni s'appliquer à un autre. Un digest qui ne correspond pas
laisse le Run en attente, avec un message explicatif.

```sh
kubectl tofu approve -n infra vpc-prod        # affiche le plan, demande confirmation
# équivalent sans le plugin (la commande est aussi dans status.message et dans la notification)
kubectl -n infra patch run vpc-prod-12 --type merge -p '{"spec":{"approvedPlan":"sha256:9c1f..."}}'
```

Le plugin `kubectl tofu` (`cmd/kubectl-tofu`, `make build-plugin`) propose aussi
`reconcile WORKSPACE`, qui demande un nouveau Run, et `logs RUN|WORKSPACE [--step plan|apply] [-f]`.

### NotificationProvider

Un endpoint de notification. Seul `webhook` est supporté pour l'instant.

```yaml
apiVersion: infra.labbs.fr/v1alpha1
kind: NotificationProvider
metadata: { name: ops-webhook, namespace: infra }
spec:
  type: webhook
  address: https://hooks.example.com/tofu
  secretRef: { name: webhook-credentials }  # clés optionnelles : address, token
  timeout: 15s
```

## 4. Cycle de vie d'un Run

```
Pending ──▶ Planning ──┬──▶ NoChanges                                   (fin)
                       ├──▶ Applying ──▶ Applied | Failed               (autoApprove)
                       ├──▶ AwaitingApproval ──(approved)──▶ Applying ──▶ Applied | Failed
                       └──▶ Failed
Pending | AwaitingApproval ──(nouvelle révision / nouveau spec)──▶ Superseded
```

- **Planning** : Job `<run>-plan`. Il produit le planfile, son résumé et son digest sha256.
- **AwaitingApproval** : uniquement si `autoApprove: false` et que le plan a des changements.
- **Applying** : Job `<run>-apply`. Il applique *exactement* le planfile produit par
  l'étape plan, après vérification du digest.
- **Échec** : `status.failureReason` vaut `StepFailed`, `Timeout` (dépassement de
  `runner.timeout`) ou `StalePlan` (le state a changé entre le plan et l'apply).
- **Superseded** : un Run en attente devient obsolète si la révision ou la génération du
  Workspace change, ou sur un déclenchement manuel. Le Workspace controller pose
  l'annotation `infra.labbs.fr/supersede` et le Run controller, seul à écrire le status
  des Runs, l'applique uniquement en `Pending`, `AwaitingApproval` ou à la fin du plan.
  Un Run en cours (Planning/Applying) n'est jamais interrompu : le suivant est créé une
  fois qu'il est terminé.
- Une approbation est liée au digest du plan (voir §3) : elle ne peut pas précéder la fin du plan.
- Les étapes plan et apply sont toujours deux Jobs distincts, chacun avec ses logs
  (`kubectl logs job/<run>-plan`). Le coût est un `init` en plus pour l'apply. Un cache
  de providers pourra réduire ce coût.

Opération `Destroy` : même cycle avec `plan -destroy`.

## 5. Workspace controller

À chaque réconciliation :

1. **Finalizer** : il est ajouté à la création. À la suppression, si `destroyOnDelete` et
   qu'au moins un apply a démarré, le controller crée un Run `Destroy` (trigger
   `Deletion`, auto-approuvé : la suppression vaut approbation) et attend `Applied`. Un
   Run en attente est d'abord marqué `Superseded`, un Run en cours est attendu. En cas
   d'échec, le finalizer reste et `Ready` passe à False avec la raison `DestroyFailed` :
   l'annotation `requested-at` relance le destroy, `destroyOnDelete: false` y renonce.
2. **Validation** du spec non couverte par la CRD (variables).
3. **Résolution de la révision** de la source :
   - git : `ls-remote` via go-git, révision `<ref>@sha1:<sha>`, polling tous les
     `git.interval` ;
   - configMap : `sha256:<hash du contenu>` (watch sur les ConfigMaps référencées) ;
   - inline : `sha256:<hash du contenu>`.
4. **Décision** : un nouveau Run est créé si aucun Run n'est actif et que l'une de ces
   conditions est vraie :
   - `generation` ≠ génération du dernier Run → `SpecChange` ;
   - révision ≠ révision du dernier Run → `SourceChange` ;
   - révision des variables ≠ celle du dernier Run → `InputChange` ;
   - annotation `infra.labbs.fr/requested-at` ≠ `status.lastHandledRequestedAt` → `Manual` ;
   - dernier Run échoué pour plan périmé (`failureReason: StalePlan`) et qui n'était
     pas lui-même une relance → `Retry` (une seule relance automatique) ;
   - `interval` défini et dernier Run terminé depuis plus de `interval` → `Interval`.

   Le dernier Run de référence est le plus récent qui n'est pas `Superseded`. Un Run
   `Failed` n'est pas relancé automatiquement, sauf plan périmé : l'`interval` ou
   l'annotation `requested-at` s'en chargent.

   Un Run `AwaitingApproval` est marqué `Superseded` pour tous les déclencheurs sauf
   `Interval` : une drift en attente d'approbation ne se re-notifie pas toutes les
   30 minutes.

   **Révision des variables** : c'est un hash des `resourceVersion` des Secrets et
   ConfigMaps référencés par `vars[].valueFrom` et `varsFrom`. Seules leurs métadonnées
   sont lues et surveillées, jamais leurs valeurs. Un Secret requis absent bloque la
   création des Runs (`Ready` passe à False avec la raison `InputsFailed`) au lieu de
   laisser un pod bloqué en `CreateContainerConfigError`. Une modification qui ne touche
   pas aux valeurs (un label par exemple) déclenche un plan sans changement.
   `env`/`envFrom` (credentials) ne déclenchent rien.
5. **Ressources du runner** : création du ServiceAccount/Role/RoleBinding `tofu-runner`
   dans le namespace si `runner.serviceAccountName` est vide.
6. **Historique** : suppression des Runs terminés au-delà de `runHistoryLimit` (les Jobs
   et Secrets du Run suivent via les ownerReferences).
7. **Status** : `Ready` reflète le dernier Run de la génération et de la révision
   courantes. `RequeueAfter` = min(`git.interval`, échéance de la drift detection).

`suspend: true` : aucun nouveau Run n'est créé, `Ready` passe à False avec la raison
`Suspended`.

## 6. Run controller

- Il watch les Runs et possède les Jobs (`Owns(&batchv1.Job{})`).
- Il crée le Job de l'étape courante s'il n'existe pas (nom déterministe `<run>-plan` /
  `<run>-apply`, idempotent).
- À la fin d'un Job, il lit le **termination message** du pod (JSON ≤ 4 Ko) : résumé du
  plan, digest, erreur éventuelle. Le runner n'a donc pas besoin de droits sur les Runs.
- Il relit le Workspace à la création de chaque Job (env, vars, runner) et passe le Run en
  `Superseded` si `workspace.generation` ≠ `spec.workspaceGeneration`.
- Il émet un Event Kubernetes et envoie les notifications à chaque transition. Les
  événements sont enregistrés dans `status.notifiedEvents` **avant** l'envoi : au plus un
  envoi par événement et par Run, même après un conflit ou un redémarrage.

Paramètres des Jobs :
- `backoffLimit: 0` : pas de retry automatique d'un apply ;
- `activeDeadlineSeconds` = `runner.timeout` ;
- `restartPolicy: Never`.

## 7. Runner

Un binaire Go (`cmd/runner`) dans une image dédiée, qui contient aussi `tofu`, `git` et
`openssh-client` (les modules `git::` en ont besoin). Ce n'est pas une image distroless.

Étape **plan** :
1. Récupère la source à la révision exacte : clone git au SHA, ConfigMap montée, ou
   contenu inline passé par le controller. Pour une ConfigMap, il vérifie que le hash
   correspond à la révision du Run, sinon il échoue avec « stale source ».
2. Installe le binaire si `engine.version` diffère de la version embarquée :
   `opentofu/tofudl` pour tofu, `hashicorp/hc-install` pour terraform. Le checksum de
   l'archive et la signature GPG du fichier de checksums sont vérifiés avec les clés
   d'OpenTofu et de HashiCorp embarquées dans ces bibliothèques. Pour un cluster sans
   accès internet, des mirrors se configurent par `spec.env` :
   - `TFOP_TOFU_API_URL` et `TFOP_TOFU_MIRROR_URL_TEMPLATE` (format tofudl) ;
   - `TFOP_TERRAFORM_RELEASES_URL` (même arborescence que releases.hashicorp.com).
3. Écrit l'override de backend (§8).
4. Lance `init -input=false`.
5. Lance `plan -input=false -out=tfplan -detailed-exitcode [-destroy] -var ...`.
6. Lance `show -json tfplan` et compte add/change/destroy/import ; un replace compte
   dans add et dans destroy, comme le CLI.
7. Stocke le planfile gzip dans le Secret `<run>-plan` (owner : le Run) et écrit le
   résumé dans le termination message.

Étape **apply** : il refait les étapes 1 à 4, récupère le planfile, vérifie son digest,
lance `apply -input=false tfplan`, puis `output -json` vers le Secret d'outputs.

Environnement : `TF_IN_AUTOMATION=1`, `-no-color`, logs sur stdout.

- **Arrêt :** sur SIGTERM (timeout du Job, suppression), le runner envoie SIGINT à tofu,
  qui a 2 minutes pour écrire le state et libérer le verrou. Le pod a un
  `terminationGracePeriodSeconds` de 150 s.
- **Erreurs :** le message d'erreur reprend la première ligne `Error:` de tofu, par
  exemple `tofu plan: exit code 1: Unsupported argument`. La fin des logs est jointe au
  résultat.
- **Pod :** non-root (65532), système de fichiers racine en lecture seule, capabilities
  supprimées. Il respecte le niveau Pod Security `restricted` (vérifié en e2e).

## 8. State et backend

- **Kubernetes** (défaut) : l'operator écrit `zz_operator_backend_override.tf` :
  ```hcl
  terraform {
    backend "kubernetes" {
      secret_suffix     = "<workspace>"
      namespace         = "<namespace>"
      in_cluster_config = true
    }
  }
  ```
  Le state va dans le Secret `tfstate-default-<workspace>`, le verrou dans un Lease. Le
  fichier override remplace un éventuel backend déclaré dans le code (à valider en e2e
  quand le code n'a pas de bloc `terraform {}`).
- **Source** : pas d'override, le backend du code est utilisé tel quel.
- **Custom** : le contenu de `backend.custom` est écrit comme override. Les credentials
  passent par `env`/`envFrom`.

Le Secret de state n'est pas supprimé avec le Workspace (perte de state = perte du suivi
des ressources). Il faut le supprimer à la main.

## 9. Variables et credentials

Les valeurs ne transitent jamais par l'operator : le controller construit le pod et c'est
le kubelet qui résout les références.

- `vars[].value` → env `TFOP_VAR_<name>` (chaîne telle quelle, sinon JSON) ;
- `vars[].valueFrom` → env `TFOP_VAR_<name>` avec `valueFrom` natif ;
- `varsFrom` → `envFrom` avec `prefix: TFOP_VAR_`.

Le runner convertit chaque `TFOP_VAR_x` en `-var x=...` sur le plan. Il ne garde que les
variables déclarées par le module (lecture des blocs `variable` des fichiers `.tf`,
`.tofu` et `.tf.json`), car `-var` sur une variable non déclarée est une erreur. Les
autres sont ignorées avec un avertissement dans les logs : un Secret importé par
`varsFrom` peut contenir d'autres clés. `-var` a la priorité
la plus haute dans OpenTofu/Terraform : le Workspace l'emporte donc sur les `*.tfvars` du
repo. Les valeurs non-chaînes sont parsées selon le type déclaré de la variable.
Priorité : `vars` > `varsFrom`, comme `env` > `envFrom` dans Kubernetes. Pour l'apply,
les variables sont déjà dans le planfile.

`env` / `envFrom` sont passés tels quels au conteneur (credentials cloud, `TF_LOG`...).
Les credentials git sont montés en volume depuis `git.secretRef`.

## 10. Notifications

Événements : `PlanHasChanges`, `PlanFailed`, `ApprovalRequired`, `ApplySucceeded`,
`ApplyFailed`, `DriftDetected` (plan avec changements sur un Run `Interval`),
`DestroySucceeded`, `DestroyFailed`.

Webhook : `POST` JSON, `Content-Type: application/json`,
`Authorization: Bearer <token>` si la clé `token` est présente.

```json
{
  "event": "ApprovalRequired",
  "timestamp": "2026-10-11T12:00:00Z",
  "workspace": { "namespace": "infra", "name": "vpc-prod" },
  "run": {
    "name": "vpc-prod-12",
    "operation": "Apply",
    "trigger": "SourceChange",
    "phase": "AwaitingApproval",
    "revision": "main@sha1:4f2c..."
  },
  "plan": { "hasChanges": true, "add": 2, "change": 1, "digest": "sha256:9c1f..." },
  "approveCommand": "kubectl -n infra patch run vpc-prod-12 --type merge -p '{\"spec\":{\"approvedPlan\":\"sha256:9c1f...\"}}'"
}
```

- L'envoi est asynchrone : un endpoint lent ne ralentit jamais la réconciliation.
- 3 tentatives (backoff 1 s puis 2 s) sur erreur réseau, 5xx ou 429 ; aucune sur les
  autres 4xx.
- Un échec produit un Event `Warning` `NotificationFailed` sur le Workspace.
- Un provider avec `suspend: true` ne reçoit rien.
- `plan` omet les compteurs à zéro. En cas d'échec, `message` contient l'erreur et les
  dernières lignes du log (2 Ko au plus).

Plus tard : Slack, Discord, Teams, GitHub (commit status / commentaire de PR), signature
HMAC du payload, templates.

## 11. Sécurité et RBAC

- **Operator** (ClusterRole) :
  - CRDs `infra.labbs.fr` ;
  - `jobs` ;
  - `pods` (lecture, pour le termination message) ;
  - `secrets` et `configmaps` (lecture des credentials git et des sources) ;
  - `serviceaccounts`, `roles`, `rolebindings` (SA du runner) ;
  - `events` ;
  - `leases`.
- **Runner** (Role par namespace) :
  - `secrets` en lecture et écriture (state, plan, outputs) ;
  - `leases` (verrou du backend).

  Le runner peut donc lire tous les Secrets du namespace. Recommandation : un namespace
  par équipe ou par niveau de confiance.
- Les credentials cloud ne sont visibles que dans les pods du runner, jamais dans les
  logs de l'operator.
- Approbation : le droit `patch` sur `runs` équivaut au droit d'appliquer. On peut le
  restreindre par RBAC.
- Webhooks : l'operator envoie des requêtes HTTP vers les adresses des
  NotificationProviders, depuis son propre réseau. Créer un NotificationProvider permet
  donc de faire appeler une URL interne par l'operator (payload fixe). Ce droit est à
  réserver par RBAC.
- Cache : les Secrets, ConfigMaps, Pods et objets RBAC ne sont pas mis en cache par
  l'operator, mais lus à la demande. Seules les métadonnées des ConfigMaps sont
  surveillées, pour réagir aux changements de source. Le cache des Jobs est filtré sur
  le label du runner.
- Licence : Terraform ≥ 1.6 est sous BSL. L'image n'embarque que tofu ; terraform est
  téléchargé à la demande.

## 12. Observabilité

- **Logs** : un Job par étape, `kubectl logs job/<run>-plan|apply`.
- **Events Kubernetes** sur le Workspace et le Run à chaque transition.
- **Métriques Prometheus** (endpoint metrics du manager) :
  - `tofu_operator_workspace_ready{namespace,workspace,reason}` : 1 si Ready, la raison
    est celle de la condition (`ApprovalRequired`, `RunFailed`...) ;
  - `tofu_operator_runs_total{namespace,workspace,operation,phase}` ;
  - `tofu_operator_step_duration_seconds{step,result}` : durée des Jobs, sans l'attente
    d'approbation ;
  - `tofu_operator_drift_detected_total{namespace,workspace}` ;
  - `tofu_operator_notifications_total{namespace,provider,result}`.

  Une transition n'est comptée qu'une fois son status enregistré. Les séries d'un
  Workspace supprimé sont retirées.

## 13. Limites connues

- **Taille des Secrets (1 Mio)** : elle limite le state du backend Kubernetes et le
  planfile (qui embarque un snapshot du state). Pour les gros states, utiliser un backend
  S3/GCS ; le stockage des plans en objet sera une évolution.
- **Coût du double init** : chaque Run avec changements fait deux `init`. Un cache de
  providers (PVC ou mirror) viendra plus tard.
- **Exécutions concurrentes** : un seul Run actif par Workspace. Les Workspaces sont
  indépendants entre eux (pas de `dependsOn` pour l'instant).

## 14. Développement local

- **make bloqué sur macOS** : si `make` renvoie le code 69, la licence Xcode n'est pas
  acceptée. Soit `sudo xcodebuild -license accept`, soit
  `export DEVELOPER_DIR=/Library/Developer/CommandLineTools`.
- **Conteneurs avec podman** :
  `make docker-build CONTAINER_TOOL=podman`, et `KIND_EXPERIMENTAL_PROVIDER=podman`
  pour kind.
- **Commandes** :
  - `make test` : tests envtest (vrai kube-apiserver), dont la validation des CRDs ;
  - `make lint` : golangci-lint ;
  - `make test-e2e` : cluster kind dédié.
- **Après modification des `*_types.go`** : `make manifests generate`.
