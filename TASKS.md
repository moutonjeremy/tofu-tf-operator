# tofu-tf-operator — Plan de tâches

> Référence : `docs/design.md`
> Date de création : 2026-10-11

---

## Conventions

- **[ ]** tâche à faire · **[x]** tâche terminée
- Chaque phase se termine par `make lint test` vert et, à partir de la phase 1, par un
  scénario e2e sur kind
- Les tests e2e n'utilisent que des providers sans credentials (`random`, `null`, `local`)

---

## Phase 0 — Bootstrap

- [x] `git init`, scaffold kubebuilder v4 (domaine `labbs.fr`, module `github.com/labbs/tofu-tf-operator`)
- [x] APIs `Workspace`, `Run` (avec controllers) et `NotificationProvider` (sans controller)
- [x] Types v1alpha1 : spec, status, conditions, printer columns, catégorie `tofu`
- [x] Validation CEL + test envtest de la validation (`internal/controller/validation_test.go`)
- [x] Samples réalistes (`config/samples`)
- [x] `docs/design.md`
- [x] `kind` et `tofu` installés dans `bin/` par le Makefile (`make kind`, `make tofu`, checksum vérifié)
- [x] README : présentation, exemple, installation, utilisation, métriques
- [ ] CI GitHub Actions : vérifier les workflows scaffoldés (lint, test, test-e2e avec le runner)

## Phase 1 — MVP : plan/apply automatique

### Runner (`cmd/runner`, `internal/runner`, `internal/source`, `internal/engine`)
- [x] Wrapper d'exécution tofu/terraform (`init`, `plan -detailed-exitcode -out`, `show -json`, `apply`, `output -json`), logs en streaming, SIGINT sur annulation
- [x] Parsing du plan JSON → `PlanSummary` (add/change/destroy/import, replace compté deux fois)
- [x] Conversion `TFOP_VAR_*` → `-var`, limitée aux variables déclarées par le module
- [x] Override de backend (Kubernetes / Custom)
- [x] Sources : inline, ConfigMap (hash du contenu), git (clone au SHA via le CLI, auth HTTPS/SSH partagée avec les modules)
- [x] Stockage du planfile dans le Secret `<run>-plan` + vérification du digest à l'apply
- [x] Écriture des outputs dans un Secret (chaînes brutes, le reste en JSON compact)
- [x] Termination message JSON (≤ 4 Ko) avec la première erreur de tofu et la fin des logs
- [x] Installation d'une version précise de tofu/terraform avec vérification du checksum
- [x] Image runner (`Dockerfile.runner` : runner + tofu 1.13.1 + git + openssh, non-root)

### Workspace controller
- [x] Finalizer
- [x] Validation des variables (`value` xor `valueFrom`)
- [x] Résolution de révision : inline, ConfigMap (+ watch des métadonnées), git `ls-remote` (go-git)
- [x] Création des Runs (SpecChange, SourceChange, Manual via annotation), noms `<ws>-<n>` idempotents
- [x] ServiceAccount / Role / RoleBinding `tofu-runner`
- [x] Conditions `Ready` / `SourceReady`, `currentRun`, `lastRun`, `lastAppliedRevision`
- [x] GC de l'historique (`runHistoryLimit`)
- [x] `suspend`

### Run controller
- [x] Construction des Jobs plan/apply (env, vars, volumes, runner spec, timeout, securityContext restricted)
- [x] Machine d'états Pending → Planning → Applying → Applied / NoChanges / Failed
- [x] Lecture du termination message, `message` en cas d'échec
- [x] Superseded via l'annotation `infra.labbs.fr/supersede`

### Tests
- [x] Unitaires : parsing du plan, construction des Jobs, décisions du Workspace controller, sources, engine
- [x] Intégration runner avec le vrai binaire tofu (plan → apply dans un autre répertoire, plan périmé, destroy)
- [x] envtest : validation des CRDs + cycles complets en simulant la fin des Jobs
- [x] e2e kind : apply + outputs, approbation, erreur de configuration, destroy à la suppression

## Phase 2 — Approbation, drift, notifications

- [x] `approvalMode: Manual` → AwaitingApproval, `spec.approved` → Applying
- [x] Superseded d'un Run en attente sur nouveau spec, nouvelle révision ou déclenchement manuel
- [x] Drift detection (`interval`), événement `DriftDetected`
- [x] `destroyOnDelete` : Run Destroy au finalizer, `DestroyFailed`, relance via `requested-at`
- [x] `internal/notify` : webhook JSON, bearer token, retries, timeout, envoi asynchrone
- [x] Envoi à chaque transition, dédoublonnage via `status.notifiedEvents`
- [x] Events Kubernetes sur Workspace et Run
- [x] Métriques Prometheus (ready, runs, durées des Jobs, drift, notifications)
- [x] Déclencher un Run quand un Secret/ConfigMap de variables change (hash des `resourceVersion`, métadonnées seules)
- [x] Relancer une fois un Run échoué pour plan périmé (`failureReason: StalePlan`, trigger `Retry`)

## Phase 3 — Prêt pour la prod

- [x] Vérification des signatures GPG des binaires téléchargés (`tofudl`, `hc-install`), mirrors configurables
- [x] Chart Helm (`dist/chart`, plugin kubebuilder `helm/v2-alpha`, `make helm-chart`)
- [ ] Images multi-arch et release (goreleaser, publication des images manager/runner)
- [ ] Cache de providers (PVC ou mirror)
- [ ] Stockage des plans hors Secret (S3) pour les plans > 1 Mio
- [x] Approbation liée au digest (`spec.approvedPlan`) + plugin `kubectl tofu` (approve, reconcile, logs)
- [ ] Providers de notification : Slack, Discord, Teams, GitHub ; signature HMAC
- [ ] `dependsOn` entre Workspaces avec passage d'outputs
- [ ] Source OCI, receiver de webhook git (push → reconcile immédiat)
