#!/bin/sh
# A throwaway self-signed certificate for the test database's TLS.
set -e
cd "$(dirname "$0")"
mkdir -p tls
openssl req -x509 -newkey rsa:2048 -nodes -days 365 -subj "/CN=db" -keyout tls/server.key -out tls/server.crt 2>/dev/null
echo "tls/server.crt and tls/server.key written"
