# rancher-system-agent

`rancher-system-agent` is a daemon designed to run on a system and apply "plans" to the system. `rancher-system-agent` can support both local and remote plans, and was built to be integrated with the Rancher2 project for provisioning next-generation, CAPI driven clusters.

## Branching and Versioning

* `main` is the primary development branch and always contains the latest changes.
* Tags from `main` are consumed by Rancher's `main` branch.
* Each Rancher release line has a corresponding `release/vX.Y` branch, created from `main`.
* Release branches only receive bug fixes and security patches.
* Release branch tags increment only the patch suffix (`.x`).
* Release candidates (RCs) may also be created from release branches before a stable release.
* Release candidates (RCs) tag format is `vX.Y.Z-rc.M` where `M` is incremented for each new RC.

Starting with releases after `v0.3.16`, system-agent minor versions are aligned with Rancher minor release lines:

| System-agent Branch | Rancher Release Line | Tag Format |
|---------------------|----------------------|------------|
| `main`              | `main`               | `vX.Y.Z`   |
| `release/v2.15`     | `v2.15`              | `v0.15.x`  |
| `release/v2.14`     | `v2.14`              | `v0.14.x`  |
| `release/v2.13`     | `v2.13`              | `v0.13.x`  |

Note: The `v0.3.x` release line uses the legacy versioning scheme and is not aligned with Rancher release lines. v0.3.16 is the final release in that series.

## Building

`make`

### Cross Compiling

You can also 

`CROSS=true make` if you want cross-compiled binaries for Darwin/Windows.

## Running

First, configure the agent by looking at the `examples/configuration` folder, then you can run the binary.

`./bin/rancher-system-agent`

## License
Copyright (c) 2026 [Rancher Labs, Inc.](http://rancher.com)

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

[http://www.apache.org/licenses/LICENSE-2.0](http://www.apache.org/licenses/LICENSE-2.0)

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
