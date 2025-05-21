# Atlassian Large File Uploader


A command-line Go application for reliably uploading large files to Atlassian issues via their chunked upload API. It splits files into optimally sized chunks, uploads them in parallel with retry/backoff logic, and finalizes the upload—all while displaying a progress bar.

## Features
- Uploads large files to the Atlassian transfer portal.
- Handles chunked uploads for reliability and performance.
- Supports resuming interrupted uploads.
- Simple command-line interface.
- Written in Go (requires Go 1.24+).

## Prerequisites
- Access credentials for the Atlassian transfer portal (if authentication is required).
- Generate an authentication token at https://transfer.atlassian.com/auth_token

## Installation

### Prerequisites

- Go 1.24 or later
- A valid Atlassian username and API token

### Build from source

```bash
go build \
  -ldflags "-X main.defaultUser=YOUR_USERNAME -X main.defaultToken=YOUR_TOKEN" \
  -o atlassian-uploader \
  main.go
``` 

## Usage
Generate an authentication token at https://transfer.atlassian.com/auth_token
```shell
./atlassian-uploader [options] ATL-ISSUE-KEY /path/to/your/largefile.zip 
```

### Command-line Options
| Flag            | Description                                                     |
|-----------------|-----------------------------------------------------------------|
| `-user` string  | Atlassian username (overrides build-time default)               |
| `-token` string | API token (overrides build-time default)                        |
| `-url` string   | Base API URL (default `https://transfer.atlassian.com`)         |

example:
```shell
./atlassian-uploader \
  -user alice@example.com \
  -token XXXXXXXXXXXXX \
  -url https://transfer.atlassian.com \
  PROJ-456 large-video.mp4
```

## How It Works

### Chunking Strategy

- Calculates block size based on file size to target roughly 10,000 MB per chunk group.
- Ensures a minimum of 5 MB and maximum of 210 MB per chunk.
- 
### Concurrency & Backoff
- Spawns up to `maxSem = 8` goroutines to upload chunks in parallel.
- Uses `cenkalti/backoff` for exponential retry on probe and upload calls.
- Finalizes the upload after all chunks succeed.