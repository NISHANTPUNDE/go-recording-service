# Go Recording Service - Local Development

## Prerequisites

1. **Install Go** (if not installed):
   - Download from https://go.dev/dl/
   - Or on Windows: `winget install GoLang.Go`

2. **Install FFmpeg** (for WAV conversion):
   - Download from https://ffmpeg.org/download.html
   - Or on Windows: `winget install FFmpeg`

## Run Locally (Windows/Linux/Mac)

```bash
cd e:\flutter_app\go-recording-service

# Download dependencies
go mod tidy

# Build
go build -o recording-service.exe .

# Run
./recording-service.exe
# OR
go run main.go
```

Server starts on **port 8081** by default.
Override with: `PORT=9000 go run main.go`

## Test Health Endpoint

```bash
curl http://localhost:8081/health
# Should return: OK
```

## Recordings

Recordings are saved to `./recordings/` folder as `.ogg` files,
then converted to `.wav` using FFmpeg.
