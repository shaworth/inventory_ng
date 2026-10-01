#!/bin/bash

APP_NAME="inventory_server"
TARGETS=(
    "darwin:arm64::"
    "linux:arm64::"
    "linux:arm::"
    "android:arm64:blackview:./inventory_ng"   # Android: launched via tmux on the Blackview phone (see README)
)

function show_usage () {
cat << "EOS"
Usage: ./deploy.sh host os cpu 
EOS
}


function deploy_remote () {
for TARGET in "${TARGETS[@]}"; do
    # Split the string by the forward slash
    IFS=":" read -r GOOS GOARCH SSH_HOST PI_DEST_DIR<<< "$TARGET"
    if [ "$SSH_HOST" == "" ] ; then
        echo "❌ Error: Target host for $GOOS $GOARCH not provided, skipping"
    else
        echo "✅ Target host provided"
        push_remote
    fi
done
}

function push_remote () {

# --- SYSTEMD / PI UPDATE AUTOMATION WORKFLOW ---
echo "⚓ Deploying to ${SSH_HOST} over network..."

DEPLOY_SRC="dist/${GOOS}/${GOARCH}/${APP_NAME}"

if [ ! -f "$DEPLOY_SRC" ]; then
    echo "❌ Error: Target deploy file $DEPLOY_SRC not found."
fi

if [ "${GOOS}" == "linux" ]; then
    echo "🛸 Creating remote architecture directory at ${SSH_HOST}:${PI_DEST_DIR}"
    ssh "${SSH_HOST}" "mkdir -p ${PI_DEST_DIR}"

    echo "🛑 Safely halting active service runtime layer..."
    # Using '|| true' guarantees the script doesn't crash on a fresh install where the service doesn't exist yet
    ssh "${PI_USER}@${PI_HOST}" "sudo systemctl stop vessel-inventory 2>/dev/null || true"
fi

echo "🚚 Shipping optimization binary to target directory..."
scp "$DEPLOY_SRC" "${SSH_HOST}:${PI_DEST_DIR}/${APP_NAME}"

echo "🔑 Applying local workspace execution privileges..."
ssh "${SSH_HOST}" "chmod +x ${PI_DEST_DIR}/${APP_NAME}"


if [ "${GOOS}" == "linux" ]; then

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
fi

echo "🎉 Live deployment pipeline updated! Context state preserved successfully."

}

deploy_remote
