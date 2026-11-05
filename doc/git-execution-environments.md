# Bundled Git binaries

Because Git is at the heart of almost all interfaces provided by Gitaly, Gitaly
must have access to Git. To ensure Gitaly always has access to Git,
Gitaly accesses bundled Git binaries. These binaries are not a complete
Git installation but only contains a subset of binaries that are required at runtime.
Bundled Git binaries are directly embedded into the Gitaly binary during compilation.

Bundled Git binaries are compiled whenever Gitaly is compiled. There's no
need to explicitly build and install them. If you're running a test without
using the `make test...` Makefile target, you must run `make build-bundled-git`
to ensure the binaries are present in the `_build/bin` directory.

When Gitaly is compiled for production, the bundled Git binaries are embedded
into the Gitaly binary using the go:embed feature. See `packed_binaries.go`.

Bundled Git binaries all have a `gitaly-` prefix so they do not clash with
normal Git executables. Furthermore, bundled Git binaries may have a version
suffix which allows Gitaly to use multiple different versions of Git at the
same time.

Bundled Git solves two important problems:

- Being able to use multiple Git versions in parallel allows us roll out new
  Git versions with the help of feature flags. This means we can roll back to
  old versions of Git if a new version is exhibiting faulty behavior.

- Embedding the Git binaries allows seamless zero-downtime upgrades, as Gitaly
  and Git can be upgraded as an atomic unit. Gitaly doesn't need to run in an
  intermediate state where the old version of Gitaly is unexpectedly using newer
  Git binaries.

Git binaries expect to be able to locate helper binaries at runtime. However,
because the bundled Git binaries all have a `gitaly-` prefix, the mechanisms Git
has to locate those helpers don't work.

To fix this, Gitaly has to bootstrap a Git execution environment on startup by:

- Creating a temporary directory.
- For each binary Git expects to exist, symlinking it into place.

To tell Git where to find those symlinks, we run all Git commands with the
`GIT_EXEC_PATH` environment variable that points into that temporary execution
environment.

## Switching between Git versions

Gitaly bundles two versions of Git:

- The latest version from the `master` branch (using a commit that's at least a week old), which is used when the `gitaly_git_master` feature flag is enabled.
- The preceding stable version for rollback purposes, which is used when the feature flag is disabled.

In gitai the flag is off by default, so the second one (`GIT_VERSION_PREV`, binaries with the
`-prev` suffix) serves every repository that does not use the MVCC reference backend (which has its
own `-mvcc` build). It is Git v2.56.0. The `master` pin (`GIT_VERSION_MASTER`) is a commit between
v2.56.0-rc1 and v2.56.0, older than the default. The repository has no renovate configuration, so
both pins are updated by hand.

[`renovate-gitlab-bot`](https://gitlab.com/gitlab-org/frontend/renovate-gitlab-bot) is configured to update the Git versions defined in the Gitaly Makefile and create a merge request every two weeks.
