#!/usr/bin/env bash
# Comprueba S3 y SQS reales de LocalStack (no un fake en proceso).
#
# 1. El zip desplegado está en el bucket nimbus-artifacts.
# 2. nimbus-async-jobs tiene RedrivePolicy hacia nimbus-async-dlq.
# 3. Un async lento aparece en vuelo en SQS y termina SUCCESS, fuera de la DLQ.
# 4. Un handler que siempre falla llega a la DLQ nativa. GET /dlq muestra
#    payload, lastError, attempts y durationMs. El redrive lo saca y,
#    como sigue fallando, vuelve a la misma cola.
#
# Requiere el stack arriba (make up). No borra el mensaje de la DLQ antes
# del POST /dlq/:id/redrive.

set -euo pipefail

API_URL="${API_URL:-http://localhost:3000}"
COMPUTE_URL="${COMPUTE_URL:-http://localhost:8080}"
LS_CONTAINER="${LS_CONTAINER:-nimbus-localstack}"
BUCKET="${S3_BUCKET:-nimbus-artifacts}"
JOBS_NAME="${SQS_QUEUE_NAME:-nimbus-async-jobs}"
DLQ_NAME="${SQS_DLQ_NAME:-nimbus-async-dlq}"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info() { echo -e "${YELLOW}[localstack-check]${NC} $1"; }
log_ok() { echo -e "${GREEN}[✓]${NC} $1"; }
log_fail() { echo -e "${RED}[✗]${NC} $1" >&2; exit 1; }

command -v curl >/dev/null 2>&1 || log_fail "curl requerido"
command -v jq >/dev/null 2>&1 || log_fail "jq requerido"
command -v zip >/dev/null 2>&1 || log_fail "zip requerido"
command -v docker >/dev/null 2>&1 || log_fail "docker requerido"

if ! curl -sf "$API_URL/health" >/dev/null 2>&1; then
  log_fail "El stack no está corriendo. Ejecutá: make up"
fi
if ! docker inspect "$LS_CONTAINER" >/dev/null 2>&1; then
  log_fail "No está el contenedor $LS_CONTAINER"
fi

awslocal() {
  docker exec -e AWS_PAGER="" "$LS_CONTAINER" awslocal "$@"
}

# Una sola sesión dentro del contenedor. awslocal arranca en ~2s por
# llamada y un long poll no vuelve antes de tiempo: así se recorre la
# DLQ en milisegundos y se distingue "sigue el mensaje" de "ya volvió".
dlq_find() {
  local id="$1" wait_s="$2" rounds="$3"
  docker exec -i "$LS_CONTAINER" python3 - "$DLQ_URL" "$id" "$wait_s" "$rounds" <<'PY'
import sys
import botocore.session

url, want, wait_s, rounds = sys.argv[1], sys.argv[2], int(sys.argv[3]), int(sys.argv[4])
client = botocore.session.get_session().create_client(
    "sqs",
    endpoint_url="http://localhost:4566",
    region_name="us-east-1",
    aws_access_key_id="test",
    aws_secret_access_key="test",
)
for _ in range(rounds):
    out = client.receive_message(
        QueueUrl=url,
        MaxNumberOfMessages=10,
        VisibilityTimeout=20,
        WaitTimeSeconds=wait_s,
    )
    messages = out.get("Messages") or []
    hit = None
    for message in messages:
        if want in (message.get("Body") or ""):
            hit = message
            break
    if hit is not None:
        client.change_message_visibility(
            QueueUrl=url,
            ReceiptHandle=hit["ReceiptHandle"],
            VisibilityTimeout=0,
        )
        sys.stdout.write(hit["Body"])
        raise SystemExit(0)
    if not messages and wait_s == 0:
        break
PY
}

JOBS_URL="$(awslocal sqs get-queue-url --queue-name "$JOBS_NAME" --query QueueUrl --output text)"
DLQ_URL="$(awslocal sqs get-queue-url --queue-name "$DLQ_NAME" --query QueueUrl --output text)"
log_info "jobs=$JOBS_URL"
log_info "dlq=$DLQ_URL"

POLICY="$(awslocal sqs get-queue-attributes --queue-url "$JOBS_URL" --attribute-names RedrivePolicy --query 'Attributes.RedrivePolicy' --output text)"
echo "  RedrivePolicy=$POLICY"
MAX="$(echo "$POLICY" | jq -r '.maxReceiveCount')"
ARN="$(echo "$POLICY" | jq -r '.deadLetterTargetArn')"
if [[ "$MAX" == "3" && "$ARN" == *"$DLQ_NAME"* ]]; then
  log_ok "RedrivePolicy maxReceiveCount=3 hacia $DLQ_NAME"
else
  log_fail "RedrivePolicy inesperada: $POLICY"
fi

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

cat >"$WORKDIR/index.js" <<'EOF'
exports.handler = async (event) => {
  if (event && event.mode === "fail") {
    throw new Error("localstack-check boom");
  }
  const ms = (event && event.sleepMs) || 0;
  if (ms > 0) {
    await new Promise((resolve) => setTimeout(resolve, ms));
  }
  return { ok: true, slept: ms };
};
EOF
(cd "$WORKDIR" && zip -q fn.zip index.js)

STAMP="$(date +%s)"
CREATE="$(curl -sf -X POST "$API_URL/functions" -H 'Content-Type: application/json' -d "{\"name\":\"ls-check-$STAMP\"}")"
FN="$(echo "$CREATE" | jq -r .id)"
DEPLOY="$(curl -sf -X POST "$API_URL/functions/$FN/deploy" -F "file=@$WORKDIR/fn.zip")"
VER="$(echo "$DEPLOY" | jq -r .version.version)"
log_info "función $FN versión $VER"

KEYS="$(awslocal s3 ls "s3://${BUCKET}/functions/${FN}/versions/${VER}/" || true)"
echo "  s3 ls: ${KEYS:-vacío}"
if echo "$KEYS" | grep -q 'code.zip'; then
  log_ok "el zip está en s3://${BUCKET}/functions/${FN}/versions/${VER}/code.zip"
else
  log_fail "no aparece code.zip en el bucket $BUCKET"
fi

attr() {
  awslocal sqs get-queue-attributes --queue-url "$1" --attribute-names All --output json
}

log_info "async lento: tiene que verse en vuelo en SQS y terminar SUCCESS"
SLOW="$(curl -sf -X POST "$API_URL/functions/$FN/invoke?async=true" -H 'Content-Type: application/json' -d '{"event":{"sleepMs":3000}}')"
SLOW_ID="$(echo "$SLOW" | jq -r .invocationId)"
echo "  202 $SLOW"
INFLIGHT=0
for _ in $(seq 1 15); do
  NOTVIS="$(attr "$JOBS_URL" | jq -r '.Attributes.ApproximateNumberOfMessagesNotVisible // "0"')"
  VIS="$(attr "$JOBS_URL" | jq -r '.Attributes.ApproximateNumberOfMessages // "0"')"
  if [[ "$NOTVIS" != "0" || "$VIS" != "0" ]]; then
    INFLIGHT=1
    echo "  en vuelo visible=$VIS noVisible=$NOTVIS"
    break
  fi
  sleep 0.2
done
if [[ "$INFLIGHT" == "1" ]]; then
  log_ok "el job pasó por SQS ($JOBS_NAME)"
else
  log_fail "no se vio el mensaje en $JOBS_NAME durante el sleep"
fi

SLOW_FINAL=""
for _ in $(seq 1 40); do
  SLOW_FINAL="$(curl -sf "$API_URL/invocations/$SLOW_ID")"
  ST="$(echo "$SLOW_FINAL" | jq -r .status)"
  if [[ "$ST" == "SUCCESS" || "$ST" == "ERROR" || "$ST" == "TIMEOUT" ]]; then
    break
  fi
  sleep 0.5
done
echo "  slow $(echo "$SLOW_FINAL" | jq -c '{status,attempts,output}')"
SLOW_DLQ="$(curl -sf "$COMPUTE_URL/dlq" | jq -r --arg id "$SLOW_ID" '[.entries[] | select(.invocationId==$id)] | length')"
if [[ "$(echo "$SLOW_FINAL" | jq -r .status)" == "SUCCESS" && "$SLOW_DLQ" == "0" ]]; then
  log_ok "el async lento terminó SUCCESS y no está en la DLQ"
else
  log_fail "async lento: $SLOW_FINAL dlq=$SLOW_DLQ"
fi

log_info "handler que falla: DLQ nativa, listado y redrive"
FAIL="$(curl -sf -X POST "$API_URL/functions/$FN/invoke?async=true" -H 'Content-Type: application/json' -d '{"event":{"mode":"fail"}}')"
FAIL_ID="$(echo "$FAIL" | jq -r .invocationId)"
echo "  202 $FAIL"
FAIL_FINAL=""
for _ in $(seq 1 40); do
  FAIL_FINAL="$(curl -sf "$API_URL/invocations/$FAIL_ID")"
  ST="$(echo "$FAIL_FINAL" | jq -r .status)"
  if [[ "$ST" == "ERROR" || "$ST" == "SUCCESS" || "$ST" == "TIMEOUT" ]]; then
    break
  fi
  sleep 0.5
done
echo "  fail $(echo "$FAIL_FINAL" | jq -c '{status,attempts,error}')"

# La DLQ puede tener mensajes de checks anteriores. El helper los deja
# ocultos 20s y al mensaje buscado le devuelve la visibilidad a 0.
FOUND_BODY="$(dlq_find "$FAIL_ID" 1 12)"
if [[ -n "$FOUND_BODY" ]]; then
  echo "  cuerpo SQS DLQ: $(echo "$FOUND_BODY" | jq -c '{invocationId,attempt,functionId: .body.functionId}' 2>/dev/null || echo "$FOUND_BODY")"
  log_ok "el fallo está en la cola SQS $DLQ_NAME"
else
  log_fail "el mensaje no apareció en $DLQ_NAME"
fi

ENTRY="$(curl -sf "$COMPUTE_URL/dlq" | jq -c --arg id "$FAIL_ID" '.entries[] | select(.invocationId==$id)')"
echo "  GET /dlq: $ENTRY"
if echo "$ENTRY" | jq -e --arg id "$FAIL_ID" 'select(.invocationId==$id) | (.payload != null) and (.lastError | length > 0) and (.attempts >= 3) and (.durationMs | type == "number")' >/dev/null; then
  log_ok "GET /dlq trae payload, lastError, attempts y durationMs"
else
  log_fail "entrada de DLQ incompleta: $ENTRY"
fi

REDRIVE="$(curl -sf -X POST "$COMPUTE_URL/dlq/$FAIL_ID/redrive")"
echo "  redrive $REDRIVE"
sleep 0.4
# WaitTimeSeconds 0 y una sola sesión: termina antes de que el job
# redriveado, que sigue fallando, vuelva a entrar en la DLQ.
STILL_BODY="$(dlq_find "$FAIL_ID" 0 30)"
if [[ -z "$STILL_BODY" ]]; then
  log_ok "el redrive sacó el mensaje de $DLQ_NAME"
else
  log_fail "el mensaje sigue en la DLQ después del redrive"
fi

BACK=""
for _ in $(seq 1 40); do
  BACK="$(curl -sf "$COMPUTE_URL/dlq" | jq -c --arg id "$FAIL_ID" '.entries[] | select(.invocationId==$id)')"
  if [[ -n "$BACK" ]]; then
    break
  fi
  sleep 0.5
done
echo "  de vuelta: ${BACK:-no}"
if echo "${BACK:-null}" | jq -e --arg id "$FAIL_ID" 'select(.invocationId==$id) and (.attempts >= 3)' >/dev/null; then
  log_ok "al seguir fallando, el redrive vuelve a $DLQ_NAME"
else
  log_fail "el redrive no volvió a la DLQ: $BACK"
fi

log_ok "LocalStack S3 + SQS/DLQ"
