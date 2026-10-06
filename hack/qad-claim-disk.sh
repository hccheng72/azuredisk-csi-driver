#!/usr/bin/env bash

# Claims one managed disk through the DiskRP ClaimResource API and prints the
# QAD blob URL and claim identifier returned by the completed operation.
#
# Usage:
#   ./hack/qad-claim-disk.sh <disk-arm-id> <owner-resource-arm-id>
# or:
#   DISK_ID=... OWNER=... ./hack/qad-claim-disk.sh
#
# Optional env:
#   API_VERSION  DiskRP api-version (default: 2025-01-02)
#   ARM_ENDPOINT ARM management endpoint (default: https://management.azure.com)
#   POLL_SECONDS seconds between async polls (default: 5)

set -euo pipefail

DISK_ID="${DISK_ID:-}"
OWNER="${OWNER:-}"
API_VERSION="${API_VERSION:-2025-01-02}"
ARM_ENDPOINT="${ARM_ENDPOINT:-https://management.azure.com}"
POLL_SECONDS="${POLL_SECONDS:-5}"

usage() {
  echo "usage: $0 <disk-arm-id> <owner-resource-arm-id>" >&2
  echo "   or: DISK_ID=... OWNER=... $0" >&2
}

if [[ $# -gt 0 ]]; then
  [[ $# -eq 2 ]] || { usage; exit 1; }
  DISK_ID="${1}"
  OWNER="${2}"
fi

if [[ -z "${DISK_ID}" || -z "${OWNER}" ]]; then
  usage
  exit 1
fi

if [[ "${DISK_ID}" != /subscriptions/*/resourceGroups/*/providers/Microsoft.Compute/disks/* ]]; then
  echo "error: disk-arm-id is not a managed disk ARM ID: ${DISK_ID}" >&2
  exit 1
fi

if [[ ! "${POLL_SECONDS}" =~ ^[0-9]+$ || "${POLL_SECONDS}" -eq 0 ]]; then
  echo "error: POLL_SECONDS must be a positive integer" >&2
  exit 1
fi

for bin in az curl jq; do
  command -v "${bin}" >/dev/null 2>&1 || {
    echo "error: '${bin}' is required" >&2
    exit 1
  }
done

HDR_FILE="$(mktemp)"
BODY_FILE="$(mktemp)"
trap 'rm -f "${HDR_FILE}" "${BODY_FILE}"' EXIT

get_token() {
  az account get-access-token \
    --resource https://management.azure.com/ \
    --query accessToken \
    -o tsv
}

poll_operation() {
  local location="${1}"
  local status_code body status token

  while :; do
    sleep "${POLL_SECONDS}"
    token="$(get_token)"
    status_code="$(curl -sS -o "${BODY_FILE}" -w '%{http_code}' \
      -H "Authorization: Bearer ${token}" \
      "${location}")"
    body="$(<"${BODY_FILE}")"

    if [[ "${status_code}" != 200 && "${status_code}" != 202 ]]; then
      echo "error: claimResource poll returned HTTP ${status_code}: ${body}" >&2
      return 1
    fi

    if [[ -z "${body//[[:space:]]/}" ]]; then
      if [[ "${status_code}" == 200 ]]; then
        echo "error: claimResource poll returned HTTP 200 with an empty body" >&2
        return 1
      fi
      continue
    fi

    if ! jq -e . "${BODY_FILE}" >/dev/null; then
      echo "error: claimResource poll returned invalid JSON: ${body}" >&2
      return 1
    fi

    status="$(jq -r '.status // empty | ascii_downcase' "${BODY_FILE}")"
    case "${status}" in
      failed|canceled|cancelled)
        echo "error: claimResource operation ${status}: ${body}" >&2
        return 1
        ;;
      succeeded)
        return 0
        ;;
      "")
        if [[ "${status_code}" == 200 ]]; then
          return 0
        fi
        ;;
    esac
  done
}

claim_url="${ARM_ENDPOINT%/}${DISK_ID}/claimResource?api-version=${API_VERSION}"
token="$(get_token)"
status_code="$(curl -sS -o "${BODY_FILE}" -D "${HDR_FILE}" -w '%{http_code}' -X POST \
  "${claim_url}" \
  -H "Authorization: Bearer ${token}" \
  -H "Content-Type: application/json" \
  -d "$(jq -cn --arg owner "${OWNER}" '{ownerResourceId: $owner}')")"

case "${status_code}" in
  200)
    ;;
  202)
    location="$(awk 'tolower($1) == "location:" {sub(/\r$/, "", $2); print $2; exit}' "${HDR_FILE}")"
    if [[ -z "${location}" ]]; then
      echo "error: claimResource returned HTTP 202 without a Location header" >&2
      exit 1
    fi
    poll_operation "${location}"
    ;;
  *)
    echo "error: claimResource returned HTTP ${status_code}: $(<"${BODY_FILE}")" >&2
    exit 1
    ;;
esac

if ! jq -e '
  (.properties.blobUrl | type == "string" and length > 0)
  and (.properties.claimIdentifier | type == "string" and length > 0)
' "${BODY_FILE}" >/dev/null; then
  echo "error: claimResource response is missing properties.blobUrl or properties.claimIdentifier: $(<"${BODY_FILE}")" >&2
  exit 1
fi

jq -r '"blobURL=\(.properties.blobUrl)\nclaimIdentifier=\(.properties.claimIdentifier)"' "${BODY_FILE}"
