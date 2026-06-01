#!/bin/bash

# A script specifically built to test Nuxt Server-Sent Events (SSE)

echo "🚀 Initializing remote connection..."
sleep 1

echo "📦 Fetching user data for: $1..."
sleep 2

echo "⚙️  Generating configuration files..."
sleep 1

# Simulate a fast process with a loop
for i in {1..5}; do
  echo "   -> Processing chunk $i/5..."
  sleep 0.5
done

echo "🔍 Verifying integrity..."
sleep 2

echo "✅ Task completed successfully!"
