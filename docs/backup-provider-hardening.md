# Off-site backup provider hardening

This guide is the security half of [off-site recovery](recovery.md). The
`BackupTarget` tells Wildstone where to put encrypted restic objects; bucket
creation, provider retention and access policy remain deliberate operator
actions.

The recommended arrangement has two credentials:

- the **node key** is limited to one bucket and, where the provider permits it,
  one prefix. It can list, read and add objects but cannot delete objects,
  change retention, change bucket policy or create another key;
- an **offline maintenance key** is kept outside the node and is the only key
  that can delete expired object versions or run restic maintenance.

Set `appendOnly: true` on the target after that boundary and Object Lock are in
place:

```yaml
spec:
  type: s3
  endpoint: https://s3.us-east-1.amazonaws.com
  region: us-east-1
  bucket: wildstone-backups
  prefix: home
  appendOnly: true
  accessKeySecret: secret/offsite-s3-access-key
  secretKeySecret: secret/offsite-s3-secret-key
```

Wildstone then passes `--no-lock` to restic and skips off-site `forget --prune`.
The backup engine serializes runs for a plan, so it does not need restic's
temporary lock object. This is important: an append-only key cannot clean up
that object, and an old lock would eventually stop backups. `appendOnly` does
not configure or verify the provider. Enabling it before the policy and lock
are actually in place gives a false sense of safety.

## Provider setup

Use a dedicated bucket for backups when possible. If a bucket is shared, use
the same prefix in the provider policy and `BackupTarget`; include the trailing
slash in policy resources (`home/*`), while the Statefile value stays `home`.

### AWS S3

Create a versioned bucket with Object Lock enabled and a default **COMPLIANCE**
retention period. Governance mode is weaker because a principal with
`s3:BypassGovernanceRetention` can bypass it. Never grant the node key that
action, any `PutObjectRetention`/`PutObjectLegalHold` action, or bucket-policy,
IAM and key-management actions.

Attach a policy like this to the node principal, replacing the bucket and
prefix. It intentionally has no `s3:DeleteObject` or
`s3:DeleteObjectVersion`:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:ListBucket", "s3:GetBucketLocation", "s3:ListBucketMultipartUploads"],
      "Resource": "arn:aws:s3:::wildstone-backups",
      "Condition": {"StringLike": {"s3:prefix": ["home", "home/*"]}}
    },
    {
      "Effect": "Allow",
      "Action": ["s3:GetObject", "s3:PutObject", "s3:AbortMultipartUpload", "s3:ListMultipartUploadParts"],
      "Resource": "arn:aws:s3:::wildstone-backups/home/*"
    }
  ]
}
```

An unversioned delete against an S3 Object Lock bucket can create a delete
marker even though the protected version remains. Denying all delete actions
to the node key prevents that availability attack as well as permanent
deletion. See AWS's [Object Lock behavior and modes](https://docs.aws.amazon.com/AmazonS3/latest/userguide/object-lock.html)
and [governance bypass rules](https://docs.aws.amazon.com/AmazonS3/latest/userguide/object-lock-managing.html).

### Backblaze B2

Enable Object Lock and set **Default Bucket Retention** before the first
Wildstone upload. Create an application key restricted to the backup bucket
and `home/` name prefix with only `listBuckets`, `listFiles`, `readFiles` and
`writeFiles`. Do not include `deleteFiles`, `writeBucketRetentions`,
`writeFileRetentions`, `writeFileLegalHolds` or `bypassGovernance`.

B2 separates `writeFiles` from `deleteFiles`, so it can provide a genuinely
append-only stable node key. Its [capability reference](https://www.backblaze.com/docs/cloud-storage-application-key-capabilities)
defines those independently, and its [Object Lock guide](https://www.backblaze.com/docs/cloud-storage-object-lock)
explains default retention. Prefer compliance mode for a node-compromise
boundary.

### Wasabi

Create the bucket with Object Lock enabled, turn on default retention, and use
**Compliance** mode. Wasabi warns that Object Lock must be selected at bucket
creation and that governance mode can be bypassed by sufficiently privileged
users. Attach the AWS-style policy above to a dedicated sub-user, substituting
the Wasabi bucket ARN, and omit delete, governance-bypass, retention and IAM
administration actions.

Wasabi also offers its separate bucket Compliance feature. Do not turn on a
bucket setting that prevents restic from writing a new version of an existing
key; validate a complete backup and restore with the selected mode before
depending on it. See Wasabi's [immutability comparison](https://docs.wasabi.com/docs/immutability-compliance-and-object-locking)
and [Object Lock setup](https://docs.wasabi.com/docs/object-locking-enable).

### Cloudflare R2

Create an Object Read & Write token scoped to the one backup bucket. R2's
long-lived token preset includes object deletion, so Bucket Locks are the
immutability boundary: add a lock rule for `home/` before the first upload and
keep the administration token off the node. R2 also supports granular actions
for locally signed temporary credentials; if an external credential broker is
available, issue only `ListObjectsV2`, `GetObject`, `HeadObject`, `PutObject`
and the multipart-upload actions.

The [R2 token guide](https://developers.cloudflare.com/r2/api/tokens/) documents
bucket scoping, the [temporary-credential guide](https://developers.cloudflare.com/r2/api/s3/temporary-credentials/)
lists granular actions, and [Bucket Locks](https://developers.cloudflare.com/r2/buckets/bucket-locks/)
can protect one prefix for a duration or indefinitely.

### MinIO

Create a versioned, lock-enabled bucket and set default COMPLIANCE retention:

```sh
mc mb --with-lock store/wildstone-backups
mc retention set --default COMPLIANCE 30d store/wildstone-backups
```

Create a dedicated MinIO user and attach the same S3 policy shown for AWS.
Keep `s3:DeleteObject`, `s3:DeleteObjectVersion`,
`s3:BypassGovernanceRetention` and all administrative actions out of the node
policy. MinIO documents that [Object Lock requires versioning and protects
individual versions](https://docs.min.io/aistor/administration/object-locking-and-immutability/).
Run `scripts/object-lock-drill.sh` against a disposable local MinIO to prove
both halves: the node key has no delete authority and even the administrator
cannot remove a compliance-locked version before expiry.

## Retention and maintenance

`restic forget --prune` needs full read, write and delete access. Wildstone
therefore never runs it for an append-only target. On a separate secured host,
after the provider lock window has expired:

1. obtain the offline maintenance key;
2. verify the repository with `restic check`;
3. inspect the snapshot list and use a time-based policy such as
   `restic forget --keep-within 180d --prune`;
4. verify again, then revoke or put away the maintenance key.

Time-based `--keep-within` is intentional. A compromised node can add fake
snapshots with chosen timestamps; count-based `--keep-daily` or
`--keep-weekly` maintenance could then select the attacker's empty snapshots
and discard legitimate ones. Restic's [append-only security guidance](https://restic.readthedocs.io/en/latest/060_forget.html#security-considerations-in-append-only-mode)
describes that attack and the separate-client model.

Provider retention must be at least as long as the recovery window, but a long
lock also delays reclamation and can make a mistaken setting expensive. Start
with a disposable bucket, prove backup, restore and attempted deletion, then
create the production bucket.

## Rotate the node key

Rotation keeps the stable secret references in the Statefile:

1. create a second provider key with the same bucket/prefix policy;
2. set both values on the backup node:

   ```sh
   printf '%s' '<new-access-key-id>' | wild secret set offsite-s3-access-key -node node1
   printf '%s' '<new-secret-key>' | wild secret set offsite-s3-secret-key -node node1
   ```

3. run one plan with `wild backup run <plan>` and confirm its off-site row
   succeeds;
4. revoke the old provider key; run the plan once more to prove the old key is
   not cached.

Each backup pass is a fresh one-shot container. Wardstone resolves
`secret/<name>` when it creates that container, so the next run sees the new
secret version without a Statefile change or service restart. The
`TestSecretOpsAndMaterialization` test pins this exact set-then-next-container
behavior.

## Cost comparison (2026-09-29)

These are storage-only monthly estimates for a steady full repository, using
decimal 1 TB = 1,000 GB. They exclude taxes, discounts and restic's
deduplication. Actual stored size includes pack/index overhead and retained
history.

| Stored | AWS S3 Standard | Backblaze B2 | Wasabi Pay-Go | Cloudflare R2 Standard | MinIO |
|---:|---:|---:|---:|---:|---:|
| 100 GB | $2.30 | $0.63 | $7.99 minimum | $1.35 | infrastructure cost |
| 1 TB | $23.00 | $6.88 | $7.99 | $14.85 | infrastructure cost |
| 4 TB | $92.00 | $27.73 | $31.21 | $59.85 | infrastructure cost |
| 10 TB | $230.00 | $69.43 | $78.03 | $149.85 | infrastructure cost |

Assumptions and variable charges:

- **AWS S3 Standard:** $0.023/GB-month in US East for this range; PUT/LIST
  $0.005/1,000, GET $0.0004/1,000; internet restore egress has a 100 GB
  account-wide free allowance, then regional tiered rates. Standard has no
  minimum storage duration. Check [S3 pricing](https://aws.amazon.com/s3/pricing/).
- **Backblaze B2:** $6.95/TB-month, first 10 GB free, ordinary transactions
  free, and egress up to three times average monthly storage free; overage is
  $0.01/GB. No minimum duration. Check [B2 pricing](https://www.backblaze.com/cloud-storage/pricing).
- **Wasabi Pay-Go:** $7.99/TiB-month in the cited US/Europe example, a 1 TiB
  monthly account minimum, and a 90-day minimum duration (early-deleted bytes
  remain billable). Egress and ordinary API requests are included subject to
  Wasabi's policy. The 4/10 TB rows use 4,000/10,000 GB divided by 1,024.
  Check the [monthly minimum](https://docs.wasabi.com/v1/docs/how-does-wasabis-monthly-minimum-storage-charge-work)
  and [pricing FAQ](https://wasabi.com/pricing/faq).
- **Cloudflare R2 Standard:** $0.015/GB-month after 10 GB free; the table
  subtracts that free tier. It includes 1 million Class A and 10 million Class
  B operations monthly; excess costs $4.50/million and $0.36/million. Internet
  egress is free and Standard has no minimum duration. Check [R2 pricing](https://developers.cloudflare.com/r2/pricing/).
- **MinIO:** software does not establish a portable storage price. Count disks,
  replicas/erasure overhead, a second site, power, hardware replacement,
  monitoring and operator time. Egress and retention minimums are those of the
  infrastructure beneath it.

Object Lock itself is not a separate line item on these plans, but locked and
non-current versions continue consuming billed storage until eligible for
deletion.
