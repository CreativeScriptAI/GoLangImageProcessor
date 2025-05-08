# Image Conversion Service
A high-performance Go service for converting images to WebP format with support for batch processing, rate limiting, and automatic resizing.

## Prerequisites

- Go 1.23 or higher
- Docker and Docker Compose (for containerized deployment)
- CGO enabled (required for image processing)

## Quick Start

### Using Docker

1. Clone the repository:
   ```bash
   git clone <repository-url>
   cd image-converter
   ```

2. Build and run using Docker Compose:
   ```bash
   docker-compose up -d
   ```

The service will be available at `http://localhost:8080`

### Manual Installation

1. Install dependencies:
   ```bash
   go mod download
   ```

2. Build the application:
   ```bash
   CGO_ENABLED=1 go build -o image-client
   ```

3. Run the service:
   ```bash
   ./image-client
   ```

## API Endpoints

### Single Image Conversion
```
POST /convert
Content-Type: multipart/form-data

Parameters:
- image: Image file (required)
- quality: Quality (0-100, default: 80)
- lossless: true/false (default: false)
```

### Batch Image Conversion
```
POST /batch-convert
Content-Type: multipart/form-data

Parameters:
- images: Multiple image files (required)
- quality: Quality (0-100, default: 80)
- lossless: true/false (default: false)
```

### Health Check
```
GET /
```

## Configuration

The service can be configured using environment variables:

| Variable | Description | Default |
|----------|-------------|---------|
| MAX_UPLOAD_SIZE | Maximum upload size in bytes | 33554432 (32MB) |
| DEFAULT_QUALITY | Default WebP quality | 80 |
| MAX_WIDTH | Maximum image width | 2500 |
| RATE_LIMIT | Rate limit (requests per minute) | 100-M |
| SERVER_TIMEOUT | Server timeout | 30s |
| PORT | Server port | 8080 |


