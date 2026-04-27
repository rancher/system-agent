# rancher-system-agent

`rancher-system-agent` is a daemon designed to run on a system and apply "plans" to the system. `rancher-system-agent` can support both local and remote plans, and was built to be integrated with the Rancher2 project for provisioning next-generation, CAPI driven clusters.

## Versioning

Starting after v0.3.16, `system-agent` releases are aligned with Rancher minor release lines. The minor version of `system-agent` corresponds to the minor version of the Rancher release it is intended for:

| system-agent version | Rancher version |
|----------------------|-----------------|
| v0.3.x               | Legacy (independent versioning) |
| v0.14.x              | Rancher v2.14.x |
| v0.15.x              | Rancher v2.15.x |

v0.3.16 is the last release following the old versioning schema.

Note that only the **minor** versions are aligned — patch versions are incremented independently as needed. For example, Rancher v2.14.6 may reference system-agent v0.14.2.

### Branches

The `main` branch is used to cut releases for the most current minor release line. For previous release lines, dedicated branches are created following the pattern `release/v0.<minor>` (e.g., `release/v0.14`).

## Building

`make`

### Cross Compiling

You can also 

`CROSS=true make` if you want cross-compiled binaries for Darwin/Windows.

## Running

First, configure the agent by looking at the `examples/configuration` folder, then you can run the binary.

`./bin/rancher-system-agent`

## License
Copyright (c) 2021 [Rancher Labs, Inc.](http://rancher.com)

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

[http://www.apache.org/licenses/LICENSE-2.0](http://www.apache.org/licenses/LICENSE-2.0)

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
