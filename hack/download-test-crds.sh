#!/bin/bash

set -e

# Create directory for test CRDs
mkdir -p config/test-crds

# Download CAPI CRDs
echo "Downloading Cluster API CRDs..."
curl -L https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.13.4/cluster-api-components.yaml -o config/test-crds/cluster-api-crds.yaml

# Download any other required CRDs here
# Example:
# echo "Downloading Other CRDs..."
# curl -L <URL> -o config/test-crds/other-crds.yaml

echo "CRDs downloaded successfully to config/test-crds/" 