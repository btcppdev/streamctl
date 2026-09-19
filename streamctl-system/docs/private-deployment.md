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
those additions. Update the pinned public input when rolling out app changes.

Keep private infrastructure resources in their own Terraform project and state.
When separating existing resources, back up state and use `terraform state mv`
to transfer ownership before planning either project. Verify resource IDs and
plans; removing declarations without moving state can schedule deletions. Do
not commit Terraform state, plans, credentials, or private host settings here.
