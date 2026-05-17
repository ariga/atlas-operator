#!/usr/bin/env bash
# Copyright 2026 The Atlas Operator Authors.
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

set -euo pipefail

VERSION="${VERSION:-latest}"
ARCH="$(dpkg --print-architecture || uname -m)"

case "$ARCH" in
  amd64 | x86_64) ARCH="amd64" ;;
  arm64 | aarch64) ARCH="arm64" ;;
  *) echo "Unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

if [ "$VERSION" = "latest" ]; then
  URL="https://storage.googleapis.com/skaffold/releases/latest/skaffold-linux-${ARCH}"
else
  # Strip leading 'v' if present for URL construction
  URL="https://storage.googleapis.com/skaffold/releases/${VERSION}/skaffold-linux-${ARCH}"
fi

echo "Installing Skaffold (${VERSION}) for ${ARCH}..."
curl -fsSL -o /tmp/skaffold "$URL"
install /tmp/skaffold /usr/local/bin/skaffold
rm -f /tmp/skaffold

skaffold version
echo "Skaffold installed successfully."
