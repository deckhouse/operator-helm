---
title: "Release Notes"
description: "Release notes for Deckhouse operator-helm."
---

## v0.2.0

### New Features

* Reworked HelmClusterAddonRepository status semantics
* Support legacy OCI chart media type with incremental indexing
* Surface force reconcile progress and completion in status
* Report scheduled repository synchronization in status

### Bug Fixes

* Propagate force reconcile to internal sources

## v0.1.1

### Bug Fixes

* Fixed authentication issue when working with a private OCI repository

### Chore

* Documentation is now available to the AI agent built into the platform

## v0.1.0

### New Features

* Enforced restricted PSS

### Bug Fixes

* Forbidden to use system namespaces

### Chore

* Added changelog and release notes generation

## v0.0.8

### Bug Fixes

* Resolved race on module disable which could lead to application disruption

### Chore

* Watch shadow custom resources in module namespace only

## v0.0.7

### New Features

* Added ability to review chart default values in console during addon creation

## v0.0.6

### New Features

* Do not mark possible status conditions as initialized on reconcile

### Chore

* Added weight annotations to validation webhook

## v0.0.5

### Chore

* Minor documentation updates

## v0.0.4

### Chore

* Updated main documentation page alerts formatting

## v0.0.3

### New Features

* The first public alpha release with HelmClusterAddon, HelmClusterAddonChart, and HelmClusterAddonRepository CRDs support

## v0.0.2

### New Features

* Applied deckhouse runtime review recommendations

## v0.0.1

### New Features

* Initial release with basic capabilities
