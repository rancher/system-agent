# rancher-system-agent

`rancher-system-agent` is a daemon designed to run on a system and apply "plans" to the system. `rancher-system-agent` can support both local and remote plans, and was built to be integrated with the Rancher2 project for provisioning next-generation, CAPI driven clusters.

## Building

`make`

For containerized builds, use `make docker-build` or `make docker-build-suc`.
Both images use `registry.rancher.com/rancher/appco-go` to build the binary and
SUSE BCI nano for the runtime base. The runtime package layers retain OpenSSL,
the FIPS pattern, and the shell and core utilities needed by plans and SUC scripts.

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
