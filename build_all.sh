#!/bin/bash

# Exit immediately if a command exits with a non-zero status
set -e

# --- CONFIGURATION ---
APP_NAME="inventory_server"
SRC_FILE="main.go"
DIST_DIR="dist"

# Target deployment node details (Modify as needed)
PI_HOST="raspberrypi.local"
PI_USER="pi"
PI_DEST_DIR="/home/pi/vessel-inventory"

# Array of target platforms in the format "GOOS/GOARCH"
TARGETS=(
    "darwin/arm64"
    "linux/arm64"
    "linux/arm"
)

# --- FLAG PARSING ---
DEPLOY=false
while getopts "d" opt; do
    case ${opt} in
        d ) DEPLOY=true ;;
        \? ) echo "Usage: $0 [-d (deploy after build)]"; exit 1 ;;
    esac
done

echo "🧹 Cleaning previous build artifacts..."
rm -rf "$DIST_DIR"
mkdir -p "$DIST_DIR"

echo "🚀 Starting the optimization build process..."

# Iterate over the array
for TARGET in "${TARGETS[@]}"; do
    # Split the string by the forward slash
    IFS="/" read -r GOOS GOARCH <<< "$TARGET"
    
    # Determine the output structure
    TARGET_DIR="${DIST_DIR}/${GOOS}/${GOARCH}"
    mkdir -p "$TARGET_DIR"
    OUTPUT_NAME="${TARGET_DIR}/${APP_NAME}"
    
    echo "📦 Building for OS: ${GOOS} | Arch: ${GOARCH} -> ${OUTPUT_NAME}"
    
    # -s: Omit symbols table (smaller size)
    # -w: Omit DWARF debugging info
    # CGO_ENABLED=0 ensures absolute portability without shared C library dependency issues
    env GOOS="$GOOS" GOARCH="$GOARCH" CGO_ENABLED=0 go build \
        -ldflags="-s -w" \
        -o "${OUTPUT_NAME}" \
        "$SRC_FILE"
done

echo "✨ Builds completed successfully! Check the '${DIST_DIR}' directory."
echo "---"

# --- SYSTEMD / PI UPDATE AUTOMATION WORKFLOW ---
if [ "$DEPLOY" = true ]; then
    echo "⚓ Deploying to Vessel Node over network..."
    
    # Determine which binary to push (Defaulting to Linux ARM64 for Pi 3/4/5)
    # If using a 32-bit Pi base image, swap path to: dist/linux/arm/inventory_server
    DEPLOY_SRC="dist/linux/arm64/${APP_NAME}"
    
    if [ ! -f "$DEPLOY_SRC" ]; then
        echo "❌ Error: Target deploy file $DEPLOY_SRC not found."
        exit 1
    fi

    echo "🛸 Creating remote architecture directory at ${PI_USER}@${PI_HOST}:${PI_DEST_DIR}..."
    ssh "${PI_USER}@${PI_HOST}" "mkdir -p ${PI_DEST_DIR}"

    echo "🛑 Safely halting active service runtime layer..."
    # Using '|| true' guarantees the script doesn't crash on a fresh install where the service doesn't exist yet
    ssh "${PI_USER}@${PI_HOST}" "sudo systemctl stop vessel-inventory 2>/dev/null || true"

    echo "🚚 Shipping optimization binary to target directory..."
    scp "$DEPLOY_SRC" "${PI_USER}@${PI_HOST}:${PI_DEST_DIR}/${APP_NAME}"

    echo "🔑 Applying local workspace execution privileges..."
    ssh "${PI_USER}@${PI_HOST}" "chmod +x ${PI_DEST_DIR}/${APP_NAME}"

    # Check if the service needs a registration run
    echo "🛠 Checking service integration tracking..."
    ssh "${PI_USER}@${PI_HOST}" "
        if [ ! -f /etc/systemd/system/vessel-inventory.service ]; then
            echo '⚠️ Service not registered yet. Initiating inline deployment registration flag...'
            sudo ${PI_DEST_DIR}/${APP_NAME} --install
        else
            echo '🔄 Service already registered. Restarting node runtime engine...'
            sudo systemctl start vessel-inventory
        fi
    "

    echo "🎉 Live deployment pipeline updated! Context state preserved successfully."
fi