#!/usr/bin/env bash

# Lists and unclaims QAD managed disks via the DiskRP UnclaimResource API.
# Each disk is verified as Unattached with managedBy set to null.
#
# Usage:
#   ./hack/qad-unclaim-disk.sh <disk-arm-id> <owner-resource-arm-id> [-f|--force]
#   ./hack/qad-unclaim-disk.sh --subscription d64ddb0c-7399-4529-a2b6-037b33265372 --resource-group MC_huichan-qad-wi-rg_huichan-qad-wi_eastus2euap \
#     [--owner <owner-resource-arm-id>] [--yes] [-f|--force]
# or for a single disk via env:
#   DISK_ID=... OWNER=... ./hack/qad-unclaim-disk.sh [-f|--force]
#
# Optional env:
#   API_VERSION   DiskRP api-version (default: 2025-01-02)
#   ARM_ENDPOINT  ARM management endpoint (default: https://management.azure.com)
#   POLL_SECONDS  seconds between async polls (default: 5)
#   VERIFY_ATTEMPTS number of disk-state verification attempts (default: 12)

set -euo pipefail

DISK_ID="${DISK_ID:-}"
OWNER="${OWNER:-}"
SUBSCRIPTION_ID="${SUBSCRIPTION_ID:-}"
RESOURCE_GROUP="${RESOURCE_GROUP:-}"
ASSUME_YES=false
FORCE_UNCLAIM=false
API_VERSION="${API_VERSION:-2025-01-02}"
ARM_ENDPOINT="${ARM_ENDPOINT:-https://management.azure.com}"
POLL_SECONDS="${POLL_SECONDS:-5}"
VERIFY_ATTEMPTS="${VERIFY_ATTEMPTS:-12}"

usage() {
  echo "usage: $0 <disk-arm-id> <owner-resource-arm-id> [-f|--force]" >&2
  echo "   or: $0 --subscription <id> --resource-group <name> [--owner <owner-resource-arm-id>] [--yes] [-f|--force]" >&2
  echo "   or: DISK_ID=... OWNER=... $0 [-f|--force]" >&2
}

if [[ $# -gt 0 && "${1}" != -* ]]; then
  [[ $# -ge 2 ]] || { usage; exit 1; }
  DISK_ID="${1}"
  OWNER="${2}"
  shift 2
fi

while [[ $# -gt 0 ]]; do
  case "${1}" in
    --subscription)
      [[ $# -ge 2 ]] || { usage; exit 1; }
      SUBSCRIPTION_ID="${2}"
      shift 2
      ;;
    --resource-group)
      [[ $# -ge 2 ]] || { usage; exit 1; }
      RESOURCE_GROUP="${2}"
      shift 2
      ;;
    --owner)
      [[ $# -ge 2 ]] || { usage; exit 1; }
      OWNER="${2}"
      shift 2
      ;;
    --yes)
      ASSUME_YES=true
      shift
      ;;
    -f|--force)
      FORCE_UNCLAIM=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "error: unknown argument '${1}'" >&2
      usage
      exit 1
      ;;
  esac
done

if [[ -n "${DISK_ID}" && ( -n "${SUBSCRIPTION_ID}" || -n "${RESOURCE_GROUP}" ) ]]; then
  echo "error: choose either single-disk mode or subscription/resource-group mode" >&2
  exit 1
fi

if [[ -n "${DISK_ID}" && -z "${OWNER}" ]]; then
  usage
  exit 1
fi

if [[ -z "${DISK_ID}" && ( -z "${SUBSCRIPTION_ID}" || -z "${RESOURCE_GROUP}" ) ]]; then
  usage
  exit 1
fi

for bin in az curl jq; do
  command -v "${bin}" >/dev/null 2>&1 || { echo "error: '${bin}' is required" >&2; exit 1; }
done

echo "Acquiring ARM access token..."
TOKEN="$(az account get-access-token --resource https://management.azure.com/ --query accessToken -o tsv)"

HDR_FILE="$(mktemp)"
BODY_FILE="$(mktemp)"
DISKS_FILE="$(mktemp)"
trap 'rm -f "${HDR_FILE}" "${BODY_FILE}" "${DISKS_FILE}"' EXIT

verify_disk() {
  local disk_id="${1}"
  local state

  for ((attempt = 1; attempt <= VERIFY_ATTEMPTS; attempt++)); do
    state="$(az disk show --ids "${disk_id}" --query '{diskState:diskState, managedBy:managedBy}' -o json)"
    if jq -e '.diskState == "Unattached" and .managedBy == null' <<<"${state}" >/dev/null; then
      echo "verified: ${state}"
      return 0
    fi
    echo "  verification ${attempt}/${VERIFY_ATTEMPTS}: ${state}"
    if ((attempt < VERIFY_ATTEMPTS)); then
      sleep "${POLL_SECONDS}"
    fi
  done

  echo "error: disk did not become Unattached with managedBy null: ${disk_id}" >&2
  return 1
}

unclaim_disk() {
  local disk_id="${1}"
  local owner="${2}"
  local unclaim_url="${ARM_ENDPOINT%/}${disk_id}/unclaimResource?api-version=${API_VERSION}"
  local status_code async_url body state

  : >"${HDR_FILE}"
  : >"${BODY_FILE}"
  echo "POST ${unclaim_url}"
  echo "  ownerResourceId: ${owner}"
  echo "  force: ${FORCE_UNCLAIM}"

  status_code="$(curl -sS -o "${BODY_FILE}" -D "${HDR_FILE}" -w '%{http_code}' -X POST \
    "${unclaim_url}" \
    -H "Authorization: Bearer ${TOKEN}" \
    -H "Content-Type: application/json" \
    -d "$(jq -cn --arg owner "${owner}" --argjson force "${FORCE_UNCLAIM}" \
      '{ownerResourceId: $owner, force: $force}')")"

  echo "HTTP ${status_code}"
  case "${status_code}" in
    200)
      echo "unclaim completed synchronously"
      ;;
    202)
      async_url="$(awk 'tolower($1)=="azure-asyncoperation:"{print $2}' "${HDR_FILE}" | tr -d '\r')"
      if [[ -z "${async_url}" ]]; then
        async_url="$(awk 'tolower($1)=="location:"{print $2}' "${HDR_FILE}" | tr -d '\r')"
      fi
      if [[ -z "${async_url}" ]]; then
        echo "error: 202 returned but no azure-asyncoperation/location header" >&2
        return 1
      fi
      echo "polling async operation..."
      while :; do
        body="$(curl -sS -H "Authorization: Bearer ${TOKEN}" "${async_url}")"
        state="$(jq -r '.status // empty' <<<"${body}")"
        echo "  status: ${state:-<none>}"
        case "${state}" in
          Succeeded) break ;;
          Failed|Canceled|Cancelled)
            echo "async operation ${state}: ${body}" >&2
            return 1
            ;;
          *) sleep "${POLL_SECONDS}" ;;
        esac
      done
      ;;
    *)
      echo "error: unclaim returned HTTP ${status_code}: $(<"${BODY_FILE}")" >&2
      return 1
      ;;
  esac

  echo "Verifying disk state..."
  verify_disk "${disk_id}"
}

if [[ -n "${DISK_ID}" ]]; then
  unclaim_disk "${DISK_ID}" "${OWNER}"
  echo "done"
  exit 0
fi

echo "Listing disks in subscription ${SUBSCRIPTION_ID}, resource group ${RESOURCE_GROUP}..."
az disk list --subscription "${SUBSCRIPTION_ID}" --resource-group "${RESOURCE_GROUP}" \
  --query '[].{id:id,name:name,diskState:diskState,managedBy:managedBy}' -o json >"${DISKS_FILE}"

if [[ -n "${OWNER}" ]]; then
  jq --arg owner "${OWNER}" '[.[] | select(.managedBy != null and (.managedBy | ascii_downcase) == ($owner | ascii_downcase))]' \
    "${DISKS_FILE}" >"${DISKS_FILE}.filtered"
else
  jq '[.[] | select(.managedBy != null)]' "${DISKS_FILE}" >"${DISKS_FILE}.filtered"
fi
mv "${DISKS_FILE}.filtered" "${DISKS_FILE}"

DISK_COUNT="$(jq 'length' "${DISKS_FILE}")"
if [[ "${DISK_COUNT}" -eq 0 ]]; then
  echo "No claimed disks were found."
  exit 0
fi

echo "Found ${DISK_COUNT} claimed disk(s):"
jq -r '.[] | "  \(.name)  state=\(.diskState)\n    disk:  \(.id)\n    owner: \(.managedBy)"' "${DISKS_FILE}"

if [[ "${ASSUME_YES}" != true ]]; then
  read -r -p "Unclaim these disks? [y/N] " answer
  [[ "${answer}" =~ ^[Yy]$ ]] || { echo "Canceled."; exit 0; }
fi

failures=()
while IFS=$'\t' read -r disk_id owner; do
  echo
  echo "Unclaiming ${disk_id}"
  if ! unclaim_disk "${disk_id}" "${owner}"; then
    failures+=("${disk_id}")
  fi
done < <(jq -r '.[] | [.id, .managedBy] | @tsv' "${DISKS_FILE}")

if ((${#failures[@]} > 0)); then
  echo >&2
  echo "Failed to unclaim or verify ${#failures[@]} disk(s):" >&2
  printf '  %s\n' "${failures[@]}" >&2
  exit 1
fi

echo
echo "Successfully unclaimed and verified ${DISK_COUNT} disk(s)."
