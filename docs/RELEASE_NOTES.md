---
title: "Release Notes"
description: "Release notes for Deckhouse operator-helm."
---

## Unreleased

### Breaking Changes

* `HelmClusterAddonRepository` condition `Ready` changed its meaning. It now reports whether the repository is usable as a whole — auxiliary resources, the internal source object and a confirmed catalog read on the current spec — instead of only the readiness of the internal source object. Alerting and dashboards that treat `Ready` as "the internal source object is ready" must be reviewed.

### New Features

* `HelmClusterAddonRepository` reports `Reconciling` and `Stalled` conditions following the kstatus convention. They are present only while applicable.
* `HelmClusterAddonRepository` status carries `lastSuccessfulSyncTime`, `nextSyncTime` and `consecutiveFetchFailures`, and `kubectl get` shows `Last Sync`, `Age` and, with `-o wide`, `Next Sync` and the `Ready` message.
* Repository reads that fail are retried with an exponential backoff — 5m, 10m, 20m, 40m, up to one hour — instead of a fixed 5 minute interval.

### Bug Fixes

* A single chart version that is not valid semver no longer fails the whole repository synchronization; the version is skipped.

## v0.1.0

### New Features

* enforce restricted pss

### Bug Fixes

* forbid to use system namespaces

### Chore

* add changelog and release notes generation

## v0.0.8

### Bug Fixes

* resolve race on module disable which could lead to application disruption

### Chore

* watch shadow custom resources in module namespace only

## v0.0.7

### New Features

* add ability to review chart default values in console during addon creation

## v0.0.6

### New Features

* do not mark possible status conditions as intitialized on reconcile

### Chore

* add weight annotations to validation webhook

## v0.0.5

### Chore

* minor documentation updates

## v0.0.4

### Chore

* update main documentation page alerts formatting

## v0.0.3

### New Features

* the first public alpha release with HelmClusterAddon, HelmClusterAddonChart, and HelmClusterAddonRepository CRDs supoort

## v0.0.2

### New Features

* apply deckhouse runtime time review recommendations

## v0.0.1

### New Features

* initial release with basic capabilities

