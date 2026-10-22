# Private deployment configuration

The public flake exports both a reusable NixOS module and a host configuration.
Keep machine-specific additions in a separate private deployment flake. Extend
`streamctl.nixosConfigurations.streamctl` with `extendModules`, supplying your
private modules, and pin the public input to a reviewed commit. Keep its lock
file with the private configuration, not in this repository's public lock file.

The Makefile optionally reads `../.private/deploy.mk`. For example:

```make
DEPLOY_FLAKE := path:$(abspath ../.private/deployment)\#streamctl
```

`make deploy`, `make deploy-local-build`, and `make deploy-dry` all use this
selection. Without a local override, they use `.#streamctl`. A command-line
`DEPLOY_FLAKE=...` takes precedence. Build and local development commands still
use the public application flake.

The root `.private/` directory is ignored by the public repository. It can hold
a separate Git checkout; back it up or connect it to a private remote. A fresh
public clone does not include these local settings. Restore them before
deploying a host that has private additions, otherwise activation would remove
those additions.

To deploy the current public checkout while retaining private additions, use
this override (assuming the private flake names its public input `streamctl`):

```make
DEPLOY_FLAKE_ARGS := --override-input streamctl "git+file://$(abspath ..)?dir=streamctl-system" --no-write-lock-file
```

All three deployment targets pass these arguments to Nix. The Git input includes
tracked local changes, just like deploying the public flake directly; add new
files to Git before deploying. Ignored private files are excluded. There is no
need to push commits or update the private application's revision for each
deployment. `--no-write-lock-file` leaves the private lockfile intact; its pinned
revision remains the fallback for deployments without this override.

Keep private infrastructure resources in their own Terraform project and state.
When separating existing resources, back up state and use `terraform state mv`
to transfer ownership before planning either project. Verify resource IDs and
plans; removing declarations without moving state can schedule deletions. Do
not commit Terraform state, plans, credentials, or private host settings here.

## October 2026 security baseline

The committed Nix lock uses nixpkgs `da6dc0e7b26060f2f71ce3ef7088fb56ee44fa36`.
Deployment explicitly builds the Go service with Go 1.27.3 and selects nginx
1.30.6 stable. This includes the [nginx security fixes](https://nginx.org/en/security_advisories.html)
and the Go certificate-validation update; the pinned NixOS trust store also
removes the compromised eMudhra roots. Rebuild and deploy the entire NixOS
configuration to update both the application binary and host trust store.
Updating source or restarting the old binary alone does not apply these fixes.
