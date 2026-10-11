# Sujets reportés

> Sujets discutés mais volontairement laissés de côté pour l'instant. Chacun garde le
> contexte de la discussion pour pouvoir être repris sans repartir de zéro.

---

## Cache des providers

**Problème.** Chaque étape (plan, apply) est un Job neuf qui refait `init` et
retélécharge les providers. Pour un Workspace avec de gros providers (aws, google,
azurerm), cela ajoute des dizaines de secondes et du trafic à chaque Run.

**Pourquoi c'est reporté.** Un PVC monté dans les Jobs n'aide qu'à moitié : les Jobs
sont éphémères, un PVC `ReadWriteOnce` ne peut pas être partagé entre deux pods sur des
nœuds différents, et un `ReadWriteMany` n'est pas disponible partout. La bonne approche
reste à choisir.

**Pistes.**
- **Mirror de providers** (`provider_installation { network_mirror { ... } }`) servi dans
  le cluster, par l'operator lui-même ou par un composant dédié. Il fonctionne pour tous
  les Jobs sans stockage partagé.
- **PVC `ReadWriteMany`** monté comme `TF_PLUGIN_CACHE_DIR`. Simple mais dépendant du
  stockage disponible, avec des accès concurrents à gérer.
- **Image runner « pré-chauffée »** : providers embarqués dans une image dérivée, via
  `spec.runner.image`. Déjà possible aujourd'hui, à documenter.

## Partage d'outputs et dépendances entre Workspaces (`dependsOn`)

**Décision actuelle.** Ce n'est pas le rôle de l'operator. Les configurations qui
dépendent les unes des autres utilisent les mécanismes habituels :
`terraform_remote_state`, data sources, ou des valeurs stockées dans S3, SSM, etc.
L'operator exporte déjà les outputs d'un Workspace dans un Secret (`spec.outputs`), ce
qui couvre les consommateurs Kubernetes.

**À reconsidérer plus tard.**
- Un ordre d'exécution entre Workspaces (`dependsOn`) pour enchaîner des applies.
- Un stockage géré par l'operator pour simplifier ou accélérer le passage de valeurs
  entre Workspaces (outputs d'un Workspace → variables d'un autre).

## Autres sujets

- **Providers de notification natifs** (Slack, Discord, Teams, GitHub commit status /
  commentaire de PR) et signature HMAC des webhooks. Le webhook générique suffit pour
  l'instant.
- **Stockage d'état géré** : aujourd'hui le backend Kubernetes (limité à 1 Mio) ou
  n'importe quel backend via `backend.type: Custom`.
- **Valeurs par défaut au niveau de l'operator**, par exemple un stockage des plans
  commun à tous les Workspaces, configuré dans le chart, au lieu d'une configuration
  par Workspace.
- **Nettoyage actif des plans orphelins** (Runs `Superseded` ou `Failed`) dans S3 par
  l'operator, en complément des règles de cycle de vie du bucket.
