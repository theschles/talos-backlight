# Third-party software

The Go controller imports github.com/siderolabs/talos/pkg/machinery v1.14.2, which is part of Talos Linux and is licensed under MPL-2.0. Its source and license are available at https://github.com/siderolabs/talos/tree/v1.14.2.

All Go module versions are recorded in controller/go.mod and controller/go.sum. Python deployment tools use PyYAML, which is licensed under MIT and is pinned in requirements.txt. This repository does not vendor their source. Dependency licenses apply independently of this project's MIT license. Redistributed binaries and container images must retain applicable dependency license notices and source availability obligations.
