# Changelog

## [0.1.0](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/compare/v0.0.1...v0.1.0) (2026-11-21)


### ⚠ BREAKING CHANGES

* harden the seed-then-manage handoff semantics ([#23](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/issues/23))

### Features

* add controller manager and ZarfPackageProxy reconciler skeleton ([#4](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/issues/4)) ([5053bbe](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/commit/5053bbe94465d10bf9aada31cecf19cf48c8620a))
* **api:** add ZarfPackageProxy and ZarfPackageRelease v1alpha1 types ([#3](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/issues/3)) ([e891751](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/commit/e89175101c01f4ed87661152628611536fc47538))
* hand the deploy pod its inputs as mounted files only ([#19](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/issues/19)) ([45d4455](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/commit/45d4455c93e611c83bb7547cee251f21ca996906))
* run each package deploy in a Kubernetes Job ([#9](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/issues/9)) ([bc3bac1](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/commit/bc3bac1d223c5e0f1e70f36a44af42b821571aa7))


### Bug Fixes

* **deps:** update k8s.io/utils digest to ca04ba5 ([#21](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/issues/21)) ([c7f363e](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/commit/c7f363e821b6909a2b960003aa8eea7ff489aebe))
* route release events to proxies via packageProxyRef ([#18](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/issues/18)) ([a18a74b](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/commit/a18a74bfa53264f3102471b68f50c55ff4acfd8a))


### Documentation

* add the clusterctl provider contract ([#15](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/issues/15)) ([31343b0](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/commit/31343b0baae7c4d4d2463eb62c636e3af5d326f7))
* add uninstall semantics and deletion policy ([#12](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/issues/12)) ([05da727](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/commit/05da727cc0bec260e9c8a06370bbe97b3430b98e))
* adopt cargoship's signature trust model ([#16](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/issues/16)) ([7b3d251](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/commit/7b3d251abfe10db2818b82c13698920415c17876))
* anchor the ApplyOnce record on the workload cluster ([#11](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/issues/11)) ([03f487d](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/commit/03f487d79ce0ea9cc9e61a01bcb6bee88f443ca9))
* define reconcile-time dependency gating verdicts ([#13](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/issues/13)) ([f828220](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/commit/f8282201c6ae2e178ddfaf1137f73c454e937d62))
* define the deploy Job contract in the reconcile workflow ([#17](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/issues/17)) ([44a116b](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/commit/44a116b725be4335e18b300d7e26e18f623272ef))
* harden the seed-then-manage handoff semantics ([#23](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/issues/23)) ([80ff591](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/commit/80ff591b07813be807fd24317b406fb885229f3a))
* pin package identity to a resolved digest ([#14](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/issues/14)) ([68c4df1](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/commit/68c4df1be1fe1acef0ddb0edad896d68d72cb201))


### Miscellaneous

* bootstrap repository from template for CAAPZ ([#2](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/issues/2)) ([78441fb](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/commit/78441fb5eef906eaefdb7d7d3762488a17a5e42b))
* **deps:** update github/codeql-action action to v4.39.4 ([#8](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/issues/8)) ([6f961d9](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/commit/6f961d983613ab4a3223409c144b86863b979fb9))
* render CRD and RBAC manifests ([#5](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/issues/5)) ([7af6b90](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/commit/7af6b90143a3724f8b55a91b3001bef08630b8fe))
* render deploy-job CRD and RBAC manifests ([#10](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/issues/10)) ([b54f7b2](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/commit/b54f7b2668f03a76dd5d863a07397bb67e6000ac))


### CI/CD

* align with template renovate ([#22](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/issues/22)) ([128c7ed](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/commit/128c7ed0a87f4a7f920ca6be30067d21a33f896b))
