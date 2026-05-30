#!/bin/bash

# Exit immediately if a command exits with a non-zero status
set -e

# Define the application name
APP_NAME="inventory_server"
SRC_FILE="main.go"

# Array of target platforms in the format "GOOS/GOARCH"
TARGETS=(
    "darwin/arm64"
    "linux/arm64"
    "linux/arm"
)

echo "Starting the build process..."

# Iterate over the array
for TARGET in "${TARGETS[@]}"; do
    # Split the string by the forward slash
    IFS="/" read -r GOOS GOARCH <<< "$TARGET"
    
    # Determine the output binary name
    OUTPUT_NAME="dist/${GOOS}/${GOARCH}/${APP_NAME}"
    
    echo "Building for OS: ${GOOS} | Arch: ${GOARCH} -> ${OUTPUT_NAME}"
    
    # Run the Go build with the necessary environment variables
    # env ensures the variables are only set for this specific command execution
    env GOOS="$GOOS" GOARCH="$GOARCH" CGO_ENABLED=0 go build -o "${OUTPUT_NAME}" "$SRC_FILE"
done

echo "Builds completed successfully! Check the 'dist' directory."
