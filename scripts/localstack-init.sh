#!/usr/bin/env bash
# Crea el bucket S3 y las colas SQS (jobs + DLQ con RedrivePolicy).
# Idempotente. El compose lo corre dentro de localstack-init.
# Desde el host: AWS_ENDPOINT_URL=http://localhost:4566 ./scripts/localstack-init.sh
#
# Hace falta el AWS CLI (la imagen de LocalStack lo trae como `aws` / `awslocal`).

set -eu

EP="${AWS_ENDPOINT_URL:-${S3_ENDPOINT:-http://localstack:4566}}"
export AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID:-test}"
export AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY:-test}"
export AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-${AWS_REGION:-us-east-1}}"
export AWS_PAGER=""

BUCKET="${S3_BUCKET:-${MINIO_BUCKET:-nimbus-artifacts}}"
JOBS="${SQS_QUEUE_NAME:-nimbus-async-jobs}"
DLQ="${SQS_DLQ_NAME:-nimbus-async-dlq}"
MAX="${NIMBUS_ASYNC_MAX_ATTEMPTS:-3}"

if ! command -v aws >/dev/null 2>&1; then
  if command -v awslocal >/dev/null 2>&1; then
    aws() { awslocal "$@"; }
  else
    echo "hace falta el AWS CLI (aws o awslocal)" >&2
    exit 1
  fi
fi

awscli() {
  aws --endpoint-url "$EP" "$@"
}

i=0
until awscli s3 ls >/dev/null 2>&1; do
  i=$((i + 1))
  if [ "$i" -ge 60 ]; then
    echo "LocalStack no respondió en $EP" >&2
    exit 1
  fi
  sleep 1
done

if ! awscli s3api head-bucket --bucket "$BUCKET" >/dev/null 2>&1; then
  awscli s3 mb "s3://${BUCKET}"
fi

awscli sqs create-queue --queue-name "$DLQ" >/dev/null
DLQ_URL="$(awscli sqs get-queue-url --queue-name "$DLQ" --query QueueUrl --output text)"
DLQ_ARN="$(awscli sqs get-queue-attributes --queue-url "$DLQ_URL" --attribute-names QueueArn --query 'Attributes.QueueArn' --output text)"

awscli sqs create-queue --queue-name "$JOBS" >/dev/null
JOBS_URL="$(awscli sqs get-queue-url --queue-name "$JOBS" --query QueueUrl --output text)"

POLICY="$(printf '{"deadLetterTargetArn":"%s","maxReceiveCount":"%s"}' "$DLQ_ARN" "$MAX")"
ESCAPED="$(printf '%s' "$POLICY" | sed 's/"/\\"/g')"
printf '{"VisibilityTimeout":"30","RedrivePolicy":"%s"}\n' "$ESCAPED" >/tmp/nimbus-sqs-attrs.json
awscli sqs set-queue-attributes --queue-url "$JOBS_URL" --attributes file:///tmp/nimbus-sqs-attrs.json

echo "bucket=${BUCKET} jobs=${JOBS_URL} dlq=${DLQ_URL} maxReceiveCount=${MAX}"
awscli sqs get-queue-attributes --queue-url "$JOBS_URL" --attribute-names RedrivePolicy QueueArn --output json
