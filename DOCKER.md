# Docker Setup for wotrlay

This directory contains Docker configuration for running the wotrlay Nostr relay.

## GitHub Container Registry

The Docker image is automatically built and published to GitHub Container Registry:

- **Latest image**: `ghcr.io/contextvm/wotrlay:latest`
- **Tagged releases**: `ghcr.io/contextvm/wotrlay:v1.2.3`

Images are automatically built on:
- Pushes to `master` branch (updates `latest` tag)
- Tagged releases (creates version tags like `v1.2.3`, `v1.2`, `v1`)

## Quick Start

### Using Docker Compose (Recommended)

1. **Build and run:**
   ```bash
   docker-compose up -d
   ```

2. **View logs:**
   ```bash
   docker-compose logs -f
   ```

3. **Stop the service:**
   ```bash
   docker-compose down
   ```

### Using Docker directly

1. **Pull from GitHub Container Registry (recommended):**
   ```bash
   docker pull ghcr.io/contextvm/wotrlay:latest
   ```

2. **Or build locally:**
   ```bash
   docker build -t wotrlay .
   ```

3. **Run the container:**
   ```bash
   docker run -d \
     --name wotrlay \
     -p 3334:3334 \
     -v wotrlay_data:/app/badger \
     -e MID_THRESHOLD=0.5 \
     -e RELATR_SECRET_KEY="your-secret-key" \
     ghcr.io/contextvm/wotrlay:latest
   ```

3. **View logs:**
   ```bash
   docker logs -f wotrlay
   ```

4. **Stop the container:**
   ```bash
   docker stop wotrlay
   docker rm wotrlay
   ```

## Configuration

### Environment Variables

You can configure the relay using environment variables in the `docker-compose.yml` file or passed directly to `docker run`:

- `MID_THRESHOLD` (default: 0.5) - Trust score threshold
- `HIGH_THRESHOLD` (optional) - High trust threshold for backfill
- `URL_POLICY_ENABLED` (optional) - Enable URL restrictions
- `RANK_QUEUE_IP_DAILY_LIMIT` (default: 250) - Rank refresh rate limit
- `RELATR_RELAY` (default: wss://relay.contextvm.org) - ContextVM relay URL
- `RELATR_PUBKEY` (default: 750682303c9f0ddad75941b49edc9d46e3ed306b9ee3335338a21a3e404c5fa3) - Relatr pubkey
- `RELATR_SECRET_KEY` (optional) - Secret key for signing requests
- `DEBUG` (optional) - Enable debug logging
- `RELAY_NAME`, `RELAY_DESCRIPTION`, `RELAY_PUBKEY`, `RELAY_CONTACT` - NIP-11 relay info

### Data Persistence

The Badger database is stored in a Docker volume named `wotrlay_data` at `/app/badger` inside the container. This ensures data persists across container restarts.

To backup the data:
```bash
docker run --rm -v wotrlay_data:/app/badger -v $(pwd):/backup alpine tar czf /backup/wotrlay_data_backup.tar.gz /app/badger
```

To restore from backup:
```bash
docker run --rm -v wotrlay_data:/app/badger -v $(pwd):/backup alpine tar xzf /backup/wotrlay_data_backup.tar.gz -C /
```

## Accessing the Relay

Once running, the relay will be accessible at:
- **WebSocket:** `ws://localhost:3334`
- **NIP-11 Info:** `http://localhost:3334` (returns relay information document)

## Troubleshooting

### View container logs
```bash
docker-compose logs -f wotrlay
```

### Shell access to container
```bash
docker-compose exec wotrlay sh
```

### Rebuild after code changes
```bash
docker-compose build --no-cache
docker-compose up -d
```

## Security Notes

- The container runs as a non-root user (UID 1001)
- Only port 3334 is exposed
- Data is persisted in a Docker volume
- Sensitive keys should be provided via environment variables