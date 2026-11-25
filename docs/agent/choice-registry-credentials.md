# Registry credentials reach the controller and the deploy pod by different routes

The controller gets them per call. The deploy pod gets them as a file. That is not an inconsistency to tidy up later - the two consumers sit behind different upstream APIs, and the right answer differs.

This is the same problem cluster-api-provider-cargoship solved in its own [`choice-registry-credentials.md`](https://github.com/colonel-byte/cluster-api-provider-cargoship/blob/main/docs/agent/choice-registry-credentials.md), and the API here is deliberately named after that one - `pullSecretRef`, `caBundleSecretRef`, `fromZarfState`, `insecureSkipTLSVerify` - so an operator running both reads one vocabulary. The mechanism diverges, and this records why.

## The controller can use the in-process hook; cargoship could not

`internal/resolver` builds its remote itself, by calling `zoci.NewRemote` directly. That means `zoci.RemoteClientOptions` is ours to fill:

```go
type RemoteClientOptions struct {
	CachePath      string
	CredentialFunc auth.CredentialFunc  // nil falls back to the Docker credential store
	Transport      *http.Transport      // proxies and mTLS
	types.RemoteOptions                 // PlainHTTP, InsecureSkipTLSVerify
}
```

So resolution passes a `CredentialFunc` built for that one call, and a `Transport` whose root pool is the system roots plus whatever `caBundleSecretRef` names. Nothing is process-wide, which matters here in a way it did not for cargoship: this controller reconciles every proxy in the cluster, and two proxies may name two registries with two different credentials. A per-call hook makes that a non-question.

Cargoship had no such hook - `distro.Load` builds the oras modifier list internally from `RemoteOptions` alone - so it writes a merged Docker config file on its cache volume and accepts a documented last-writer-wins where two resources name the same registry with different credentials. We do not inherit that shortcut.

### The scheme negotiator is scoped per call for the same reason

`zoci.NewRemote` probes for plain HTTP when `PlainHTTP` is set, through `ocischeme.From(ctx)` - a package-level negotiator that caches its answer per registry host for the whole process, failures included for 30 seconds. In a CLI that is right; in this controller it means a proxy that cannot verify a registry's certificate caches that failure *for every other proxy naming the same registry*, correctly configured or not. The e2e suite found it the honest way: three cases that should have resolved inherited the refusal of the case that should not have. So `internal/resolver` installs a negotiator per resolution. The zero TTL is the second half of the same problem - a long-lived process holding a scheme decision forever - and a per-call negotiator answers both.

## The deploy pod gets a file, because it has no other way

`packager.LoadOptions` - what the deploy subcommand calls `LoadPackage` with - carries `types.RemoteOptions` and nothing else:

```go
type LoadOptions struct {
	Shasum, Architecture string
	VerifyBlobOptions    *signing.VerifyBlobOptions
	Filter               filters.ComponentFilterStrategy
	// ...
	types.RemoteOptions   // PlainHTTP, InsecureSkipTLSVerify
	layout.VerificationStrategy
}
```

No credential function, no transport. The fallback oras takes when no credential function is set is the Docker credential store, and `getDockerConfigPath()` reads `$DOCKER_CONFIG` before `~/.docker`. So the controller writes the credentials into the release's deploy-spec Secret, the Job mounts it, and the pod's `DOCKER_CONFIG` points at the mount. The CA arrives the same way, trusted through `SSL_CERT_FILE`, which Go's `crypto/x509` honours on Linux.

Two things make the file acceptable here that are worth stating, because "a credential on a disk" is where review attention goes:

- **One pod serves exactly one package.** There is no merging, no shared file, and no window where one pull reads another's credentials - the scoping problem cargoship had to solve with a mutex and a rename does not arise, because the process-wide variable is scoped to a process that exists to perform one pull.
- **The pod is already the blast radius.** It mounts the target cluster's admin kubeconfig. A registry pull credential is strictly less than what it already holds, and the pod runs as `65532` from an image with no shell.

It is also reversible: if `LoadOptions` ever carries a credential function, the pod uses the same per-call hook the controller does and the file goes away. The mount is the concession, not the design.

## `fromZarfState` reads the management cluster, not the workload cluster

The field exists because the common air-gapped arrangement is a Zarf registry in the management cluster holding the packages the fleet pulls - and Zarf already generated that registry's credentials and signed its certificate. Both are in that cluster as `zarf-state` and `zarf-registry-server-tls`, so making the operator export them by hand into a Secret whose server string has to match the repository host exactly is asking them to copy data that is already there.

Which cluster is the part to get right. Every other Zarf-state read in this controller targets a *workload* cluster, through the CAPI kubeconfig secret - the init gate, probe-and-adopt, removal. This one reads the controller's own cluster, because that is where the registry being pulled *from* lives. A workload cluster's Zarf registry is not a package source: its address in state is node-local, and nothing in the management cluster can route to it.

The repository is still named in the spec rather than derived from that state, for the reason cargoship found: Zarf records the address a node reaches the registry at, and a controller inside the cluster wants the service address, which the state does not carry.

## No client certificate, and that is measured

A Zarf registry reports mTLS in its state for any `mtlsStrategy` other than `none`, and the obvious reading - that a client certificate is therefore required - is wrong. Zarf's registry chart sets `clientauth: verify-client-cert-if-given`, which verifies a certificate where the client offers one and serves a client that offers none. The certificate Zarf issues belongs to the per-node registry proxy, not to anything a management-cluster controller runs. So the CA out of `zarf-registry-server-tls` is the whole requirement.

The case this does not cover is a registry that genuinely requires a client certificate - Harbor or Nexus configured for it, or a Zarf registry whose `clientauth` an operator overrode. That needs a `clientCertSecretRef` here and, for the deploy pod, somewhere to put it: the pod's half runs through `LoadOptions`, which carries no transport, and `SSL_CERT_FILE` trusts roots rather than presenting a certificate. So it is an upstream change first. Deferred deliberately, not overlooked.
