# Copyright 2026 colonel-byte
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# syntax=docker/dockerfile:1

# The controller image goreleaser's dockers_v2 block builds, mirroring
# cluster-api-provider-cargoship's containers/manager/Dockerfile.
#
# The digest is pinned, and Renovate owns moving it. A floating tag in a controller image is how
# a management cluster ends up running something nobody chose.

# Chainguard static, matching the cargoship controllers' base image: no shell, no package
# manager, nothing to exec into. This controller reads every workload cluster's admin kubeconfig
# secret, so the image has nothing in it that could read process memory or open a socket. The
# deploy subcommand runs in deploy pods from this same image - the pod's inputs are mounted
# files, so it needs no shell either.
FROM cgr.dev/chainguard/static:latest@sha256:fc42af33e643f20d08baf03cddcc1c02ba57052dfb02edb0d79f3fc47bcea8d9

ARG TARGETPLATFORM

# Renamed to /manager so the deployment's command does not carry the project name.
COPY ${TARGETPLATFORM}/cluster-api-addon-provider-zarf /manager

USER 65532:65532

ENTRYPOINT ["/manager"]
