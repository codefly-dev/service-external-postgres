## Build multi-platform images

# Set up buildx if you haven't already
```bash
docker buildx create --use
```

# Build and push the multi-arch image

```bash 
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  -t codeflydev/alembic:latest \
  -f migrations/Dockerfile.alembic \
  --push \
  migrations/
```