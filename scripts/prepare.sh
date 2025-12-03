#!/bin/bash

set -e

# Get script directory and project root
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# Change to project root
cd "$PROJECT_ROOT"

# Configuration - can be overridden by environment variables
IMAGE_NAME=${IMAGE_NAME:-"prices-app"}
IMAGE_TAG=${IMAGE_TAG:-"latest"}
DOCKERFILE=${DOCKERFILE:-"Dockerfile"}

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Function to print colored messages
print_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

print_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

print_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Check if Docker is installed and running
if ! command -v docker &> /dev/null; then
    print_error "Docker is not installed. Please install Docker first."
    exit 1
fi

if ! docker info &> /dev/null; then
    print_error "Docker is not running. Please start Docker first."
    exit 1
fi

# Check if Dockerfile exists
if [ ! -f "$DOCKERFILE" ]; then
    print_error "Dockerfile not found: $DOCKERFILE"
    exit 1
fi

# Build Docker image
print_info "Building Docker image: ${IMAGE_NAME}:${IMAGE_TAG}"
print_info "Using Dockerfile: $DOCKERFILE"

if docker build -t "${IMAGE_NAME}:${IMAGE_TAG}" -f "$DOCKERFILE" .; then
    print_info "Docker image built successfully: ${IMAGE_NAME}:${IMAGE_TAG}"
    
    # Show image info
    print_info "Image details:"
    docker images "${IMAGE_NAME}:${IMAGE_TAG}" --format "table {{.Repository}}\t{{.Tag}}\t{{.Size}}\t{{.CreatedAt}}"
    
    # Optionally show image ID
    IMAGE_ID=$(docker images "${IMAGE_NAME}:${IMAGE_TAG}" --format "{{.ID}}" | head -1)
    print_info "Image ID: $IMAGE_ID"
else
    print_error "Failed to build Docker image"
    exit 1
fi
