---
title: "Release Notes"
description: "Release notes for Deckhouse operator-helm."
---

## v0.1.1

### Bug Fixes

* Fixed authentication issue when working with a private OCI repository

### Chore

* Documentation is now available to the AI agent built into the platform

## v0.1.0

### New Features

* enforced restricted pss

### Bug Fixes

* forbidden to use system namespaces

### Chore

* added changelog and release notes generation

## v0.0.8

### Bug Fixes

* resolved race on module disable which could lead to application disruption

### Chore

* watch shadow custom resources in module namespace only

## v0.0.7

### New Features

* added ability to review chart default values in console during addon creation

## v0.0.6

### New Features

* do not mark possible status conditions as intitialized on reconcile

### Chore

* added weight annotations to validation webhook

## v0.0.5

### Chore

* minor documentation updates

## v0.0.4

### Chore

* updated main documentation page alerts formatting

## v0.0.3

### New Features

* the first public alpha release with HelmClusterAddon, HelmClusterAddonChart, and HelmClusterAddonRepository CRDs supoort

## v0.0.2

### New Features

* applied deckhouse runtime time review recommendations

## v0.0.1

### New Features

* initial release with basic capabilities

