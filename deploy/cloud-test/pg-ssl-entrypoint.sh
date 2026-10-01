#!/bin/sh
# PostgreSQL with TLS on and required for every network connection, like
# AlloyDB's enforced encryption. The certificate comes from make-cert.sh
# (mounted at /tls) and is copied so that postgres owns its key.
set -e
CERT_DIR=/var/lib/postgresql/tls
mkdir -p "$CERT_DIR"
cp /tls/server.crt /tls/server.key "$CERT_DIR/"
chown -R postgres:postgres "$CERT_DIR"
chmod 600 "$CERT_DIR/server.key"
mkdir -p /docker-entrypoint-initdb.d
cat > /docker-entrypoint-initdb.d/zz-hostssl.sh <<'EOF'
#!/bin/sh
# only TLS connections from the network (the local socket stays for the entrypoint)
sed -i 's/^host all all all scram-sha-256$/hostssl all all all scram-sha-256/' "$PGDATA/pg_hba.conf"
echo "hostnossl all all 0.0.0.0/0 reject" >> "$PGDATA/pg_hba.conf"
EOF
exec docker-entrypoint.sh postgres -c ssl=on -c ssl_cert_file="$CERT_DIR/server.crt" -c ssl_key_file="$CERT_DIR/server.key"
