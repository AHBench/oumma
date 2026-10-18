#!/usr/bin/env bash
set -euo pipefail

# Usage: ./deploy.sh [--build|-b] [--help|-h]
#   --build | -b   : run `docker compose build` before `up`
#   --help  | -h   : show this help

BUILD=false

while [[ $# -gt 0 ]]; do
	case "$1" in
		-b|--build)
			BUILD=true
			shift
			;;
		-h|--help)
			echo "Usage: $0 [--build|-b] [--help|-h]"
			exit 0
			;;
		*)
			echo "Unknown option: $1"
			echo "Usage: $0 [--build|-b] [--help|-h]"
			exit 1
			;;
	esac
done

docker compose down --volumes

if [ "$BUILD" = true ]; then
	docker compose build
fi

docker compose up -d --force-recreate
