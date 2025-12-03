#!/bin/bash

set -e

# Get script directory and project root
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# Change to project root
cd "$PROJECT_ROOT"

# Configuration - can be overridden by environment variables
YC_FOLDER_ID=${YC_FOLDER_ID:-""}
YC_ZONE=${YC_ZONE:-"ru-central1-a"}
YC_SUBNET_ID=${YC_SUBNET_ID:-""}
INSTANCE_NAME=${INSTANCE_NAME:-"prices-app-$(date +%s)"}
IMAGE_FAMILY=${IMAGE_FAMILY:-"ubuntu-2004-lts"}
IMAGE_FOLDER_ID=${IMAGE_FOLDER_ID:-"standard-images"}
CORES=${CORES:-2}
MEMORY=${MEMORY:-2}
DISK_SIZE=${DISK_SIZE:-10}
SSH_USER=${SSH_USER:-"ubuntu"}
POSTGRES_DB=${POSTGRES_DB:-"prices_db"}
POSTGRES_USER=${POSTGRES_USER:-"postgres"}
POSTGRES_PASSWORD=${POSTGRES_PASSWORD:-"postgres"}

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

# Check if yc CLI is installed
if ! command -v yc &> /dev/null; then
    print_error "Yandex Cloud CLI (yc) is not installed. Please install it first."
    exit 1
fi

# Check if yc is configured
if ! yc config list &> /dev/null; then
    print_error "Yandex Cloud CLI is not configured. Please run 'yc init' first."
    exit 1
fi

# Get folder ID if not set
if [ -z "$YC_FOLDER_ID" ]; then
    YC_FOLDER_ID=$(yc config get folder-id 2>/dev/null || echo "")
    if [ -z "$YC_FOLDER_ID" ]; then
        print_error "YC_FOLDER_ID is not set and cannot be determined from yc config."
        print_info "Please set YC_FOLDER_ID environment variable or configure yc with 'yc config set folder-id <folder-id>'"
        exit 1
    fi
fi

print_info "Using folder ID: $YC_FOLDER_ID"

# Test yc connectivity with the folder
print_info "Testing Yandex Cloud connectivity..."
if ! yc compute instance list --folder-id "$YC_FOLDER_ID" --limit 1 &>/dev/null; then
    print_error "Cannot connect to Yandex Cloud or access folder $YC_FOLDER_ID"
    print_info "Please check:"
    print_info "  1. Your internet connection"
    print_info "  2. Folder ID is correct: yc resource-manager folder list"
    print_info "  3. You have permissions to access this folder"
    exit 1
fi
print_info "Connectivity OK"

# Get default subnet if not set - must match the zone
if [ -z "$YC_SUBNET_ID" ]; then
    print_info "Subnet ID not specified, trying to find subnet in zone $YC_ZONE..."
    
    # Find subnet that matches the zone
    YC_SUBNET_ID=$(yc vpc subnet list --folder-id "$YC_FOLDER_ID" --format json 2>/dev/null | \
        python3 -c "
import sys, json
try:
    data = json.load(sys.stdin)
    for subnet in data:
        if subnet.get('zone_id') == '$YC_ZONE':
            print(subnet.get('id', ''))
            break
except:
    pass
" 2>/dev/null)
    
    # Fallback to grep if python didn't work
    if [ -z "$YC_SUBNET_ID" ]; then
        YC_SUBNET_ID=$(yc vpc subnet list --folder-id "$YC_FOLDER_ID" --format json 2>/dev/null | \
            grep -B5 "\"zone_id\": \"$YC_ZONE\"" | grep -o '"id": "[^"]*"' | head -1 | cut -d'"' -f4 || echo "")
    fi
    
    if [ -z "$YC_SUBNET_ID" ]; then
        print_warn "No subnet found in zone $YC_ZONE. Will let Yandex Cloud create one automatically."
    else
        print_info "Using subnet: $YC_SUBNET_ID (zone: $YC_ZONE)"
    fi
fi

# Generate SSH key pair if it doesn't exist
SSH_KEY_NAME="${INSTANCE_NAME}-key"
SSH_PRIVATE_KEY="${HOME}/.ssh/${SSH_KEY_NAME}"
SSH_PUBLIC_KEY="${SSH_PRIVATE_KEY}.pub"

if [ ! -f "$SSH_PRIVATE_KEY" ]; then
    print_info "Generating SSH key pair..."
    mkdir -p "${HOME}/.ssh"
    ssh-keygen -t rsa -b 4096 -f "$SSH_PRIVATE_KEY" -N "" -C "yc-${INSTANCE_NAME}"
    chmod 600 "$SSH_PRIVATE_KEY"
    chmod 644 "$SSH_PUBLIC_KEY"
fi

# Get public key content
PUBLIC_KEY_CONTENT=$(cat "$SSH_PUBLIC_KEY")

print_info "Creating VM instance: $INSTANCE_NAME"

# Create SSH metadata file
SSH_METADATA_FILE=$(mktemp)
cat > "$SSH_METADATA_FILE" <<EOF
#cloud-config
users:
  - name: $SSH_USER
    groups: sudo
    shell: /bin/bash
    sudo: ['ALL=(ALL) NOPASSWD:ALL']
    ssh-authorized-keys:
      - $PUBLIC_KEY_CONTENT
EOF

# Check if instance with this name already exists
if yc compute instance get "$INSTANCE_NAME" --folder-id "$YC_FOLDER_ID" &>/dev/null; then
    print_warn "Instance '$INSTANCE_NAME' already exists!"
    read -p "Do you want to delete it and create a new one? (y/N): " -n 1 -r
    echo
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        print_info "Deleting existing instance..."
        yc compute instance delete "$INSTANCE_NAME" --folder-id "$YC_FOLDER_ID" --quiet || true
        sleep 5
    else
        print_info "Using existing instance..."
        EXISTING_IP=$(yc compute instance get "$INSTANCE_NAME" --format json 2>/dev/null | \
            python3 -c "
import sys, json
try:
    data = json.load(sys.stdin)
    for interface in data.get('network_interfaces', []):
        if 'primary_v4_address' in interface:
            if 'one_to_one_nat' in interface['primary_v4_address']:
                print(interface['primary_v4_address']['one_to_one_nat']['address'])
                break
except:
    pass
" 2>/dev/null)
        if [ -n "$EXISTING_IP" ]; then
            print_info "Found existing instance with IP: $EXISTING_IP"
            echo "$EXISTING_IP"
            exit 0
        else
            print_error "Could not get IP of existing instance"
            exit 1
        fi
    fi
fi

# Create user-data script file with cloud-config (minimal - just SSH setup)
USER_DATA_FILE=$(mktemp)
trap "rm -f $USER_DATA_FILE $SSH_METADATA_FILE" EXIT

cat > "$USER_DATA_FILE" <<EOF
#cloud-config
users:
  - name: $SSH_USER
    groups: sudo
    shell: /bin/bash
    sudo: ['ALL=(ALL) NOPASSWD:ALL']
    ssh-authorized-keys:
      - $PUBLIC_KEY_CONTENT
EOF

# Build network interface parameter
if [ -n "$YC_SUBNET_ID" ]; then
    NETWORK_PARAM="subnet-id=$YC_SUBNET_ID,nat-ip-version=ipv4"
else
    NETWORK_PARAM="nat-ip-version=ipv4"
fi

# Create VM instance
print_info "Creating VM instance with network: $NETWORK_PARAM"
print_info "This may take a few minutes..."

# Check if timeout command is available (not available on macOS by default)
if command -v timeout &> /dev/null; then
    TIMEOUT_CMD="timeout 300"
else
    TIMEOUT_CMD=""
    print_warn "timeout command not available, VM creation may take longer"
fi

# Run the command and capture output
print_info "Running: yc compute instance create..."
print_info "Command parameters:"
print_info "  Name: $INSTANCE_NAME"
print_info "  Zone: $YC_ZONE"
print_info "  Cores: $CORES, Memory: ${MEMORY}GB"
print_info ""
print_info "Note: VM creation typically takes 1-3 minutes. Please wait..."

# Create a temporary file for output
TEMP_OUTPUT=$(mktemp)
TEMP_PID=$(mktemp)

# Run the command in background with output capture
(yc compute instance create \
    --name "$INSTANCE_NAME" \
    --folder-id "$YC_FOLDER_ID" \
    --zone "$YC_ZONE" \
    --network-interface "$NETWORK_PARAM" \
    --create-boot-disk image-folder-id="$IMAGE_FOLDER_ID",image-family="$IMAGE_FAMILY",size="$DISK_SIZE" \
    --cores "$CORES" \
    --memory "${MEMORY}GB" \
    --metadata-from-file user-data="$USER_DATA_FILE" \
    --format json > "$TEMP_OUTPUT" 2>&1; echo $? > "$TEMP_PID") &

BG_PID=$!

# Monitor with timeout (5 minutes = 300 seconds)
ELAPSED=0
MAX_WAIT=300
while [ $ELAPSED -lt $MAX_WAIT ]; do
    if ! kill -0 $BG_PID 2>/dev/null; then
        # Process finished
        break
    fi
    
    # Show progress every 10 seconds
    if [ $((ELAPSED % 10)) -eq 0 ] && [ $ELAPSED -gt 0 ]; then
        echo -n "."
    fi
    
    sleep 2
    ELAPSED=$((ELAPSED + 2))
done

# Check if still running
if kill -0 $BG_PID 2>/dev/null; then
    print_error "VM creation timed out after $MAX_WAIT seconds"
    kill $BG_PID 2>/dev/null || true
    wait $BG_PID 2>/dev/null || true
    rm -f "$TEMP_OUTPUT" "$TEMP_PID"
    exit 1
fi

# Get exit code
if [ -f "$TEMP_PID" ]; then
    CREATE_EXIT_CODE=$(cat "$TEMP_PID")
    rm -f "$TEMP_PID"
else
    CREATE_EXIT_CODE=1
fi

# Get output
INSTANCE_OUTPUT=$(cat "$TEMP_OUTPUT" 2>/dev/null || echo "")
rm -f "$TEMP_OUTPUT"

echo "" # New line after dots

CREATE_EXIT_CODE=$?

# Debug: show first few lines of output
if [ -n "$INSTANCE_OUTPUT" ]; then
    echo "$INSTANCE_OUTPUT" | head -5
fi

if [ $CREATE_EXIT_CODE -eq 124 ]; then
    print_error "VM creation timed out after 5 minutes"
    exit 1
elif [ $CREATE_EXIT_CODE -ne 0 ]; then
    print_error "Failed to create VM instance (exit code: $CREATE_EXIT_CODE)"
    echo "Full output:"
    echo "$INSTANCE_OUTPUT"
    exit 1
fi

# Check if output contains error
if echo "$INSTANCE_OUTPUT" | grep -qi "error\|failed\|denied"; then
    print_error "VM creation failed with error:"
    echo "$INSTANCE_OUTPUT"
    exit 1
fi

# Check if we got valid JSON output
if ! echo "$INSTANCE_OUTPUT" | python3 -c "import sys, json; json.load(sys.stdin)" 2>/dev/null; then
    print_warn "Output is not valid JSON, but continuing..."
    print_info "Output: $INSTANCE_OUTPUT"
fi

# Extract instance ID
INSTANCE_ID=$(echo "$INSTANCE_OUTPUT" | python3 -c "import sys, json; data=json.load(sys.stdin); print(data.get('id', ''))" 2>/dev/null || \
    echo "$INSTANCE_OUTPUT" | grep -o '"id": "[^"]*"' | head -1 | cut -d'"' -f4)

if [ -z "$INSTANCE_ID" ]; then
    print_error "Failed to extract instance ID"
    echo "$INSTANCE_OUTPUT"
    exit 1
fi

# Wait a bit for instance to initialize
print_info "Waiting for instance to initialize..."
sleep 15

# Get external IP address
print_info "Retrieving external IP address..."
EXTERNAL_IP=""
MAX_IP_ATTEMPTS=10
IP_ATTEMPT=0

while [ -z "$EXTERNAL_IP" ] && [ $IP_ATTEMPT -lt $MAX_IP_ATTEMPTS ]; do
    INSTANCE_INFO=$(yc compute instance get "$INSTANCE_NAME" --format json 2>/dev/null)
    if [ $? -eq 0 ]; then
        # Try to extract IP using Python if available, otherwise use grep
        EXTERNAL_IP=$(echo "$INSTANCE_INFO" | python3 -c "
import sys, json
try:
    data = json.load(sys.stdin)
    for interface in data.get('network_interfaces', []):
        if 'primary_v4_address' in interface:
            if 'one_to_one_nat' in interface['primary_v4_address']:
                print(interface['primary_v4_address']['one_to_one_nat']['address'])
                break
except:
    pass
" 2>/dev/null)
        
        # Fallback to grep if Python didn't work
        if [ -z "$EXTERNAL_IP" ]; then
            EXTERNAL_IP=$(echo "$INSTANCE_INFO" | grep -o '"address": "[^"]*"' | grep -v "0.0.0.0" | head -1 | cut -d'"' -f4)
        fi
    fi
    
    if [ -z "$EXTERNAL_IP" ]; then
        IP_ATTEMPT=$((IP_ATTEMPT + 1))
        if [ $IP_ATTEMPT -lt $MAX_IP_ATTEMPTS ]; then
            print_info "Waiting for IP assignment... (attempt $IP_ATTEMPT/$MAX_IP_ATTEMPTS)"
            sleep 5
        fi
    fi
done

if [ -z "$EXTERNAL_IP" ]; then
    print_error "Failed to get external IP address after $MAX_IP_ATTEMPTS attempts"
    print_info "Instance ID: $INSTANCE_ID"
    print_info "You can check the instance manually: yc compute instance get $INSTANCE_NAME"
    exit 1
fi

print_info "VM instance created successfully!"
print_info "Instance ID: $INSTANCE_ID"
print_info "External IP: $EXTERNAL_IP"

# Wait for SSH to be available
print_info "Waiting for SSH to be available..."
MAX_ATTEMPTS=30
ATTEMPT=0
while [ $ATTEMPT -lt $MAX_ATTEMPTS ]; do
    if ssh -i "$SSH_PRIVATE_KEY" \
        -o StrictHostKeyChecking=no \
        -o UserKnownHostsFile=/dev/null \
        -o ConnectTimeout=5 \
        -o BatchMode=yes \
        "${SSH_USER}@${EXTERNAL_IP}" \
        "echo 'SSH is ready'" &> /dev/null; then
        print_info "SSH is ready!"
        break
    fi
    ATTEMPT=$((ATTEMPT + 1))
    if [ $ATTEMPT -lt $MAX_ATTEMPTS ]; then
        print_info "Waiting for SSH... (attempt $ATTEMPT/$MAX_ATTEMPTS)"
        sleep 5
    else
        print_error "SSH is not available after $MAX_ATTEMPTS attempts"
        exit 1
    fi
done

# Install Docker on the remote server
print_info "Installing Docker on remote server..."

ssh -i "$SSH_PRIVATE_KEY" \
    -o StrictHostKeyChecking=no \
    -o UserKnownHostsFile=/dev/null \
    "${SSH_USER}@${EXTERNAL_IP}" << 'ENDSSH'
set -e

echo "Updating packages..."
sudo apt-get update -qq

echo "Installing prerequisites..."
sudo apt-get install -y -qq ca-certificates curl gnupg

echo "Adding Docker GPG key..."
sudo install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg | sudo gpg --dearmor -o /etc/apt/keyrings/docker.gpg
sudo chmod a+r /etc/apt/keyrings/docker.gpg

echo "Adding Docker repository..."
echo \
  "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu \
  $(. /etc/os-release && echo "$VERSION_CODENAME") stable" | \
  sudo tee /etc/apt/sources.list.d/docker.list > /dev/null

echo "Installing Docker..."
sudo apt-get update -qq
sudo apt-get install -y -qq docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin

echo "Adding user to docker group..."
sudo usermod -aG docker $USER

echo "Starting Docker service..."
sudo systemctl enable docker
sudo systemctl start docker

echo "Creating app directory..."
sudo mkdir -p /app
sudo chown $USER:$USER /app

echo "Docker installation complete!"
docker --version
docker compose version
ENDSSH

if [ $? -ne 0 ]; then
    print_error "Failed to install Docker on remote server"
    exit 1
fi

print_info "Docker installed successfully!"

# Copy project files to server
print_info "Copying project files to server..."

# Create a temporary directory for files to copy
TEMP_DIR=$(mktemp -d)
trap "rm -rf $TEMP_DIR" EXIT

# Copy necessary files
cp docker-compose.yaml "$TEMP_DIR/"
cp Dockerfile "$TEMP_DIR/"
cp go.mod "$TEMP_DIR/"
cp go.sum "$TEMP_DIR/"
cp main.go "$TEMP_DIR/"

# Create .env file (named without dot to ensure it gets copied)
cat > "$TEMP_DIR/env.txt" <<EOF
POSTGRES_DB=$POSTGRES_DB
POSTGRES_USER=$POSTGRES_USER
POSTGRES_PASSWORD=$POSTGRES_PASSWORD
EOF

# Copy files to server
scp -i "$SSH_PRIVATE_KEY" \
    -o StrictHostKeyChecking=no \
    -o UserKnownHostsFile=/dev/null \
    -r "$TEMP_DIR"/* \
    "${SSH_USER}@${EXTERNAL_IP}:/app/"

# Deploy application
print_info "Deploying application on remote server..."

ssh -i "$SSH_PRIVATE_KEY" \
    -o StrictHostKeyChecking=no \
    -o UserKnownHostsFile=/dev/null \
    "${SSH_USER}@${EXTERNAL_IP}" << ENDSSH
set -e
cd /app

# Rename env.txt to .env
mv env.txt .env 2>/dev/null || true

# Show env file contents for debugging
echo "Environment variables:"
cat .env

# Verify Docker is working
echo "Verifying Docker..."
sudo docker info > /dev/null 2>&1 || { echo "Docker not running"; exit 1; }

# Stop existing containers if any
sudo docker compose down 2>/dev/null || true

# Build and start containers with env file
echo "Building and starting containers..."
sudo docker compose --env-file .env up -d --build

# Wait for services to be ready
echo "Waiting for services to start..."
sleep 15

# Check if containers are running
echo "Container status:"
sudo docker compose --env-file .env ps

# Show app logs
echo "App logs:"
sudo docker compose --env-file .env logs --tail=20 app || true
ENDSSH

if [ $? -eq 0 ]; then
    print_info "Application deployed successfully!"
    print_info "Application is available at: http://${EXTERNAL_IP}:8080"
    echo "$EXTERNAL_IP"
else
    print_error "Failed to deploy application"
    exit 1
fi
