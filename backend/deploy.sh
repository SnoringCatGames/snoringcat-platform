#!/bin/bash
# Deployment script for Hop 'n Bop backend

set -e

echo "🚀 Deploying Hop 'n Bop backend..."

# Check for samconfig.toml
if [ ! -f "samconfig.toml" ]; then
    echo "❌ samconfig.toml not found"
    echo "📝 Copy samconfig.toml.example to samconfig.toml and fill in your values"
    exit 1
fi

# Check for AWS CLI
if ! command -v aws &> /dev/null; then
    echo "❌ AWS CLI not found. Please install it first."
    exit 1
fi

# Check for SAM CLI
if ! command -v sam &> /dev/null; then
    echo "❌ SAM CLI not found. Please install it first."
    exit 1
fi

# Install dependencies
echo "📦 Installing dependencies..."
pip install -r requirements.txt -q

# Build
echo "🔨 Building SAM application..."
sam build --use-container

# Deploy
echo "🚀 Deploying to AWS..."
sam deploy --no-confirm-changeset

# Get outputs
echo ""
echo "✅ Deployment complete!"
echo ""
echo "📋 Outputs:"
sam list stack-outputs --stack-name hopnbop-backend --output table

echo ""
echo "🎮 Update your Godot settings with the ApiEndpoint URL above"
