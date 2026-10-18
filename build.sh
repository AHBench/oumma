#!/usr/bin/env bash
set -euo pipefail

IMAGE_NAME="${IMAGE_NAME:-oumma}"
IMAGE_TAG="${IMAGE_TAG:-latest}"
PLATFORMS="${PLATFORMS:-linux/amd64,linux/arm64}"
PUSH="${PUSH:-false}"
REGISTRY="${REGISTRY:-}"

FULL_TAG="${IMAGE_NAME}:${IMAGE_TAG}"
if [ -n "${REGISTRY}" ]; then
    FULL_TAG="${REGISTRY}/${FULL_TAG}"
fi

echo "================================================"
echo "  Building: ${FULL_TAG}"
echo "  Platforms: ${PLATFORMS}"
echo "  Push: ${PUSH}"
echo "================================================"

# Ensure a buildx builder exists.
BUILDER_NAME="oumma-builder"
if ! docker buildx inspect "${BUILDER_NAME}" >/dev/null 2>&1; then
    echo "Creating buildx builder: ${BUILDER_NAME}"
    docker buildx create --name "${BUILDER_NAME}" --driver docker-container --use
else
    docker buildx use "${BUILDER_NAME}"
fi

BUILD_ARGS=(
    --file Dockerfile
    --platform "${PLATFORMS}"
    --tag "${FULL_TAG}"
)

if [ "${IMAGE_TAG}" != "latest" ]; then
    if [ -n "${REGISTRY}" ]; then
        BUILD_ARGS+=(--tag "${REGISTRY}/${IMAGE_NAME}:latest")
    else
        BUILD_ARGS+=(--tag "${IMAGE_NAME}:latest")
    fi
fi

if [ "${PUSH}" = "true" ]; then
    BUILD_ARGS+=(--push)
else
    BUILD_ARGS+=(--load)
    # --load only supports a single platform; fall back to native.
    BUILD_ARGS=(
        --file Dockerfile
        --tag "${FULL_TAG}"
        --load
    )
fi

docker buildx build "${BUILD_ARGS[@]}" .

echo ""
echo "Done. Image: ${FULL_TAG}"

if [ "${PUSH}" != "true" ]; then
    echo ""
    echo "Run with:"
    echo "  docker run -d \\"
    echo "    -p 8080:8080 \\"
    echo "    -v \$(pwd)/workflows:/processes \\"
    echo "    -e OPENAI_API_KEY=sk-... \\"
    echo "    ${FULL_TAG}"
fi
