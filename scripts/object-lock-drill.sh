#!/usr/bin/env bash
# Prove the #423 immutability boundary against a disposable local MinIO.
set -euo pipefail

server_image="${MINIO_IMAGE:-minio/minio:RELEASE.2025-04-22T22-12-26Z}"
client_image="${MINIO_MC_IMAGE:-minio/mc:RELEASE.2025-04-16T18-13-26Z}"
container="wildstone-object-lock-drill-$$"
network="wildstone-object-lock-drill-$$"
root_user="drill-root"
root_password="drill-root-$RANDOM-$RANDOM"
node_user="backup-node"
node_password="backup-node-$RANDOM-$RANDOM"
bucket="wildstone-backups"

cleanup() {
	docker rm -f "$container" >/dev/null 2>&1 || true
	docker network rm "$network" >/dev/null 2>&1 || true
}
trap cleanup EXIT

docker network create "$network" >/dev/null
docker run -d --name "$container" --network "$network" \
	-e "MINIO_ROOT_USER=$root_user" -e "MINIO_ROOT_PASSWORD=$root_password" \
	"$server_image" server /data >/dev/null

mc() {
	docker run --rm --network "$network" --entrypoint /bin/sh "$client_image" -ec "
    mc alias set store http://$container:9000 '$root_user' '$root_password' >/dev/null
    $*
  "
}

for ((attempt = 0; attempt < 30; attempt++)); do
	if mc "mc ready store >/dev/null" 2>/dev/null; then
		break
	fi
	sleep 1
done
mc "mc ready store >/dev/null"
mc "mc mb --with-lock store/$bucket >/dev/null"
mc "mc retention set --default COMPLIANCE 1d store/$bucket >/dev/null"

policy_dir="$(mktemp -d)"
policy_file="$policy_dir/node-policy.json"
trap 'rm -rf "$policy_dir"; cleanup' EXIT
cat >"$policy_file" <<'JSON'
{
  "Version": "2012-10-17",
  "Statement": [
    {"Effect":"Allow","Action":["s3:ListBucket","s3:GetBucketLocation","s3:ListBucketMultipartUploads"],"Resource":["arn:aws:s3:::wildstone-backups"],"Condition":{"StringLike":{"s3:prefix":["home","home/*"]}}},
    {"Effect":"Allow","Action":["s3:GetObject","s3:PutObject","s3:AbortMultipartUpload","s3:ListMultipartUploadParts"],"Resource":["arn:aws:s3:::wildstone-backups/home/*"]}
  ]
}
JSON

docker run --rm --network "$network" --entrypoint /bin/sh \
	-v "$policy_file:/tmp/node-policy.json:ro" "$client_image" -ec "
    mc alias set store http://$container:9000 '$root_user' '$root_password' >/dev/null
    mc admin user add store '$node_user' '$node_password' >/dev/null
    mc admin policy create store backup-node /tmp/node-policy.json >/dev/null
    mc admin policy attach store backup-node --user '$node_user' >/dev/null
  "

docker run --rm --network "$network" --entrypoint /bin/sh "$client_image" -ec "
  mc alias set node http://$container:9000 '$node_user' '$node_password' >/dev/null
  printf 'encrypted-restic-pack\n' | mc pipe node/$bucket/home/probe >/dev/null
  mc stat node/$bucket/home/probe >/dev/null
  if mc rm --force node/$bucket/home/probe >/dev/null 2>&1; then
    echo 'FAIL: node key deleted the probe' >&2
    exit 1
  fi
"

# The root user has delete authority, so this second refusal proves that the
# stored version itself is protected by COMPLIANCE retention, not only IAM.
if mc "mc rm --versions --force store/$bucket/home/probe >/dev/null 2>&1"; then
	echo "FAIL: administrator deleted a compliance-locked version" >&2
	exit 1
fi

echo "PASS: node deletion was denied and the locked version survived an administrator delete"
